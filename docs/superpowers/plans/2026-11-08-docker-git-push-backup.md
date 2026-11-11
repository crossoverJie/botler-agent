# Docker 部署下 git push 备份失效：根因与修复方案（r3）

日期：2026-11-08（r3 修订）
范围：`main`(v1 TypeScript) 的 `src/safety/git.ts` 链路 + `Dockerfile` + compose 挂载；v2 的 `crates/botler/src/safety/git.rs` 改动**单独在 `v2` 分支提交**
目标：容器内 `GIT_PUSH=1` 能真正把 `DATA_ROOT/*` 的改动推回 GitHub，失败可诊断、偶发失败可自愈且不自激

---

## 0. 修订记录

### r3（本轮，对照第二轮 review）

| # | 问题 | 处置 |
|---|---|---|
| 1 | §5.2 引用的 CI 事实错误：`docker-publish.yml` 只产 `edge`+sha，`latest` 只由 `docker-release.yml` 在 `v*` tag 时产出 → 合并 `main` 不会更新 `latest`，修复到不了 NAS | §5.2 重写为两条正确路径（打 tag 走 release / 或改用 `edge`），并给出 digest 校验命令 |
| 2 | §6.2「无 upstream → `push origin HEAD`」自激：不带 `-u` 只建 remote-tracking ref、不写 `branch.<name>.merge`，`@{u}` 永远失败 → 每次 dispatch 空推一次 | `pushIfPending` 在**未建立跟踪**时改用 `push -u origin HEAD`（建立而非修改 tracking），并补 detached HEAD 防护；§6.2.1 增加"无 tracking + 无改动 + 第二次 dispatch"用例 |
| 3 | §8 `safe.directory` 只加了单个仓库 | 改为 `'*'`（或循环 9 个），并说明取舍 |
| 4 | §8 "持久化" 路径写不通（宿主机没建文件、挂成 `:ro` 却让容器 `--add`） | 改为"宿主机先写好 `./config/gitconfig`，再挂 `:ro`，容器内不再写入" |
| 5 | §8 变体 B 的 compose 块缺 `GIT_AUTHOR_*` 等，照抄会退回原故障 | 变体 B 改为「在 §5.5 基础上覆盖以下项」，并列出全部保留项与 `chown 1000:1000` |
| 6 | §7 #11 的第二步若是查询消息不会触发补偿（`dispatcher.ts:206` 早退） | 明确"必须是写入型消息，或启用 §6.3" |
| 7 | §0/§6.1「任意一次 dispatch 都能补推」言过其实 | 改为「任意一次**写入型** dispatch；只读任务需 §6.3」 |
| 8 | §6.2 commit 失败时 `continue` 跳过补偿，与"解耦"目标冲突 | commit 失败不再 `continue`，补偿与 commit 结果彻底无关（实测见 §6.2.1 F） |
| 9 | §6.2 调用次数写少（实际 4 次） | 改为单次 `rev-list` 判定，实测每仓库 **3** 次本地调用 + 命中时各 1 次 commit/push，并给出 9 仓库的量级 |
| 10 | 细节：`firstLine()` 兜底未过 `firstLine`；§11 残留 pi/WeChat；`ahaead` 笔误；§6.2.1 易被误读为"代码已落地" | 已逐项修正（§6.2.1 明确标注"草稿脚本一次性验证，仓库代码未改"） |

### r2（对照第一轮 review）

| # | 问题 | 处置 |
|---|---|---|
| 1 | per-repo deploy key 不可行，文档自相矛盾 | 凭据模型改为「专用机器用户账号级 key + 9 仓库 collaborator Write」（§3） |
| 2 | 镜像发布没闭环 | §5.2 发布闭环（r3 修正了 CI 事实） |
| 3 | v2 改动在 `v2` 分支，`main` 不跟踪 `crates/` | §1、§6.4 明确 worktree 与分别提交 |
| 4 | push 与「本次 commit 成功」耦合 | §6.2 解耦为"本地判定 ahead 后单次 push" |
| 5 | 代码片段丢掉 `projects` 过滤 | §6.2 给完整替换体 |
| 6 | `GIT_PUSH` 留在 `/config/.env` 的理由反了 | §5.5 全部 `GIT_*` 进 compose `environment:` |
| 7 | 只处理 git 属主、`.gitconfig` 路径不一致 | §8 两套自洽变体 + 统一 `/config/gitconfig` |
| 8 | `-u origin HEAD` 会悄悄改 tracking | §6 改为"仅首次推送用 `-u`"（r3 修正） |
| 9 | `add -A` 与 `node_modules` 没连起来 | §9 实测各仓库 `.gitignore` |

---

## 1. 结论

不是"换个基础镜像就好"。容器化后 push 链路上有 8 个独立断点，基础镜像只占 1 个：

1. `node:20-alpine` + `apk add --no-cache git` **不含 `ssh` 客户端**（`Dockerfile:12`），而 9 个数据仓库全部是 SSH remote。
2. 镜像里没有任何 SSH 凭据（无 `/root/.ssh`、无 `known_hosts`、无 ssh config）。
3. 镜像里没有 git 身份，数据仓库也没有 repo-local 身份（`agent/cook`、`agent/ledger` 实测为空）→ `git commit` 直接失败。
4. **push 只在 commit 成功后执行**（`src/safety/git.ts:31-41`）→ 身份缺失让流程永远走不到 push。这是"备份没生效"的第一现场。
5. v1 三处 `execFileSync(..., stdio:"ignore")`（`git.ts:29,32,36`）丢弃 stderr → 日志只剩 `Command failed: git push`。
6. 无 tracking 分支时 `git push` 会 128（**条件性**：取决于 `push.default` 与仓库是否本来有 upstream）。
7. NAS bind mount 常见 `dubious ownership`（宿主 uid ≠ 容器 root），以及 OpenSSH 对 config/密钥的属主检查。
8. 容器可能出不去 22 端口。

核心判断：**凭据、身份、host key 永远不属于镜像**。解法是"镜像补二进制 + 凭据/身份以挂载与 env 注入"，不是换 base image。

镜像当前形态：`Dockerfile:44` `ENTRYPOINT ["node","bin/botler.mjs"]`，`bin/botler.mjs:9` 用 tsx 加载 `src/index.ts` —— **运行的是 v1**；`crates/botler`（v2）在 `main` 上未被 git 跟踪（`git ls-files crates` = 0，工作树仅剩空目录 `crates/botler/src/runtime/`）。

---

## 2. 证据

### 2.1 代码（`main`）

```ts
// src/safety/git.ts:11  签名（projects 用于把提交范围限定到本次真正改动的仓库）
export function commitIfChanged(msg: string, projects?: readonly string[]): void

// :25  范围过滤（必须保留）
if (projects && !projects.includes(d.name)) continue;

// :29,32,36  stderr 全部丢弃
execFileSync("git", ["add", "-A"], { cwd: dir, stdio: "ignore" });
execFileSync("git", ["status", "--porcelain"], { cwd: dir });
execFileSync("git", ["commit", "-m", msg], { cwd: dir, stdio: "ignore" });
execFileSync("git", ["push"], { cwd: dir, stdio: "ignore" });

// :31-41  push 嵌在 commit 成功分支内
// :43-46  外层 try/catch 把 commit 失败统一记成 "[git] commit failed for <name>"
```

调用点：

- `src/dispatcher.ts:247`（写入任务成功）与 `:234`（自愈成功后）传 `log.projects`。
- `src/dispatcher.ts:406` 脚本任务传单 project 数组。
- `src/dispatcher.ts:206` `if (!result.mutated) { ... return }` —— **只读任务一次都不调用**，也没有 push 补偿。

`GIT_PUSH` 读取：`src/config.ts:304` `process.env.GIT_PUSH === "1"`。
env 优先级：`src/config.ts:40` `if (key && process.env[key] === undefined)` —— `.env`（`/config/.env` 与源码目录）**只填 `process.env` 里没有的键**，容器 env 与镜像 `ENV`（`Dockerfile:28-35`）永远优先。

v2（`v2:crates/botler/src/safety/git.rs`）：push/commit 分支已用 `String::from_utf8_lossy(&output.stderr).trim()` 记录**整段** stderr（与 v1 的"首行"口径不同，见 §6.4）。

### 2.2 已发布镜像 `crossoverjie/botler-agent:latest`（与你运行的是同一个）

```
git version 2.52.0
ssh: NOT FOUND                     # apk add git 不含 openssh-client
ssh-keygen: NOT FOUND
/root/.ssh: No such file or directory
git config --global --list → fatal: unable to read config file '/root/.gitconfig'
credential.helper → (none)
github.com → 140.82.114.4          # DNS 正常，纯粹是客户端缺失
```

### 2.3 四类失败的实际报错

```bash
# (a) SSH remote —— 你当前的形态
error: cannot run ssh: No such file or directory
fatal: unable to fork
exit=128

# (b) HTTPS remote
fatal: could not read Username for 'https://github.com': No such device or address
exit=128

# (c) 容器内 commit（无任何身份）
Author identity unknown
fatal: unable to auto-detect email address (got 'root@<cid>.(none)')
exit=128          # ← 这一步失败后 push 永远不会被调用

# (d) 无 upstream
fatal: The current branch main has no upstream branch.
exit=128
```

### 2.4 数据仓库 remote 全为 SSH

```
brainstorm  car-maintenance  cook  daily-log  dream-journal
fitness  ledger  practice-english  travel
→ 全部指向同一 host：git@github.com / ssh://git@github.com/<owner>/<repo>.git
```

`git` 对 `GIT_SSH_COMMAND` 的调用只传 `[git@github.com]`（scp 式与 `ssh://` 无显式端口两种 remote 均如此，**不传 `-p`**），所以 ssh config 里的 `Port 443` + `HostName ssh.github.com` 对 9 个仓库统一生效 —— §5.4 的 443 兜底成立。

### 2.5 用户日志里的第二条问题线

```
/data/cook/web/node_modules/rollup/dist/native.js: Cannot find module '@rollup/rollup-linux-x64-musl'
WARN: web 仪表盘构建失败（npm run build 退出码 1），已跳过
WARN: index 仓库不存在：/root/Documents/dev/github/index（跳过部署）
```

`/root/Documents/dev/github/index` 是宿主机路径（容器内不存在）；`node_modules` 是宿主 glibc 装的，容器是 musl。与框架 `GIT_PUSH` 链路独立，但表现同样是"备份没生效"，见 §9。

---

## 3. 凭据模型：为什么必须是一把机器用户 key

### 3.1 排除 per-repo deploy key

GitHub 的 deploy key **绑定到单一仓库**，一把 key 加到第二个仓库会直接 `Key is already in use`。要在"同一 host"下用 9 把 deploy key，必须：

1. 在 `~/.ssh/config` 里定义 9 个 `Host` 别名（`gh-ledger` / `gh-cook` / …），各带一个 `IdentityFile`；
2. 重写 9 个仓库的 remote 为 `gh-<repo>:crossoverJie/<repo>.git`（fetch/push 两个 URL 各改一次）。

代价：9 份 host 配置 + 18 次 remote 改写 + 9 把 key 轮换，收益只是"精确到单仓库的分权"。SSH 按 `Host` 匹配 identity，**同一个 `Host github.com` 下无法按目标仓库选 key**，所以单 `IdentityFile` 与 per-repo deploy key 天然冲突。

### 3.2 采用：专用机器用户 + 账号级 key

- 新建专用 GitHub 账号（如 `crossoverjie-botler`）；
- 为该账号生成一把 ed25519 key（无 passphrase），作为**账号级 SSH key**；
- 把该账号作为 **collaborator（Write 权限）**加到 9 个仓库。

结论：单 `IdentityFile` + 单 `Host github.com` 配置自洽，撤销时移除 collaborator 即可。

### 3.3 方案对比

| 方案 | 权限边界 | 落地成本 | 网络依赖 | 结论 |
|---|---|---|---|---|
| **A. 机器用户账号级 key（本章）** | 该账号在 9 个仓库的 Write | 1 把 key、1 个 `IdentityFile`、9 次 collaborator 邀请 | 22 或 443 均可 | ✅ 主方案 |
| B. fine-grained PAT（限这 9 仓库 + `Contents: read/write`） | 与 A 相当（"PAT 爆炸半径更大"只对 classic PAT 成立） | 改 9 个 remote 为 HTTPS + `credential.helper store` + `/config/.git-credentials` | 仅 443，穿透性最好 | 🟡 备选（22 被封锁时首选） |
| C. per-repo deploy key ×9 | 最小（单仓库） | 9 把 key + 9 个 Host 别名 + 18 次 remote 改写 + 9 把轮换 | 同 A | ❌ 成本远超收益（§3.1） |
| D. 宿主机 cron 跑 push | 凭据不进容器 | 容器只 commit，宿主定时推 | 宿主负责 | 🟡 兜底，违背 `GIT_PUSH` 语义 |

判定规则：能出 22/443 → A；只能出 443 且不想依赖 `ssh.github.com` → B。

---

## 4. P0 应急（不停机，5 分钟验证诊断）

```bash
docker compose exec -u 0 botler apk add --no-cache openssh-client
docker compose exec botler sh -c 'command -v ssh && ssh -V'
```

⚠️ 该包在 `--force-recreate` 后丢失，**现象与"没修"完全一致**。它是复现/验证手段，不是修复。必须走完 §5。

---

## 5. P1 镜像与发布闭环

### 5.1 镜像补 `openssh-client`

`Dockerfile`：

```diff
-RUN apk add --no-cache git tzdata python3 ca-certificates
+# openssh-client: GIT_PUSH 走 SSH remote 时 `git push` 需要 ssh 二进制（Alpine 的 git 不拉它）。
+RUN apk add --no-cache git openssh-client tzdata python3 ca-certificates
```

同步更新文件头注释里的包清单（`git` 条目下补一行 `openssh-client  GIT_PUSH 的 SSH 传输`）。

### 5.2 发布闭环（r3 修正：`latest` 只由 tag 触发）

现有两条 workflow 的分工（`.github/workflows/`）：

| 文件 | 触发 | 产出 |
|---|---|---|
| `docker-publish.yml` | `push: branches: [main]` / 手动 | **仅** `edge` + `sha-<short>`（文件头注释：`latest` is reserved for tagged releases） |
| `docker-release.yml` | `push: tags: ['v*']` / 手动 | **`latest`** + `v0.5.1` + `0.5.1` + `0.5` |

因此 `restart: unless-stopped` + `image: ...:latest` 的组合下：**合并到 `main` 不会更新 `latest`**，`docker compose pull` 拉到的仍是旧镜像，§7 #12 会用旧镜像"通过"并给出虚假信心。

两条正确路径，选一条：

**路径 1（生产，跟随版本 tag）**

```bash
# 仓库侧：合并到 main 后打版本 tag（Makefile 的 release 目标会 bump、commit、tag、push）
make release VERSION=0.5.1

# NAS 侧
cd /path/to/botler
docker compose pull
docker compose up -d --force-recreate
docker compose exec botler sh -c 'command -v ssh && ssh -V'     # 必须打印 OpenSSH 版本
```

> compose 里把 `image:` 钉到版本 tag（`crossoverjie/botler-agent:v0.5.1`）比 `latest` 更稳：回滚就是改一行 tag。用 `latest` 也可以，但**必须完成一次 `make release`** 才有新镜像。

**路径 2（dev channel，跟随 main）**

```yaml
image: crossoverjie/botler-agent:edge     # main 合并即更新
```

生产不建议跟 `edge`；若采用，需接受"每次 main 合并即换镜像"。

**确认真的换了镜像**（`docker compose images` 只显示 Image ID，不显示 digest）：

```bash
docker compose ps -q botler | xargs docker inspect --format '{{index .RepoDigests 0}} {{.Created}}'
docker compose config --images
```

本地 build 应急（不推荐长期）：必须在**运行容器的同一台机器**上 `docker build -t crossoverjie/botler-agent:latest .` 并 `up -d --force-recreate`，否则 §4 的现象会复现。

### 5.3 生成 key（宿主机，不进镜像、不进 git）

```bash
mkdir -p ./config/ssh && chmod 700 ./config/ssh
ssh-keygen -t ed25519 -N '' -C 'botler-agent@nas' -f ./config/ssh/id_ed25519
cat ./config/ssh/id_ed25519.pub      # 加到机器用户账号 Settings → SSH and GPG keys
```

随后把该机器用户加到 9 个仓库：Settings → Collaborators → Add people（Write）。

### 5.4 预置 `known_hosts`（强制校验，禁止交互确认）

容器内 stdin 不是 tty，`StrictHostKeyChecking=ask` 必失败；`no` 又引入 MITM 风险。用预置指纹 + `yes`：

```bash
# 实际用的是 443 之上的 ssh.github.com，这一条是必须的
ssh-keyscan -p 443 ssh.github.com 2>/dev/null > ./config/ssh/known_hosts
# 22 通畅时再加（受限网络下这条会静默空输出，属正常）
ssh-keyscan -p 22 github.com 2>/dev/null >> ./config/ssh/known_hosts

# 强制检查：必须能看到 [ssh.github.com]:443 的条目
grep -c '^\[ssh.github.com\]:443' ./config/ssh/known_hosts    # 期望 >= 1
ssh-keygen -lf ./config/ssh/known_hosts                        # 指纹需与 GitHub 官方公布值一致
```

`./config/ssh/config`：

```sshconfig
Host github.com
  HostName ssh.github.com
  Port 443
  User git
  IdentityFile /root/.ssh/id_ed25519
  IdentitiesOnly yes
  BatchMode yes
  StrictHostKeyChecking yes
  UserKnownHostsFile /root/.ssh/known_hosts
  ConnectTimeout 10
  ServerAliveInterval 15
```

> 443 走 `ssh.github.com`（GitHub 官方支持）；22 通畅时可改回 `HostName github.com` / `Port 22`。
> **端口只能写在 ssh config 里，不要写进 remote URL**：设了 `GIT_SSH_COMMAND` 后 git 把 ssh 判为 `simple` variant，`ssh://git@github.com:443/...` 会直接 `ssh variant 'simple' does not support setting port`。
> `IdentitiesOnly yes` 防止把容器里可见的 key 全试一遍撞 `Too many authentication failures`；`BatchMode yes` 让缺 key/需口令时立刻失败而不是挂住；key **必须无 passphrase**。

权限：

```bash
chmod 600 ./config/ssh/id_ed25519 ./config/ssh/known_hosts ./config/ssh/config
```

### 5.5 compose（全部 `GIT_*` 统一进 `environment:`）

```yaml
services:
  botler:
    image: crossoverjie/botler-agent:v0.5.1     # 或 latest（需先 make release）
    restart: unless-stopped
    environment:
      TZ: Asia/Shanghai
      # commit 身份（git 原生读取，无需任何 gitconfig 文件）
      GIT_AUTHOR_NAME: botler-agent
      GIT_AUTHOR_EMAIL: botler@localhost
      GIT_COMMITTER_NAME: botler-agent
      GIT_COMMITTER_EMAIL: botler@localhost
      # 任何需要输入的场合立刻失败，而不是挂住
      GIT_TERMINAL_PROMPT: "0"
      GIT_SSH_COMMAND: ssh -F /root/.ssh/config
      # 推送开关：进程 env 优先于 /config/.env
      GIT_PUSH: "1"
    volumes:
      - ./config:/config
      - ./data:/data
      - ./config/ssh:/root/.ssh:ro
```

`GIT_PUSH` 的正确规则：

- `src/config.ts:40` 只在 `process.env[key] === undefined` 时才用 `.env` 的值 → **进程 env 优先级高于 `/config/.env`**；
- 镜像 `ENV`（`Dockerfile:28-35`）本身就是进程 env → 同样优先生效（`Dockerfile:28-29` 注释的含义）；
- 所以 `GIT_*` 全部放 compose `environment:`：既生效、又与镜像约定一致。写进 `/config/.env` 会被静默忽略（当前无冲突，但不要依赖"镜像里恰好没有"）。

---

## 6. P2 代码：push 解耦 + 可诊断 + 不自激（v1）

### 6.1 四个必须一起改的点

1. **解耦**：push 从 `if (status.trim())` 里搬出来 —— 任意一次**写入型** dispatch 都能补推上次失败的提交（只读任务需 §6.3）。
2. **不自激**：未建立跟踪时必须 `push -u origin HEAD`；裸 `push origin HEAD` 只建 remote-tracking ref、不写 `branch.<name>.merge`，会让 `@{u}` 永远失败 → 每轮空推。
3. **`-u` 只用于"尚未建立跟踪"**：那是建立而非修改 tracking，不存在"悄悄改"；已有 upstream 的仓库一律裸 `git push`。
4. **无损诊断**：捕获 stderr 首行（`firstLine` 也要兜住 `execFileSync` 的 `message`）。

### 6.2 完整替换体（保留签名、`Dirent` 循环、`projects` 过滤）

```ts
import { execFileSync } from "node:child_process";
import { existsSync, readdirSync, type Dirent } from "node:fs";
import { join, resolve } from "node:path";
import { CONFIG } from "../config.ts";

type GitResult = { ok: boolean; stdout: string; stderr: string };

/** git 报错的价值集中在首行（Permission denied (publickey) / Host key verification failed / no upstream）。 */
function firstLine(s: string): string {
	return s.split("\n").map((l) => l.trim()).find((l) => l.length > 0) ?? "";
}

function git(cwd: string, args: string[]): GitResult {
	try {
		const stdout = execFileSync("git", args, { cwd, stdio: ["ignore", "pipe", "pipe"] }).toString();
		return { ok: true, stdout, stderr: "" };
	} catch (e) {
		const err = e as { stdout?: Buffer | string; stderr?: Buffer | string; message?: string };
		const stderr = firstLine((err.stderr ?? "").toString());
		return { ok: false, stdout: (err.stdout ?? "").toString(), stderr: stderr || firstLine(err.message ?? "") };
	}
}

/**
 * 判定是否需要推送：`rev-list --count @{u}..HEAD` 只遍历本地对象，不产生网络往返；
 * 跟踪分支尚未建立时它直接失败，据此走一次性首次推送。
 */
function pushIfPending(dir: string, name: string): void {
	const ahead = git(dir, ["rev-list", "--count", "@{u}..HEAD"]);

	let out: GitResult | null = null;
	if (!ahead.ok) {
		// 首次推送必须带 -u：不带 -u 只写 remote-tracking ref、不写 branch.<name>.merge，
		// 会让 @{u} 永远解析失败 → 每次 dispatch 都空推一次。
		// 分离头指针下 `push origin HEAD` 会在远端建出名为 HEAD 的分支，先挡住。
		const branch = git(dir, ["symbolic-ref", "--short", "HEAD"]);
		if (!branch.ok) {
			console.error(`[git] skip push for ${name}: detached HEAD`);
			return;
		}
		out = git(dir, ["push", "-u", "origin", "HEAD"]);
	} else if (Number(ahead.stdout.trim() || "0") > 0) {
		out = git(dir, ["push"]);
	}
	if (!out) return;
	if (out.ok) console.log(`[git] pushed ${name}`);
	else console.error(`[git] push failed for ${name}: ${out.stderr}`);
}

export function commitIfChanged(msg: string, projects?: readonly string[]): void {
	if (!CONFIG.dataRoot) return;
	const root = resolve(CONFIG.dataRoot);
	if (!existsSync(root)) return;

	let entries: Dirent[];
	try {
		entries = readdirSync(root, { withFileTypes: true });
	} catch {
		return;
	}

	for (const d of entries) {
		if (!d.isDirectory() || d.name.startsWith(".")) continue;
		if (projects && !projects.includes(d.name)) continue;

		const dir = join(root, d.name);
		if (!existsSync(join(dir, ".git"))) continue;

		const added = git(dir, ["add", "-A"]);
		if (!added.ok) {
			console.error(`[git] add failed for ${d.name}: ${added.stderr}`);
			continue;
		}

		const status = git(dir, ["status", "--porcelain"]);
		if (!status.ok) {
			console.error(`[git] status failed for ${d.name}: ${status.stderr}`);
			continue;
		}

		if (status.stdout.trim()) {
			const commit = git(dir, ["commit", "-m", msg]);
			if (commit.ok) console.log(`[git] committed ${d.name}: ${msg}`);
			else console.error(`[git] commit failed for ${d.name}: ${commit.stderr}`);
		}

		// 与本次 commit 结果无关：只要有遗留待推提交就补推一次（瞬时失败即自愈）。
		if (CONFIG.gitPush) pushIfPending(dir, d.name);
	}
}
```

行为与成本（写进 PR 描述）：

- 每仓库每轮最多 **3 次本地调用**（`add -A`、`status --porcelain`、`rev-list --count`），命中时才追加 1 次 `commit` / 1 次 `push`（push 是唯一的网络往返）。9 个仓库 → 27 次进程 + 命中时的额外调用；实测单次本地调用约 1–3 ms，量级在几十毫秒，但**不要写成"几 ms"**。
- 无改动且 `ahead=0` 时不产生任何网络调用（§6.2.1 A2/A3）。
- commit 失败不再跳过补偿：上一轮遗留的待推提交仍会被推送（§6.2.1 F）。
- 首次推送用 `-u` 建立 tracking，第二次起走裸 `git push`（§6.2.1 A1→A2）。
- 分离头指针仓库跳过推送并告警，不在远端制造 `HEAD` 分支（§6.2.1 E）。

### 6.2.1 上述代码的实测结果

> **状态：一次性草稿脚本验证，仓库内 `src/safety/git.ts` 未改动**（`git status` 仅 `?? docs/superpowers/`、`?? target/`；`src/safety/git.test.ts` 仍是 `GIT_PUSH=0` 原版）。落地时按 §6.4 补测试。

环境：`tsc --noEmit` 通过；本地 `git init --bare` 远端 + `DATA_ROOT=<tmp>/data` + `GIT_PUSH=1`，并用 shim 记录每次 `git` 调用。

| # | 场景 | 观测 | 结论 |
|---|---|---|---|
| A1 | 无 tracking + 无改动（首次） | `[git] pushed a`，随后 `@{u}` = `origin/main` | ✅ 建立 tracking |
| A2 | A1 之后再跑（无改动） | **无输出、无网络调用**（仅 3 次本地调用） | ✅ 不自激（r2 的 `push origin HEAD` 在此会永远空推） |
| A3 | 再跑第三次 | 无输出 | ✅ |
| B | 有新改动 | `[git] committed a` + `[git] pushed a` | ✅ |
| C | 无改动但本地 `ahead=1`（模拟上次推送失败） | `[git] pushed a`，之后 `ahead=0` | ✅ 自愈 |
| D | `projects=["a"]` 时不传 `b`，`b` 有未提交改动 | `b` 仍为 `?? x.json` | ✅ 过滤保留 |
| E | 分离头指针仓库 | `[git] skip push for d: detached HEAD`，远端只有 `main` | ✅ 不制造 `HEAD` 分支 |
| F | commit 被 pre-commit hook 拒绝 + 存在遗留待推提交 | `[git] commit failed for c: hook says no` **且** `[git] pushed c` | ✅ 补偿与 commit 结果无关 |

### 6.3 只读任务的补偿入口（可选，P3）

`src/dispatcher.ts:206` 的 `!result.mutated` 早退意味着查询类任务完全没有补推机会。若接受"每次查询可能多几次本地 git 调用"（`log.projects` 非空时才有意义）：

```ts
// src/safety/git.ts 追加导出：只推，不做 add/commit
export function pushPending(projects?: readonly string[]): void {
	// 复用上面的循环骨架，只对每个命中仓库调用 pushIfPending(dir, name)
}
```

在 `:206` 分支内、`return` 之前调用 `pushPending(log.projects)`。约束：仍在单 worker 内执行（不新增并发写）、失败不改变任务状态。

> 不做 §6.3 的后果要写清楚：§6.2 的补偿只覆盖**写入型**任务；纯查询长期不写数据时 `[ahead N]` 会一直留着。

### 6.4 v2 侧（`v2` 分支 / worktree，单独提交）

- 推送判定与 v1 同构（`rev-list --count @{u}..HEAD`），**首次推送同样用 `-u`**；无 `-u` 的 `push origin HEAD` 在 v2 里同样是自激缺陷。
- 日志口径统一：v2 现状是整段 `String::from_utf8_lossy(&...stderr).trim()`，改为与 v1 一致的**首行**（`stderr.lines().find(|l| !l.trim().is_empty())`）。
- `main` 上 `crates/` 未被跟踪 → 落地方式：
  ```bash
  git worktree add ../botler-v2 v2     # 推荐
  # 或 git switch v2（先确保 v1 的 Dockerfile/git.ts 改动已在 main 提交完）
  ```
  v1 改动（`Dockerfile`、`src/safety/git.ts`、`src/safety/git.test.ts`）提交到 `main`；v2 改动提交到 `v2`。不要混在一个 commit 里。
- 测试改动：现有 `git.test.ts` 在模块级 `before` 里把 `GIT_PUSH` 设为 `"0"` 后动态 `import("./git.ts")`，**push 用例需要独立文件或独立 import 周期**（`CONFIG` 在 import 时求值）。远端用本地 bare 仓库，避免网络。至少覆盖（对应 §6.2.1 的表）：A1/A2 不自激、B 提交即推、C 遗留补推、D 过滤、E 分离头指针、F commit 失败仍补推。

---

## 7. 验证清单

| # | 检查（容器内） | 期望 |
|---|---|---|
| 1 | `command -v ssh && ssh -V` | 打印 OpenSSH 版本（`not found` = 仍在跑旧镜像 → 回 §5.2） |
| 2 | `ls -ln /root/.ssh` | `id_ed25519`(600)、`known_hosts`、`config` 存在；属主 **0**（变体 A）/ 与运行 uid 一致（变体 B，见 §8） |
| 3 | `git config --global --list` | 允许为空（身份来自 env） |
| 4 | `echo $GIT_AUTHOR_EMAIL; echo $GIT_PUSH` | 均非空 |
| 5 | `ssh -F /root/.ssh/config -T git@github.com` | `Hi <user>! You've successfully authenticated ...` |
| 6 | `grep -c '^\[ssh.github.com\]:443' /root/.ssh/known_hosts` | `>= 1` |
| 7 | `git -C /data/ledger ls-remote origin \| head -1` | 列出 ref（只读，安全） |
| 8 | 造一次写入任务 | 日志含 `[git] committed <repo>` + `[git] pushed <repo>` |
| 9 | `git -C /data/ledger status -sb` | 无 `[ahead N]` |
| 10 | GitHub 页面 | 出现 `agent: ...` 提交 |
| 11 | 断网 30s → 造一次**写入型**任务（或启用 §6.3）→ 恢复网络 → 再发一条**写入型**消息 | 第二条日志出现 `[git] pushed`，`[ahead 1]` 消失。**查询类消息不会触发补偿**（`dispatcher.ts:206`） |
| 12 | `docker compose pull && up -d --force-recreate` 后重跑 #1、#8 | 仍通过（证明不是靠 §4 的临时包） |
| 13 | `docker compose ps -q botler \| xargs docker inspect --format '{{index .RepoDigests 0}} {{.Created}}'` | digest/Created 是本次发布（`docker compose images` 不显示 digest） |
| 14 | 连续发两条**无数据改动**的消息后看日志 | 无 `[git] pushed` 刷屏（验证不自激） |

失败对照：

| 报错首行 | 处置 |
|---|---|
| `cannot run ssh: No such file or directory` | §5.1 未生效（旧镜像），回 §5.2 |
| `Bad owner or permissions on /root/.ssh/config` | §8 属主问题（OpenSSH 要求属主为 root 或当前用户） |
| `Permissions 0644 for '/root/.ssh/id_ed25519' are too open` | `chmod 600` |
| `Host key verification failed` | §5.4 `known_hosts` 缺 `[ssh.github.com]:443` |
| `ssh variant 'simple' does not support setting port` | remote URL 里写了端口 → 端口只放 ssh config |
| `Permission denied (publickey)` | 机器用户 key 未加到账号，或该用户不是这 9 个仓库的 collaborator |
| `Author identity unknown` | `GIT_AUTHOR_*` 未进容器 env（§5.5） |
| `could not read Username for 'https://github.com'` | remote 是 HTTPS 且无 helper → 方案 B |
| `detected dubious ownership` | §8 |

---

## 8. NAS 属主 / 权限：两套自洽变体

两个独立的检查：

- **git**：`detected dubious ownership in repository at '/data/<repo>'` — 仓库属主 ≠ 运行用户。
- **OpenSSH**：`Bad owner or permissions on /root/.ssh/config` — config/密钥属主必须等于运行用户（root 运行时要求属主为 root）；模式过宽报 `Permissions 0644 ... are too open`。

### 变体 A（推荐，保持镜像默认 root）

```bash
# 宿主机：gitconfig 必须先在这里创建好，容器只读挂载、不再写入
printf '[safe]\n\tdirectory = *\n' > ./config/gitconfig

sudo chown -R 0:0 ./config/ssh ./config/gitconfig
chmod 700 ./config/ssh && chmod 600 ./config/ssh/id_ed25519 ./config/ssh/known_hosts ./config/ssh/config
```

```yaml
environment:
  GIT_CONFIG_GLOBAL: /config/gitconfig
volumes:
  - ./config/gitconfig:/config/gitconfig:ro
```

`safe.directory = *` 覆盖 `/data` 下全部仓库（只加单个路径会让其余 8 个继续报 dubious ownership）。之所以敢用 `*`：容器只挂 `/config` 与 `/data`，`/data` 下全是本人数据仓库。

若坚持逐个声明，就在宿主机生成 9 行：

```bash
{ printf '[safe]\n'; for d in ./data/*/; do printf '\tdirectory = /data/%s\n' "$(basename "$d")"; done; } > ./config/gitconfig
```

⚠️ 不要挂 `:ro` 之后还在容器里跑 `git config --global --add`（写入会失败）；也不要挂一个宿主机不存在的文件（Docker 会建成目录）。要么按上面在宿主机先写好，要么先 `up` 再 `exec` 后 `docker cp` 出来 —— 前者才是可复现的做法。

### 变体 B（uid 对齐，无法 `chown 0:0` 时）

**在 §5.5 的基础上覆盖以下项，其余 `GIT_AUTHOR_*` / `GIT_COMMITTER_*` / `GIT_PUSH` / `GIT_TERMINAL_PROMPT` / `TZ` 全部保留**（漏掉身份就退回"commit 失败 → 永不 push"的原始故障）：

```yaml
services:
  botler:
    user: "1000:1000"
    environment:
      HOME: /home/botler                        # 关键：否则 ssh 仍找 /root/.ssh
      GIT_SSH_COMMAND: ssh -F /home/botler/.ssh/config
      GIT_CONFIG_GLOBAL: /home/botler/gitconfig
    volumes:
      - ./config:/config
      - ./data:/data
      - ./config/ssh:/home/botler/.ssh:ro
      - ./config/gitconfig:/home/botler/gitconfig:ro
```

配套（三处路径全部要换成 `/home/botler/...`）：

1. `./config/ssh/config` 里的 `IdentityFile` / `UserKnownHostsFile`；
2. §7 验证命令里的 `-F /root/.ssh/config`；
3. 宿主机属主：`chown -R 1000:1000 ./config/ssh ./config/gitconfig`，`/config` 与 `/data` 也需该 uid 可写。

代价：变体 B 下 §7 #2 的"属主为 0"不再成立（应为 1000），且 `/config`、`/data` 的宿主属主必须与 `user:` 一致，否则 botler 写不了数据。

---

## 9. 自建脚本那条线 + `add -A` 的连带风险

### 9.1 脚本假设了宿主环境

| 现象 | 原因 | 处置（二选一） |
|---|---|---|
| `index 仓库不存在：/root/Documents/dev/github/index` | 宿主机路径，容器内不存在 | 挂进来（`- ./deploy/index-repo:/repo/index:ro`）并改脚本路径；或该 deploy 步骤只在宿主机跑 |
| `Cannot find module '@rollup/rollup-linux-x64-musl'` | `/data/cook/web/node_modules` 是宿主 glibc 装的，容器 musl | 构建不在容器内做：产物由宿主/CI 产出后同步；或脚本内 `rm -rf node_modules && npm ci`（慢、需网络） |

原则：容器内脚本只做"数据写盘 + git"，不承担需要宿主工具链的构建。

### 9.2 `git add -A` 与 node_modules（已实测）

各数据仓库的 `.gitignore` 现状：

```
brainstorm web/node_modules/     car-maintenance node_modules/     cook web/node_modules/
daily-log web/node_modules/      dream-journal web/node_modules/   fitness web/node_modules/
ledger web/node_modules/         practice-english node_modules/    travel （无 node_modules 条目）
```

→ 8/9 已忽略；`travel` 无条目（目前也没检出 node_modules，但一旦有就会被 `add -A` 收进来）。

```bash
# 一次性核查（已跟踪的依赖会让 commit 体积暴涨甚至 push 失败）
for d in /data/*/; do
  n=$(git -C "$d" ls-files 2>/dev/null | grep -c 'node_modules/')
  [ "$n" -gt 0 ] && echo "WARN $d tracked node_modules entries: $n"
done
```

---

## 10. 回滚

1. compose `environment:` 里置 `GIT_PUSH: "0"` → 立刻退化为本地 commit（**不是**改 `/config/.env`，见 §5.5）。
2. 移除 `./config/ssh:/root/.ssh:ro` 与 `GIT_SSH_COMMAND` → 回到当前（无 push）状态。
3. 账号级止损：把机器用户从 9 个仓库的 collaborator 移除，并删除该账号的 SSH key。
4. 镜像回退：把 `image:` 改回上一个版本 tag（这也是把 `image:` 钉版本 tag 比 `latest` 更稳的原因）。
5. 代码回退：`git revert <sha>`（`src/safety/git.ts`），`projects` 过滤与签名保持不变，不影响历史提交。

---

## 11. 红线与不做的事

- **不把 SSH key / PAT 打进镜像**，也不写进 `DATA_ROOT`。凭据只存在于 `/config/ssh`（权限 600）。
- **不引入通用 shell 工具**；本次只改 `safety/git.ts` 的进程调用参数与日志，不新增命令执行面。
- **不把 commit/push 失败升级为任务失败**（保持"失败仅告警"）。
- **不做重试循环**：只做"下一次写入型 dispatch 的补偿性单次 push"，同一次内不重试、不退避。
- **不改 `projects` 语义**：提交范围仍由调用方传入；片段落地时必须保留该过滤。
- **不声称 v2 可用**：Docker 镜像仍是 v1（`Dockerfile:44` → `bin/botler.mjs:9` → `src/index.ts`）；v2 的 `git.rs` 改动只在 `v2` 分支。

---

## 12. 附录

### 12.1 一次性最小复现（无需碰你的 NAS）

```bash
T=$(mktemp -d) && mkdir -p $T/data/proj && cd $T/data/proj
git init -q -b main . && git config user.name t && git config user.email t@t
echo hi > a.txt && git add -A && git commit -qm init
git remote add origin ssh://git@github.com/crossoverJie/cook.git
echo x > b.txt

docker run --rm -v $T/data:/data --entrypoint sh crossoverjie/botler-agent:latest -c '
  cd /data/proj && git add -A && git commit -qm t && git push -u origin main 2>&1'
# → error: cannot run ssh: No such file or directory / fatal: unable to fork
#   （这里的 -u 只是随手写的，失败发生在建立连接之前，与 §6 的 -u 讨论无关）

docker run --rm -v $T/data:/data --entrypoint sh crossoverjie/botler-agent:latest -c '
  cd /data/proj && git config --unset user.name && git commit --allow-empty -m t 2>&1'
# → Author identity unknown / fatal: unable to auto-detect email address
```

### 12.2 无 tracking 时的自激（r3 断点 #2 的最小复现）

```bash
T=$(mktemp -d); git init -q --bare $T/remote.git; mkdir -p $T/r; cd $T/r
git init -q -b main; git config user.name t; git config user.email t@t
git remote add origin $T/remote.git; echo a > f; git add -A; git commit -qm init

git rev-parse --abbrev-ref --symbolic-full-name '@{u}'   # fatal: no upstream configured
git push origin HEAD                                     # * [new branch] HEAD -> main（成功）
git config --get branch.main.merge                       # (unset) ← 关键：tracking 没建立
git rev-parse --abbrev-ref --symbolic-full-name '@{u}'   # 仍然 fatal
git push origin HEAD                                     # Everything up-to-date → 每轮都会走一次

git push -u origin HEAD                                  # branch 'main' set up to track 'origin/main'
git rev-parse --abbrev-ref --symbolic-full-name '@{u}'   # origin/main
git rev-list --count '@{u}..HEAD'                        # 0 → 后续不再推送
```

### 12.3 被否决的 per-repo deploy key 形态（留档，勿实施）

```sshconfig
Host gh-ledger
  HostName ssh.github.com
  Port 443
  User git
  IdentityFile /root/.ssh/id_ed25519_ledger
  IdentitiesOnly yes
Host gh-cook
  HostName ssh.github.com
  Port 443
  User git
  IdentityFile /root/.ssh/id_ed25519_cook
  IdentitiesOnly yes
# ... 9 段
```

配套：`git -C /data/ledger remote set-url origin gh-ledger:crossoverJie/ledger.git`（fetch/push 各一次），并在 9 个仓库分别添加对应 deploy key。仅当确实需要"单仓库最小权限"时才值得付出这个成本。
