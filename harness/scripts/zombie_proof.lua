-- zombie-proof: THE gate. Everything else is a proxy for this one.
--
-- The gate runs the probe itself. There is no agent-authored evidence here at all:
-- proof_zombie.py launches a real orchestrator against an agent that owns real
-- children, terminates it by each path in the shutdown contract, and reports which
-- PIDs are still alive. The agent's only influence is the Go code it wrote.
--
-- The positive control is what makes the zero meaningful. The same probe, same run,
-- also drives the pinned PRE-FIX binary, which must still LEAK. A probe that reports
-- "no survivors" for both binaries is broken, and a broken probe is exactly how a
-- hollow zero gets mistaken for a fix (law 4).
local L = dofile(gralph.profile_dir .. "/scripts/lib.lua")

-- referees first: this gate's whole value is that its script was not edited.
local m = io.popen("python scripts/probe_manifest.py")
local ms = m:read("*a"); m:close()
local mism, miss = L.num(ms, "mismatched=(%d+)"), L.num(ms, "missing=(%d+)")
if mism == nil or miss == nil then gralph.fail("probe_manifest.py printed no counts"); return end
if mism ~= 0 or miss ~= 0 then
  gralph.fail("the probe files were modified (" .. mism .. " changed, " .. miss ..
              " missing). This gate cannot judge a fix using a referee the fix's author edited — `git checkout -- harness/`")
  return
end

-- run the probe (several minutes: it starts and kills real orchestrators)
local p = io.popen("python scripts/proof_zombie.py --uut bin/gralph-uut.exe --control bin/gralph-orch.exe --out artifacts/zombie_proof.json")
local s = p:read("*a"); p:close()
local paths    = L.num(s, "uut_paths=(%d+)")
local spawned  = L.num(s, "spawned_min=(%d+)")
local surv     = L.num(s, "survivors=(%d+)")
local ctlsurv  = L.num(s, "control_survivors=(%d+)")
if paths == nil or spawned == nil or surv == nil or ctlsurv == nil then
  gralph.fail("proof_zombie.py printed no summary. Is bin/gralph-uut.exe built? Rebuild with `python scripts/check_go.py --build bin/gralph-uut.exe` and run the probe manually to see its error")
  return
end

-- law 1: bind the zero to a positive count. No tree, no proof.
if spawned < 2 then
  gralph.fail("the fake agent never brought up a tree (spawned_min=" .. spawned ..
              "). `survivors=0` here would mean 'nothing ran', not 'nothing leaked'")
  return
end

-- law 4/5: the referee must be able to see the defect it is looking for.
if ctlsurv < 1 then
  gralph.fail("the PRE-FIX control binary leaked nothing (control_survivors=0). The probe can no longer detect the known defect, so the UUT result proves nothing. Investigate the probe before touching gralph")
  return
end

-- Survivors before path coverage, deliberately. This is the most specific failure and
-- the one that names which fix is missing, so it must be the reason the agent sees
-- first (gralph keeps the FIRST fail reason). Ordering it after the coverage check
-- would also leave the harness's single most important check permanently unexercised
-- while `graceful_stop` is unsupported -- i.e. untested until the day it matters.
if surv ~= 0 then
  gralph.fail(surv .. " descendant process(es) survived termination. See artifacts/zombie_proof.json for the per-path breakdown: `terminate_gralph` survivors mean the job object is missing or not KILL_ON_JOB_CLOSE (F1); `agent_timeout` survivors mean the launcher-side cancel still kills only the direct child (F2); `sigint_double` survivors mean the console-event path does not reap children that ignore Ctrl-C")
  return
end

if paths < 4 then
  gralph.fail("no survivors, but only " .. paths .. "/4 termination paths ran against the UUT. All four in spec/shutdown-contract.md must be covered: agent_timeout, sigint_double, terminate_gralph, graceful_stop. A missing `stop` subcommand silently drops one — implement F3")
  return
end

-- provenance + shape from the file the probe just wrote
local blob = L.slurp("artifacts/zombie_proof.json")
if not blob then gralph.fail("artifacts/zombie_proof.json was not written by the probe"); return end
if not L.must(blob, '"proof_source":"live-pid-snapshot"',
  "the verdict must come from a live PID snapshot, not from a Go test's own claim") then return end
if not L.must(blob, '"control_leaked":true', "the control run must still demonstrate the leak") then return end
if not L.must(blob, '"survivors":[]', "no descendant may survive any termination path") then return end
