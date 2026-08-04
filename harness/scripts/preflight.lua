-- preflight: the harness may only start from a known-good, un-tampered baseline.
-- Three independent things must hold, and each is recomputed here rather than read
-- from the agent's report:
--   1. every referee file still hashes to its pinned value (the agent must not be
--      able to soften the probes that will judge it),
--   2. the pre-fix control baseline on disk actually recorded a LEAK -- if the probe
--      could not see the known defect, nothing downstream means anything (law 4/5),
--   3. the Go toolchain is green BEFORE any edit, so a later RED is attributable.
local L = dofile(gralph.profile_dir .. "/scripts/lib.lua")

-- 1. referee integrity ------------------------------------------------------------
local p = io.popen("python scripts/probe_manifest.py")
local s = p:read("*a"); p:close()
local checked = L.num(s, "checked=(%d+)")
local mism    = L.num(s, "mismatched=(%d+)")
local miss    = L.num(s, "missing=(%d+)")
if not checked or not mism or not miss then
  gralph.fail("probe_manifest.py produced no counts — run `python scripts/probe_manifest.py` and report its output")
  return
end
if checked < 24 then
  gralph.fail("probe manifest covers only " .. checked .. " files (expected >=24) — the manifest was truncated; restore it from git")
  return
end
if mism ~= 0 or miss ~= 0 then
  gralph.fail("HARNESS-OWNED probe files were modified (" .. mism .. " changed, " .. miss ..
              " missing). The referees are not yours to edit: `git checkout -- harness/scripts harness/probe-*.yaml` and fix the Go code instead")
  return
end

-- 2. the control baseline must prove the probe detects the defect ------------------
local base = L.slurp("artifacts/baseline/zombie_prefix_baseline.json")
if not base then
  gralph.fail("artifacts/baseline/zombie_prefix_baseline.json missing — regenerate with `python scripts/proof_zombie.py --uut bin/gralph-orch.exe --control bin/gralph-orch.exe --out artifacts/baseline/zombie_prefix_baseline.json`")
  return
end
if not L.must(base, '"control_leaked":true',
  "the pinned pre-fix binary must LEAK under the probe; a baseline with no leak means the probe is broken, not that gralph is fixed") then return end
if base:find('"control_survivors_total":0', 1, true) then
  gralph.fail("baseline records 0 control survivors — the probe cannot see the known defect; do not proceed")
  return
end

-- 3. toolchain green at HEAD ------------------------------------------------------
local q = io.popen("python scripts/check_go.py --test --fmt --vet")
local g = q:read("*a"); q:close()
local ran  = L.num(g, "gotest_ran=(%d+)")
local pass = L.num(g, "gotest_pass=(%d+)")
local fail = L.num(g, "gotest_fail=(%d+)")
local fmtn = L.num(g, "gofmt_unformatted=(%d+)")
local vet  = L.num(g, "vet_issues=(%d+)")
if not ran or ran == 0 then
  gralph.fail("`go test` produced no test events — the package does not build. Fix the build first")
  return
end
if not pass or pass == 0 then
  gralph.fail("0 passing tests — a hollow green; the suite did not actually run")
  return
end
if fail ~= 0 then
  gralph.fail("baseline `go test ./...` is not green (" .. tostring(fail) .. " failing) — the harness must start from green")
  return
end
if fmtn ~= 0 then
  gralph.fail("gofmt reports " .. tostring(fmtn) .. " unformatted files — run `gofmt -w .` in the repo root")
  return
end
if vet ~= 0 then
  gralph.fail("`go vet` reports " .. tostring(vet) .. " issue(s) — fix them before starting")
  return
end
-- Pin the pre-existing passing count. `integration` requires the final suite to have at
-- least this many passes, so deleting an inherited test to make the suite green fails.
gralph.store.set("base_pass", pass)

-- 4. the agent's own report, for provenance + the numbers it observed ---------------
local blob = L.slurp("artifacts/preflight.json")
if not blob then
  gralph.fail("artifacts/preflight.json not found — report {\"toolchain\":\"go\",\"baseline_green\":true,\"gotest_pass\":N,\"uut_build_ok\":true,\"probes_unmodified\":true}")
  return
end
if not L.must(blob, '"baseline_green":true', "state that the baseline suite is green") then return end
if not L.must(blob, '"probes_unmodified":true', "state that you left the HARNESS-OWNED probe files untouched") then return end
if not L.must(blob, '"uut_build_ok":true',
  "build the binary under test with `python scripts/check_go.py --build bin/gralph-uut.exe`; the harness NEVER rebuilds bin/gralph-orch.exe (it is the running orchestrator and the pinned control)") then return end
