# The shutdown contract

The single authoritative answer to "how does a gralph run stop?". Confirmed with the user
before this harness was authored. Every fix in `fixes.json` exists to make this contract true.

## Two tiers, and nothing else

```
gralph stop <profile> [--name I]        graceful
Ctrl-C  (1st)                           graceful
    |
    +-> record the stop intent (stop file in the state dir)
        the in-flight agent session RUNS TO COMPLETION          <- no work lost
        its gate is evaluated, the cursor advances, state commits
        the loop does NOT start another iteration
        exit 0   "stopped cleanly at <cursor>"

Ctrl-C  (2nd)                           immediate
gralph stop --now                       immediate
    |
    +-> close the job object -> the whole process tree is reaped by the OS
        the cursor is already durable on disk (atomic write + fsync + rename)
        exit 130 "force-stopped at <cursor>"
```

Zombies after either tier: **zero**. That is the property `zombie-proof` measures.

## Why graceful is defined at the iteration boundary

`runLoop` already has exactly one safe point: the top of the iteration, after `resolveNext`
and before the launcher is exec'd. At that point the previous stage is fully committed and
nothing is in flight. Stopping there loses nothing by construction. Stopping anywhere else
means killing an agent mid-edit, which is what "진행중인게 유실" means.

So graceful stop is not "stop as soon as possible" — it is **"stop at the next point where
stopping is free"**. If the agent has 40 minutes left on its turn, graceful stop waits 40
minutes. That is intended; the second Ctrl-C exists for when it is not acceptable.

## What each tier must guarantee (these become gate fields)

| guarantee | field in evidence | measured how |
|---|---|---|
| graceful never truncates an in-flight session | `agent_completed:true` | the fake agent writes a completion sentinel only on its own clean exit |
| graceful commits the stage it finished | `cursor_advanced:true` | `state.json` cursor before vs after |
| graceful exits 0 | `graceful_exit_code:0` | process exit code |
| immediate leaves no survivors | `survivors:[]` with `descendants_spawned>=2` | live PID-set snapshot diff |
| immediate preserves the cursor | `cursor_preserved:true` | `state.json` parses and matches the pre-kill cursor |
| a hard `TerminateProcess` on gralph alone still reaps the tree | `survivors_after_terminate:[]` | `taskkill /F /PID <gralph>` **without** `/T` |

The `/T` omission in the last row is the whole point: `/T` would kill the tree by walking it,
which proves nothing about gralph. Killing gralph *alone* and finding the tree gone can only
be explained by the job object.

## Mutual exclusion (F4) is part of the contract

A second `gralph run` against a live instance must be **refused**, not queued and not raced:

- exit non-zero with a message naming the holder's PID,
- leave `state.json` byte-identical,
- and a lock whose holder is dead must be reaped automatically (a crashed run must not
  wedge the instance forever).

## The blocked verb (F5) is the third exit, and it is not a failure

`gralph block "<reason>"` from inside an agent session records a terminal blocked state.
The loop then exits with a distinct code instead of respawning the same command. It is
distinct from a gate failure (retryable) and from DONE (finished):

```
DONE      exit 0    the flow completed
stopped   exit 0    a human asked it to stop
blocked   exit 3    a human must act; retrying cannot help
failed    exit 1    gave up after the failure budget
```

Without this, a stage that needs a human decision burns the entire iteration budget
re-asking the same question (recorded: 10+ sessions on one cursor).
