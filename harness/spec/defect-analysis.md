# Defect analysis — gralph process lifecycle on Windows

Authoritative problem statement for this harness. Every claim below was measured, not assumed.

## The process chain

```
run-until-done.sh  ->  gralph run  ->  gralph __galp-subprocess  ->  claude -p  ->  MCP servers
     (bash)            (host)           (launcher)                   (agent)      (7..23 children)
```

Three process hops from the outer loop to the agent, plus one more to the MCP servers.

## D1 — `Signal(syscall.SIGTERM)` never works on Windows

`launcher.go:230-237` (host cancelling the launcher) and `galp_subprocess.go:103-109`
(launcher cancelling the agent) both use:

```go
cmd.Cancel = func() error {
    if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
        return cmd.Process.Kill()
    }
    return nil
}
```

Measured (go1.25.6 windows/amd64):

```
Signal(SIGTERM) on windows -> err=not supported by windows
```

So the fallback `Kill()` is taken **every time** on Windows. `Kill()` is `TerminateProcess`,
which terminates exactly one process.

## D2 — `TerminateProcess` does not touch descendants

Measured with a Go parent -> `cmd.exe` -> `ping.exe` tree, killing only the direct child:

```
killing direct child cmd.exe pid 66336
--- ping.exe after parent Kill() ---
"PING.EXE","67484","Console","1","5,772 K"      <- survived
```

The grandchild survives. There is no process-tree kill anywhere in the codebase:
`grep -rn "JobObject|SysProcAttr|Setpgid|taskkill"` over `*.go` returns **zero hits**.

## D3 — the leak, per termination path

| path | what dies | what leaks |
|---|---|---|
| agent timeout (`GALP_TIMEOUT_MS`, 75m in production profiles) | `claude.exe` only | every MCP child (measured 7-23 per session) |
| host ctx cancel | the launcher only — `TerminateProcess` runs no deferred code, so the launcher's own `cmd.Cancel` never fires | `claude.exe` **and** all its MCP children |
| console Ctrl-C | whole console group via `CTRL_C_EVENT` — this path is currently OK | nothing |
| parent session killed (Claude Code reaping a background bash) | whatever the reaper names | everything below it |

Observed live: 13 `claude.exe`, each with 7-23 children
(`mcp-server.cjs`, `npx @playwright/mcp`, `npx chrome-devtools-mcp`).

## D4 — no run-level mutual exclusion

`lock.go:17` states the design explicitly:

> Hold it only around the commit phase, never around lua execution -- gates
> may run builds or test suites and must stay parallel.

That is correct for gates, but it means **nothing serializes two `gralph run` orchestrators on
the same instance**. Combined with D1-D3 (zombie orchestrators that never die), concurrent
instances are the normal steady state, not an accident.

## D5 — recorded downstream damage (codeGrove knowledge graph)

These are recorded observations from prior runs, not predictions:

- `zombie-gralph-exe-instances-persist-across-dozen-sessions`: three `gralph.exe` zombies alive
  across 12+ consecutive sessions (PIDs 82212/68864/72180), peaking at 4 `gralph.exe` +
  27 `claude.exe` concurrently. Required a human `taskkill` to clear.
- `gralph-harness-cursor-resets-to-preflight-due-to-concurrent-instances`: `.gralph/state.json`
  cursor reverted from `integration` to `preflight` with an unrelated `session_id`, across
  20+ sessions.
- `concurrent-gralph-corruption-diagnostic-symptoms`: torn writes lost gate-evidence files
  under `artifacts/` (`integration.json`, `baseline_failures_mcp.json`); duplicate log files
  accumulated from independent instances re-running the full suite.
- `gralph-loop-has-no-blocked-signal-verb`: the command protocol cannot express "permanently
  blocked, stop respawning me", so a stage stuck on a human decision loops until the iteration
  budget is exhausted. Observed holding the `integration` cursor for 10+ sessions.

D5 is caused by D1-D4. Fixing the process lifecycle removes the root, not the symptom.

## Constraint carried over from prior runs

**Blanket-killing `gralph.exe` is forbidden.** Other harnesses (`tier-a`, `search-quality`,
`codeGrove-customer`) run their own gralph instances on this machine. Any cleanup must be
scoped to the instance/tree it owns, never `taskkill /IM gralph.exe`.

## Non-goals

- POSIX behaviour changes beyond keeping parity (the current POSIX path already works via
  process groups; do not regress it).
- Changing GALP V1 protocol semantics, outcome vocabulary, or the launcher plugin contract.
- Touching the `claude-tmux` / `ratelimit` example launchers.
