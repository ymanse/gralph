# PRD — gralph process lifecycle on Windows

**Repo** `D:/dev_ext/gralph` · fork `ymanse/gralph` (upstream `gralph-loop/gralph`, READ-only)
**Branch** `feature/windows-lifecycle`
**Goal** A gralph run can always be stopped through a defined channel, that channel never
loses in-flight work, and no path leaves a surviving process behind.

## Authoritative inputs

| document | what it fixes |
|---|---|
| `spec/defect-analysis.md` | the measured defect (D1-D5) and the non-goals |
| `spec/shutdown-contract.md` | the shutdown design, already decided — do not redesign it |

Both were written from measurements on this machine, not from reading the code. Where a
guidance block and a spec disagree, the spec wins; say so rather than guessing.

## The five fixes

| id | what | done when |
|---|---|---|
| `F1-jobobject` | `run` assigns the current process to a Windows job object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`; descendants inherit it | killing gralph alone (`taskkill /F /PID`, no `/T`) leaves no descendants |
| `F2-treekill` | both `cmd.Cancel` bodies (`launcher.go`, `galp_subprocess.go`) kill the process **tree**, not one process; POSIX keeps its process-group behaviour | `agent_timeout` and host-cancel paths leave no descendants; `scan_lifecycle.py` reports `legacy_cancel_sites=0` |
| `F3-graceful-stop` | `gralph stop [--now]` + stop file + 1st SIGINT graceful / 2nd immediate; graceful checked at the top of the iteration | an in-flight turn completes, its gate commits, the cursor advances, exit 0, next stage never starts |
| `F4-run-lock` | run-level exclusive lock holding the orchestrator PID, with dead-holder reaping | a second `gralph run` is refused, names the holder PID, leaves `state.json` byte-identical; a crashed holder's lock is reclaimed |
| `F5-block-verb` | `gralph block "<reason>"` → terminal blocked state, loop exits 3 | the stage is not respawned |

F1+F2 close the leak. F3 defines the channel. F4+F5 close the two failure modes the leak
caused downstream (concurrent state corruption, infinite respawn) — both recorded across
20+ prior sessions in `spec/defect-analysis.md` §D5.

## Exit codes (the contract)

```
0   DONE            the flow completed
0   stopped         a human asked it to stop (graceful)
130 force-stopped   second Ctrl-C / `stop --now`
3   blocked         a human must act; retrying cannot help
1   failed          gave up after the failure budget
```

## Hard constraints

- **Never rebuild `bin/gralph-orch.exe`.** It is the running orchestrator (Windows locks a
  running executable) and the probes' pre-fix positive control. Build `bin/gralph-uut.exe`.
- **Never edit a HARNESS-OWNED file.** They are listed in `probe_manifest.json` and every
  gate re-hashes them. If one is wrong, stop and say so.
- **Never blanket-kill `gralph.exe`.** Other harnesses run on this machine. Scope any
  cleanup to the instance you own.
- **Do not change GALP V1** protocol semantics, the outcome vocabulary, or the launcher
  plugin contract. The example launchers (`claude-tmux`, `ratelimit`) are out of scope.
- **Keep POSIX working.** The package must still build for linux and darwin, and the POSIX
  cancel path must keep signalling the process group.

## Out of scope

Performance work, refactoring unrelated packages, new features, and anything that widens
the diff beyond the five fixes. A smaller diff is easier to send upstream as a PR.
