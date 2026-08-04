-- shutdown-proof: the contract in spec/shutdown-contract.md, measured on a live run.
-- zombie-proof showed nothing SURVIVES. This shows nothing is LOST: a graceful stop
-- lets the in-flight turn finish and commit, a second instance is refused instead of
-- racing the state file, and a blocked stage stops instead of respawning forever.
local L = dofile(gralph.profile_dir .. "/scripts/lib.lua")

local m = io.popen("python scripts/probe_manifest.py")
local ms = m:read("*a"); m:close()
local mism, miss = L.num(ms, "mismatched=(%d+)"), L.num(ms, "missing=(%d+)")
if mism == nil or miss == nil then gralph.fail("probe_manifest.py printed no counts"); return end
if mism ~= 0 or miss ~= 0 then
  gralph.fail("probe files modified (" .. mism .. " changed, " .. miss .. " missing) — `git checkout -- harness/`")
  return
end

local p = io.popen("python scripts/proof_shutdown.py --uut bin/gralph-uut.exe --out artifacts/shutdown_proof.json")
local s = p:read("*a"); p:close()
if not s:find("graceful_exit=", 1, true) then
  gralph.fail("proof_shutdown.py printed no summary — run it manually to see its error")
  return
end

local blob = L.slurp("artifacts/shutdown_proof.json")
if not blob then gralph.fail("artifacts/shutdown_proof.json was not written"); return end
if not L.must(blob, '"proof_source":"live-orchestrator-run"', "the verdict must come from a live run") then return end

-- ---- tier 1: graceful loses nothing -------------------------------------------------
if blob:find('"descendants_spawned":0', 1, true) then
  gralph.fail("the graceful measurement never had a live agent tree — its zeros prove nothing")
  return
end
if not L.must(blob, '"agent_completed":true',
  "graceful stop truncated the in-flight agent turn. The contract is stop-at-the-next-safe-point: the running session must finish, however long it takes. The second Ctrl-C is the escape hatch, not the first") then return end
if not L.must(blob, '"cursor_advanced":true',
  "the finished turn's gate did not commit — the cursor must reach step-two before the loop exits, or the completed work is lost on resume") then return end
if not L.must(blob, '"no_extra_turn_started":true',
  "a NEW agent turn started after the stop was requested. Graceful stop must not begin another iteration") then return end
if not L.must(blob, '"graceful_exit_code":0',
  "a graceful stop is a success, not a failure — exit 0") then return end
if not L.must(blob, '"graceful_survivors":[]', "graceful stop must also leave no survivors") then return end

-- ---- F4: one orchestrator per instance ----------------------------------------------
if not L.must(blob, '"second_instance_rejected":true',
  "a second `gralph run` on a live instance was NOT refused. Recorded consequence: concurrent instances rewound the cursor to preflight across 20+ sessions and tore evidence files in half") then return end
if not L.must(blob, '"second_names_holder_pid":true',
  "the refusal must name the PID holding the lock — 'already running' with no PID leaves the user with no next step, and blanket-killing gralph.exe is forbidden (other harnesses run on this machine)") then return end
if not L.must(blob, '"state_sha_unchanged":true',
  "the refused second instance modified state.json. A rejected run must not touch the state dir at all") then return end
if not L.must(blob, '"stale_lock_reaped":true',
  "a lock whose holder is dead was not reaped, so one crash wedges the instance forever. Reap on a dead holder PID; never on age alone") then return end

-- ---- F5: blocked is a third exit, not a retry ---------------------------------------
if not L.must(blob, '"blocked_exit_code":3',
  "`gralph block` must exit 3 (distinct from 0 done/stopped and 1 failed) so a wrapper can tell 'a human must act' from 'try again'") then return end
if not L.must(blob, '"respawn_count":0',
  "the loop respawned the stage after `gralph block`. Blocking exists precisely to stop the respawn that held one cursor for 10+ sessions") then return end
