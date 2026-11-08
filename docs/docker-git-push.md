# Git push from the container (`GIT_PUSH=1`)

`GIT_PUSH=1` makes the dispatcher push every data subproject that still has unpushed commits, once per
write task — even when that task committed nothing new (so a push that failed earlier self-heals), and
even when the repo never had an upstream (a one-time `push -u` sets tracking, but only if an `origin`
remote exists and the branch has a commit).
Inside a container the push path has several independent failure points that have nothing to do with the
application code, so the **image** ships the binary (`openssh-client`) and everything sensitive is
**mounted / injected**, never baked in:

| Concern | Where it lives | Never |
| --- | --- | --- |
| `ssh` binary | image (`Dockerfile`: `apk add openssh-client`) | — |
| SSH key + `known_hosts` + ssh config | host `./config/ssh`, mounted read-only | baked into the image |
| commit identity | `GIT_AUTHOR_*` / `GIT_COMMITTER_*` in the container env | a committed `.gitconfig` |
| push switch | `GIT_PUSH` in the container env | relying on `/config/.env` |

`GIT_PUSH` in `/config/.env` is silently ignored when the value is also present in the container env:
`src/config.ts` only fills keys that are not already in `process.env`, and the image `ENV` defaults are
process env. Put every `GIT_*` setting in the container env.

## 1. Publishing the image

`latest` is produced **only by a `v*` tag** (`.github/workflows/docker-release.yml`); merging to `main`
publishes `edge` + `sha-<short>` (`.github/workflows/docker-publish.yml`). A NAS running
`image: ...:latest` therefore keeps the old image — and the old image has no `ssh` — until a release tag
is pushed:

```bash
# repo side
make release VERSION=0.5.1

# NAS side
docker compose pull
docker compose up -d --force-recreate
docker compose exec botler sh -c 'command -v ssh && ssh -V'   # must print an OpenSSH version
```

Pin the version tag (`crossoverjie/botler-agent:v0.5.1`) rather than `latest`: rolling back is then a
one-line change. `docker compose images` only shows image IDs, so confirm the digest:

```bash
docker compose ps -q botler | xargs docker inspect --format '{{index .RepoDigests 0}} {{.Created}}'
```

## 2. Credentials: one machine-user key, not per-repo deploy keys

GitHub deploy keys are bound to a single repository (`Key is already in use` on the second one). Nine
data repos would need nine keys, nine `Host` aliases and 18 `remote set-url` calls, while SSH picks
identities by `Host` — it cannot select a key per target repository. Use instead:

1. A dedicated machine user (e.g. `crossoverjie-botler`) with one ed25519 key without a passphrase.
2. That user added as a **collaborator with Write** on each data repo.

Revoking access = remove the collaborator + delete the key.

```bash
mkdir -p ./config/ssh && chmod 700 ./config/ssh
ssh-keygen -t ed25519 -N '' -C 'botler-agent@nas' -f ./config/ssh/id_ed25519
cat ./config/ssh/id_ed25519.pub        # add to the machine user: Settings -> SSH and GPG keys
```

A fine-grained PAT (9 repos, `Contents: read/write`, HTTPS remotes + `credential.helper store`) is the
fallback when outbound port 22 and 443-over-SSH are both blocked.

## 3. `known_hosts` and ssh config

The container has no tty, so `StrictHostKeyChecking=ask` always fails and `=no` gives up MITM
protection. Pre-seed fingerprints and keep strict checking on:

```bash
ssh-keyscan -p 443 ssh.github.com 2>/dev/null > ./config/ssh/known_hosts
ssh-keyscan -p 22 github.com 2>/dev/null >> ./config/ssh/known_hosts   # fails silently if 22 is blocked

grep -c '^\[ssh.github.com\]:443' ./config/ssh/known_hosts              # expect >= 1
ssh-keygen -lf ./config/ssh/known_hosts                                 # compare with GitHub's published values
```

`./config/ssh/config`:

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

- Port **only** in the ssh config, never in the remote URL: with `GIT_SSH_COMMAND` set, git uses the
  `simple` ssh variant and `ssh://git@github.com:443/...` fails with
  `ssh variant 'simple' does not support setting port`.
- `IdentitiesOnly yes` avoids `Too many authentication failures`; `BatchMode yes` fails fast instead of
  hanging on a passphrase prompt; the key must have no passphrase.
- Port 22 works too: set `HostName github.com` / `Port 22`.

```bash
chmod 600 ./config/ssh/id_ed25519 ./config/ssh/known_hosts ./config/ssh/config
```

## 4. Compose

```yaml
services:
  botler:
    image: crossoverjie/botler-agent:v0.5.1
    restart: unless-stopped
    environment:
      TZ: Asia/Shanghai
      # commit identity: read natively by git, no gitconfig file needed
      GIT_AUTHOR_NAME: botler-agent
      GIT_AUTHOR_EMAIL: botler@localhost
      GIT_COMMITTER_NAME: botler-agent
      GIT_COMMITTER_EMAIL: botler@localhost
      # fail immediately instead of waiting for input
      GIT_TERMINAL_PROMPT: "0"
      GIT_SSH_COMMAND: ssh -F /root/.ssh/config
      GIT_PUSH: "1"
      # only needed for the ownership variant below
      GIT_CONFIG_GLOBAL: /config/gitconfig
    volumes:
      - ./config:/config
      - ./data:/data
      - ./config/ssh:/root/.ssh:ro
      - ./config/gitconfig:/config/gitconfig:ro
```

## 5. NAS ownership / permissions

Two independent checks:

- **git**: `detected dubious ownership in repository at '/data/<repo>'` — repo owner ≠ running user.
- **OpenSSH**: `Bad owner or permissions on /root/.ssh/config` — the config and key must be owned by the
  running user (root when running as root); overly wide modes give
  `Permissions 0644 for ... are too open`.

### Variant A — keep the image's default root (recommended)

```bash
# create the gitconfig on the HOST first; the container mounts it read-only and never writes it
printf '[safe]\n\tdirectory = *\n' > ./config/gitconfig

sudo chown -R 0:0 ./config/ssh ./config/gitconfig
chmod 700 ./config/ssh && chmod 600 ./config/ssh/id_ed25519 ./config/ssh/known_hosts ./config/ssh/config
```

`safe.directory = *` covers every repo under `/data` (whitelisting one path leaves the other eight
failing). It is acceptable here because the container only mounts `/config` and `/data`, and `/data`
holds your own data repos. To enumerate instead:

```bash
{ printf '[safe]\n'; for d in ./data/*/; do printf '\tdirectory = /data/%s\n' "$(basename "$d")"; done; } > ./config/gitconfig
```

Do not mount the gitconfig `:ro` and then run `git config --global --add` inside the container (the
write fails), and do not mount a host path that does not exist yet (Docker creates it as a directory).

### Variant B — align the uid (when `chown 0:0` is not possible)

Override the items below on top of the section 4 block; keep `GIT_AUTHOR_*`, `GIT_COMMITTER_*`,
`GIT_PUSH` and `GIT_TERMINAL_PROMPT` (dropping the identity brings back "commit fails → push never runs"):

```yaml
services:
  botler:
    user: "1000:1000"
    environment:
      HOME: /home/botler                        # otherwise ssh still looks in /root/.ssh
      GIT_SSH_COMMAND: ssh -F /home/botler/.ssh/config
      GIT_CONFIG_GLOBAL: /home/botler/gitconfig
    volumes:
      - ./config:/config
      - ./data:/data
      - ./config/ssh:/home/botler/.ssh:ro
      - ./config/gitconfig:/home/botler/gitconfig:ro
```

Also update `./config/ssh/config` (`IdentityFile`, `UserKnownHostsFile`), the verification commands, and
`chown -R 1000:1000 ./config/ssh ./config/gitconfig`; `/config` and `/data` must be writable by 1000 too.

## 6. Verification checklist

| # | Check (inside the container) | Expected |
| --- | --- | --- |
| 1 | `command -v ssh && ssh -V` | OpenSSH version (`not found` = still the old image, see section 1) |
| 2 | `ls -ln /root/.ssh` | `id_ed25519` (600), `known_hosts`, `config`; owner 0 (variant A) / the runtime uid (variant B) |
| 3 | `git config --global --list` | the mounted `/config/gitconfig` contents only (`[safe] directory = *` when following section 5); commit identity comes from env, not from this file |
| 4 | `echo $GIT_AUTHOR_EMAIL; echo $GIT_PUSH` | both non-empty |
| 5 | `ssh -F /root/.ssh/config -T git@github.com` | `Hi <user>! You've successfully authenticated ...` |
| 6 | `grep -c '^\[ssh.github.com\]:443' /root/.ssh/known_hosts` | `>= 1` |
| 7 | `git -C /data/<repo> ls-remote origin \| head -1` | lists a ref (read-only) |
| 8 | run a write task | log shows `[git] committed <repo>` + `[git] pushed <repo>` |
| 9 | `git -C /data/<repo> status -sb` | no `[ahead N]` |
| 10 | GitHub | an `agent: ...` commit appears |
| 11 | cut the network 30s → run a **write** task → restore → run another **write** task | second one logs `[git] pushed` and `[ahead 1]` disappears (read-only tasks do not trigger the compensation push) |
| 12 | `docker compose pull && up -d --force-recreate`, re-run #1 and #8 | still pass (proves it is not a temporary `apk add`) |
| 13 | `docker compose ps -q botler \| xargs docker inspect --format '{{index .RepoDigests 0}} {{.Created}}'` | the digest/Created matches this release |
| 14 | two consecutive **write** tasks that change no data (a read-only task never calls the commit/push path) | no `[git] pushed` line at all |

Troubleshooting by first stderr line:

| First line | Fix |
| --- | --- |
| `cannot run ssh: No such file or directory` | old image (no `openssh-client`) — see section 1 |
| `Bad owner or permissions on /root/.ssh/config` | section 5 ownership |
| `Permissions 0644 for '/root/.ssh/id_ed25519' are too open` | `chmod 600` |
| `Host key verification failed` | `known_hosts` has no `[ssh.github.com]:443` entry — section 3 |
| `ssh variant 'simple' does not support setting port` | port in the remote URL — move it to the ssh config |
| `Permission denied (publickey)` | machine-user key not on the account, or not a collaborator on that repo |
| `Author identity unknown` | `GIT_AUTHOR_*` missing from the container env — section 4 |
| `could not read Username for 'https://github.com'` | HTTPS remote without a credential helper — use a fine-grained PAT |
| `detected dubious ownership` | section 5 |

## 7. Behaviour of the commit/push loop

`src/safety/git.ts` (`commitIfChanged`):

- per repo and per round: `git add -A`, `git status --porcelain`, and `rev-list --count @{u}..HEAD`
  (local objects only) → 3 local invocations; `commit` / `push` only when needed. `rev-list` is skipped
  when `GIT_PUSH` is off, so a plain local setup is unaffected. A repo without a resolvable `@{u}` costs
  a few extra local lookups (`symbolic-ref`, `remote get-url`, `config --get`, `rev-parse`) but no network
  call, and each skip cause is logged only once per process.
- the push is **decoupled from this round's commit**: any repo with pending commits is pushed, so a push
  that failed earlier (network blip, expired key) self-heals on the next write task.
- the **first** push for a repo uses `push -u origin HEAD` (sets up tracking); afterwards a bare
  `git push`. Without `-u`, `@{u}` would never resolve and every round would push once for nothing.
- `rev-list` failing is **not** treated as "no upstream": `branch.<name>.merge` is checked first, so a
  configured-but-gone tracking ref (remote branch renamed/deleted, `git remote prune`) is reported and
  skipped instead of having its tracking rewritten by `-u`.
- the following are skipped with one warning per process (no network call, no repeated log lines):
  detached HEAD, no `origin` remote, unborn branch (no commit yet), unresolvable upstream.
- no changes and nothing to push → no network call at all.
- failures are logged with the first stderr line (`Permission denied (publickey)`, ...) and only warn;
  they never turn the task into an error. There is no retry loop inside one round.
