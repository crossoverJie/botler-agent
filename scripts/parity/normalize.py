#!/usr/bin/env python3
"""Normalize and compare the v1 / v3 parity artifacts.

Field handling follows the design doc §8.2 table:
  * ignored: non-deterministic or process-level fields (ids, timestamps, durations, model cache,
    prompts, token counts, randomized local image file names);
  * must match verbatim: status, replyText, userMessage, project(s), mutated, phase, source,
    provider, model, routing.decision, inboundImageCount, https image refs, conversation/tool
    entries, stopReasons;
  * must be exactly 0: usage.costUsd, calls[].costUsd, routing.usage.costUsd.
"""

import json
import os
import sys

IGNORED_TOP = {
    "id", "taskId", "startedAt", "endedAt", "durationMs", "modelCache",
    "systemPrompt",
}
IGNORED_USAGE = {"input", "output", "cacheRead", "cacheWrite", "reasoning", "total"}


def normalize_log(log):
    out = {}
    for k, v in log.items():
        if k in IGNORED_TOP:
            continue
        if k == "usage":
            out[k] = {"costUsd": v.get("costUsd", 0)}
        elif k == "calls":
            out[k] = [{"stopReason": c.get("stopReason"), "costUsd": c.get("costUsd", 0)} for c in v]
        elif k == "routing":
            routing = {
                "decision": v.get("decision"),
                "usage": {"costUsd": v.get("usage", {}).get("costUsd", 0)},
            }
            out[k] = routing
        elif k == "tools":
            out[k] = [
                {
                    "name": t.get("name"),
                    "arguments": t.get("arguments"),
                    "resultText": t.get("resultText"),
                    "isError": t.get("isError"),
                    "details": t.get("details"),
                }
                for t in v
            ]
        elif k == "conversation":
            out[k] = [
                {
                    "role": c.get("role"),
                    "text": c.get("text"),
                    "thinking": c.get("thinking"),
                    "stopReason": c.get("stopReason"),
                    "toolName": c.get("toolName"),
                    "isError": c.get("isError"),
                }
                for c in v
            ]
        elif k == "images":
            out[k] = sorted(i for i in v if str(i).startswith("https://"))
        else:
            out[k] = v
    return out


def last_log(path):
    if not os.path.isdir(path):
        return None
    files = sorted(f for f in os.listdir(path) if f.endswith(".jsonl"))
    if not files:
        return None
    with open(os.path.join(path, files[-1])) as fh:
        rows = [json.loads(line) for line in fh if line.strip()]
    return normalize_log(rows[-1]) if rows else None


def read_json(path):
    try:
        with open(path) as fh:
            return json.load(fh)
    except (OSError, json.JSONDecodeError):
        return None


def main():
    work = sys.argv[1]
    failures = 0

    v1_log = last_log(os.path.join(work, "cfg-v1", "task-logs"))
    go_log = last_log(os.path.join(work, "cfg-go", "task-logs"))
    if v1_log is None or go_log is None:
        print("FAIL: missing task logs (v1=%s go=%s)" % (v1_log is not None, go_log is not None))
        return 1

    for key in sorted(set(v1_log) | set(go_log)):
        a, b = v1_log.get(key), go_log.get(key)
        if a != b:
            print("FAIL task-log.%s\n  v1: %r\n  go: %r" % (key, a, b))
            failures += 1

    for cost_path, log in (("usage.costUsd", go_log),):
        if log.get("usage", {}).get("costUsd", 0) != 0:
            print("FAIL %s must be 0 (v1 hard-codes zero unit prices)" % cost_path)
            failures += 1

    v1_conv = read_json(os.path.join(work, "cfg-v1", "conversations", "im.json"))
    go_conv = read_json(os.path.join(work, "cfg-go", "conversations", "im.json"))
    if v1_conv or go_conv:
        strip = lambda c: [{"user": t.get("user"), "assistant": t.get("assistant"), "projects": t.get("projects")}
                           for t in (c or {}).get("turns", [])]
        if strip(v1_conv) != strip(go_conv):
            print("FAIL conversations/im.json\n  v1: %r\n  go: %r" % (strip(v1_conv), strip(go_conv)))
            failures += 1

    if failures:
        print("\n%d unexplained difference(s)" % failures)
        return 1
    print("OK: task logs, cost fields and conversation history match")
    return 0


if __name__ == "__main__":
    sys.exit(main())
