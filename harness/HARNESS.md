# gralph lifecycle harness

An autonomous build harness that fixes **gralph itself**: it eliminates the Windows
process-tree leak and gives the loop a defined shutdown channel. Nine stages, each
guarded by a Lua gate that recomputes its own verdict rather than reading the agent's.

```
preflight -> decompose
  -> write-tests -(fixes remain)-> self -(all 5 RED)-> implement
  -> implement   -(fixes remain)-> self -(all 5 GREEN)-> lint
  -> integration -> zombie-proof -> shutdown-proof -> dep-verify -> DONE
```

- **Repo** `D:/dev_ext/gralph` · fork `ymanse/gralph` · branch `feature/windows-lifecycle`
- **Spec** `spec/defect-analysis.md` (the measured defect) + `spec/shutdown-contract.md`
  (the decided design) + `PRD.md` (the five fixes and the constraints)
- **Run** `cd harness && ./run-until-done.sh`
- **Cursor** `./bin/gralph-orch.exe status --profile lifecycle.yaml`

## The one thing to understand first: two binaries

| file | what it is | who may rebuild it |
|---|---|---|
| `bin/gralph-orch.exe` | the **pinned pre-fix** orchestrator that RUNS this harness, and the probes' positive control | nobody — ever |
| `bin/gralph-uut.exe` | the binary under test, rebuilt from the working tree | the agent, every stage |

Two independent reasons, both load-bearing:

1. Windows locks a running executable. A loop that executed the file it was rebuilding
   would deadlock on its own first `go build`.
2. A probe that reports "no survivors" is only meaningful if it *can* report survivors.
   `gralph-orch.exe` still has the defect, so every proof run re-demonstrates the leak
   on it. If the control ever stops leaking, the probe broke — not gralph.

`run-until-done.sh` uses `./bin/gralph-orch.exe` explicitly, never `gralph` from PATH.

## What each gate proves

| # | stage | evidence | the decisive check |
|---|---|---|---|
| 1 | `preflight` | `artifacts/preflight.json` | referee hashes match; the pre-fix **control baseline leaked**; `go test`/`gofmt`/`go vet` green at HEAD (all recomputed in-gate) |
| 2 | `decompose` | `artifacts/fixes.json` | all five spec ids present by name; each declares a `test_pattern` |
| 3 | `write-tests` ↻ | `artifacts/tests_red/<id>.json` | gate re-runs `go test -run <pattern>` and requires a **real failure**; `tests_hash` recomputed from disk |
| 4 | `implement` ↻ | `artifacts/impl/<id>.json` | test bytes must equal the hash **the write-tests gate stored** (not one the agent restates); selection must now pass |
| 5 | `lint` | `artifacts/lint.json` | `scan_lifecycle.py` run in-gate: 0 legacy cancel sites, **both** `cmd.Cancel` sites route through the tree-killer, a job object exists, a `!windows` counterpart exists |
| 6 | `integration` | `artifacts/integration.json` | full suite green **and** passing count ≥ the preflight baseline (green-by-deletion rejected) |
| 7 | `zombie-proof` | `artifacts/zombie_proof.json` | **the gate runs the probe**: 4 termination paths, `survivors:[]`, `descendants_spawned_min≥2`, and the control still leaks |
| 8 | `shutdown-proof` | `artifacts/shutdown_proof.json` | **the gate runs the probe**: graceful loses nothing, second instance refused, blocked exits 3 without respawning |
| 9 | `dep-verify` | `artifacts/dep_report.json` | linux+darwin still build; `go.mod` delta against `upstream/main` inside the allowlist |

Stages 7 and 8 generate their own evidence. There is no agent-authored input to them at
all — the agent's only influence is the Go code it wrote.

## Why the probes are hash-pinned

The gates judge the agent; the probes are the gates' instruments. An agent that could
edit `proof_zombie.py` could pass every gate without fixing anything. So the **24** files
in `probe_manifest.json` are re-hashed by `preflight`, `integration`, `zombie-proof` and
`shutdown-proof`, and any change is a hard fail.

The manifest covers the **gates themselves** (`lifecycle.yaml`, every `scripts/*.lua`) as
well as the probes. A gate obviously cannot protect its own bytes — an edited gate could
just delete the check. What this buys is **cross-protection**: four separate gates verify
the manifest, so weakening any one of them is caught by the other three.

**Proven:** appending one comment line to `scripts/proc.py` produced
`HARNESS-OWNED probe files were modified (1 changed, 0 missing)`.

If a probe genuinely needs changing, that is a harness-author decision: stop the loop,
change it deliberately, re-run `python scripts/probe_manifest.py --write`.

## Measured baseline (the defect, before any fix)

`artifacts/baseline/zombie_prefix_baseline.json`, produced by the same probe the gate
uses, against the pinned pre-fix binary:

| termination path | descendants spawned | **survivors** |
|---|---|---|
| `agent_timeout` | 3 | **2** — the launcher kills the agent; its children are orphaned |
| `sigint_double` | 3 | **2** — children that ignore console events survive |
| `terminate_gralph` | 3 | **3** — the whole tree orphaned; nothing but a job object can help here |
| `graceful_stop` | — | unsupported (no `stop` verb pre-fix) |

7 leaked processes per probe run. In production each of those is a `claude.exe` with
7–23 MCP children behind it.

## Gate proving (law 6) — what was verified before the first run

Every gate was fed known-good and known-bad evidence via `gralph try`. Each cheat is
rejected with its **own** prescriptive reason (a single generic rejection would mean one
of the checks is dead code).

| gate | accepted | rejected |
|---|---|---|
| `preflight` | real green baseline | a modified referee file |
| `decompose` | all five ids | silently dropping `F5-block-verb` |
| `write-tests` | real RED, hash from disk — **`route: implement` fired** | `failed:0`; no negative path; hand-written `tests_hash`; a pattern selecting nothing (`go test` prints `ok [no tests to run]`); missing evidence |
| `implement` | real GREEN — **both `route: implement` and `route: lint` fired** | weakening the test instead of the code; `skipped>0`; no `tests_baseline`; a structural claim with no scanner; skipping `write-tests` entirely |
| `lint` | — (see below) | the pre-fix tree, *even when the evidence file claims `legacy_cancel_sites:0`* — the gate runs the scanner itself |
| `integration` | suite green with a real baseline | passing count below the baseline; missing `assertions_preserved` |
| `dep-verify` | real cross-build + dep delta | claiming a `cg-self` index that does not exist |
| `zombie-proof` | — (see below) | the unfixed binary: **7 survivors**, naming F1/F2 per path |

**`gralph.route()` coverage (law 9).** Only `write-tests` and `implement` have two
successors, and both had their routing tails exercised on **passing** evidence — the only
condition under which a missing route surfaces. The `implement` proof seeded
`.gralph/lifecycle/store.json` for both branches (`green_done=0` → self, `green_done=4` →
`lint`) and the seed was deleted afterwards.

**Not proven pre-first-run, and why.** `lint`, `zombie-proof` and `shutdown-proof` cannot
pass until the fix exists — their PASS direction is first exercised on the live run.
All three are single-successor nodes, so no routing tail is hiding behind them. Their
FAIL direction is proven, which is the direction that matters for a broken referee.

**Three real bugs the proving pass caught**, all before any agent ran:

- **The manifest was stale** (10 files pinned, more required). The gate fail-closed on
  the count rather than silently checking fewer files.
- **`gofmt -l` reported all 29 files** because `core.autocrlf=true` checked the repo out
  as CRLF. Left alone, the `lint` gate would have been *impossible to satisfy* — the
  broken-referee failure mode, inside my own gate. The repo is now normalized to LF
  (`core.autocrlf=false`).
- **`zombie-proof` checked path coverage before survivors.** Against a pre-fix binary
  `graceful_stop` is unsupported, so coverage was always 3/4 and failed first — which
  meant the harness's single most important check, `survivors == 0`, was never reached
  and therefore never tested. Reordered to survivors-first; it now fails with
  `7 descendant process(es) survived` and names which fix each path implicates. The
  general lesson: **a check that some earlier check always shadows is untested code**,
  and ordering decides which one that is.

## Loop policy

Stop hierarchy: **gate-pass** > **self-loop progress** (`fix_done` / `green_done` in the
store) > **`fail_threshold`** session rotation > **`--max-iterations`** hard seatbelt.

`fail_threshold` is 2 for the mechanical nodes (`preflight`, `decompose`, `lint`,
`dep-verify`) and 3 for the authoring and proof nodes. `lua_timeout` is 1800s on
`zombie-proof`, `shutdown-proof` and `dep-verify` because those gates run real
orchestrators and cross-compiles inside themselves.

**Usage limits are absorbed outside gralph.** When the Claude CLI hits its limit,
`claude -p` exits 1 and gralph reads a plain crash; after 5 abnormal exits without cursor
progress it gives up. That give-up is a **safe stop** — state and store are preserved —
so `run-until-done.sh` just notices why it stopped and resumes. Do **not** reach for the
GALP `launcher:` on Windows: the argv round-trip strips `{{prompt}}`'s braces, so the
launcher hands the agent the bare word `prompt` and no gate ever advances.

**Cursor rewind.** `.gralph/lifecycle/state.json` is `{"cursor","session_id","failures"}`.
Editing `cursor` re-runs only the tail stages — useful after hardening a gate, without a
full rebuild. `gralph try` is a dry-run: it neither commits store writes nor advances the
cursor, so it is safe against live state.

## Completion alarm

`run-until-done.sh` calls `notify()` at its `DONE` / `STUCK` / `TIMEOUT` exits (terminal
bell + Windows toast, `BurntToast` → `msg *` → bell). The loop is a separate process and
gralph only prints `cursor is DONE` to stderr, so nothing else surfaces the finish. To be
re-invoked on exit instead, run it under Claude Code with `run_in_background: true`.

## Known hazards for the build agent

- **Job nesting.** Once F1 lands, `gralph run` assigns itself to a job object. The probes
  start *nested* orchestrators; Windows 8+ allows nested jobs, so this works — but the
  fix must not request `CREATE_BREAKAWAY_FROM_JOB`, and a failed assignment must be
  logged and **non-fatal** (fail open: never make `run` unusable because a job could not
  be created).
- **The harness's own orchestrator is pre-fix**, so no outer job object exists while the
  build runs. That is intentional and keeps the probe measurements clean.
- **Never blanket-kill `gralph.exe`.** Other harnesses (`tier-a`, `search-quality`,
  `codeGrove-customer`) run their own instances on this machine. `proc.kill_tree` is
  always scoped to a PID the probe itself started.
- **`withStateLock` is not the run lock.** It guards the commit phase and deliberately
  does not span gate execution (`lock.go:17`). F4 adds a separate run-level lock; do not
  widen `withStateLock` instead — that would serialize test suites across parallel gates.

## Calibration notes for the next harness in this chain

- Real baseline suite size: **125 passing tests** at branch point. `integration` binds to
  the count `preflight` records at run time, so this number never needs updating by hand.
- The probe cycle costs ~5 minutes per full run (7 orchestrator launches). Budget for
  `zombie-proof` and `shutdown-proof` to consume most of a session each.
- If a future harness extends this one, its stage 0 should re-verify this harness's DONE
  evidence with a live spot-check — re-run `proof_zombie.py` rather than trusting
  `artifacts/zombie_proof.json` on disk.
