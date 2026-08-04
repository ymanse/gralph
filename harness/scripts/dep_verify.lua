-- dep-verify (final node -> DONE): the fix must be shippable back upstream.
-- Two things a passing test suite on Windows cannot tell you: whether the package
-- still builds for everyone else, and whether the fix quietly took a dependency with
-- it. Both are recomputed here.
local L = dofile(gralph.profile_dir .. "/scripts/lib.lua")

-- 0. The suite, AGAIN. `integration` ran it three stages ago, and the agent edits Go
--    code at zombie-proof and shutdown-proof to make those probes pass. Nothing
--    between there and here re-runs the tests, so without this a fix made to satisfy
--    a probe could leave the suite red and still reach DONE. A terminal gate must
--    re-establish everything it is about to declare finished.
local t = io.popen("python scripts/check_go.py --test")
local ts = t:read("*a"); t:close()
local tran  = L.num(ts, "gotest_ran=(%d+)")
local tpass = L.num(ts, "gotest_pass=(%d+)")
local tfail = L.num(ts, "gotest_fail=(%d+)")
if tran == nil or tpass == nil or tfail == nil then
  gralph.fail("check_go.py printed no test counts — run `python scripts/check_go.py --test` and report its output")
  return
end
if tran == 0 then gralph.fail("`go test ./...` produced no test events — the package does not build"); return end
if tfail ~= 0 then
  gralph.fail(tfail .. " test(s) failing at the terminal gate. Code changed after `integration` (probably to satisfy a probe) and broke the suite — fix it; DONE must not ship a red suite")
  return
end
if tpass == 0 then gralph.fail("0 passing tests — a hollow green"); return end
local basep = tonumber(gralph.store.get("base_pass")) or 0
if basep > 0 and tpass < basep then
  gralph.fail("the suite passes " .. tpass .. " tests but the pre-change baseline passed " .. basep .. " — tests were lost between integration and here")
  return
end

-- 1. POSIX parity. A windows-tagged file with no counterpart breaks `go build` on
--    linux/darwin, which this machine would never notice on its own.
local c = io.popen("python scripts/check_go.py --cross")
local cs = c:read("*a"); c:close()
local okb = L.num(cs, "cross_builds_ok=(%d+)")
local tgt = L.num(cs, "cross_targets=(%d+)")
if okb == nil or tgt == nil then
  gralph.fail("check_go.py --cross printed no counts — run it and report its output")
  return
end
if tgt == 0 then gralph.fail("no cross targets were attempted — a hollow zero"); return end
if okb ~= tgt then
  gralph.fail(okb .. "/" .. tgt .. " cross-builds succeeded. The Windows lifecycle code needs a !windows counterpart that keeps the POSIX behaviour (process-group signal), not a stub that drops it")
  return
end

-- 2. Dependency provenance against the upstream baseline this fork branched from.
local d = io.popen("python scripts/scan_deps.py --baseline upstream/main")
local ds = d:read("*a"); d:close()
local found = L.num(ds, "baseline_found=(%d+)")
local added = L.num(ds, "deps_added=(%d+)")
local rmvd  = L.num(ds, "deps_removed=(%d+)")
local bad   = L.num(ds, "disallowed=(%d+)")
if found == nil or added == nil or bad == nil or rmvd == nil then
  gralph.fail("scan_deps.py printed no counts — run `python scripts/scan_deps.py` and report its output")
  return
end
if found ~= 1 then
  gralph.fail("could not read upstream/main:go.mod, so the dependency delta is unknown. Run `git fetch upstream` — an unverifiable delta is a FAIL, not a pass")
  return
end
if bad ~= 0 then
  gralph.fail(bad .. " new dependency/dependencies outside the allowlist. A job object is reachable from the stdlib (syscall.NewLazyDLL on kernel32) or golang.org/x/sys; anything else is a human decision, not a build-loop one")
  return
end
if rmvd ~= 0 then
  gralph.fail(rmvd .. " dependency/dependencies were REMOVED from go.mod — this fix has no reason to drop one")
  return
end

-- 3. The agent's report, with the provenance it is allowed to claim.
local blob = L.slurp("artifacts/dep_report.json")
if not blob then
  gralph.fail("artifacts/dep_report.json not found — report {\"graph_source\":\"on-disk-static\",\"staleness_impossible\":true,\"files_scanned\":N,\"cross_builds_ok\":2,\"deps_added\":N,\"disallowed\":0,\"upstream_baseline\":\"upstream/main\"}")
  return
end
if not L.must(blob, '"graph_source":"on-disk-static"',
  'the verdict must come from reading the sources at HEAD; there is no code-intel index for this repo, so a "cg-self" claim would be a hollow zero') then return end
if not L.must(blob, '"staleness_impossible":true',
  "an on-disk scan has no cache to drift — state it") then return end
if blob:find('"files_scanned":0', 1, true) then gralph.fail("files_scanned is 0 — the scan covered nothing"); return end
if not L.must(blob, '"files_scanned":', "report how many Go files were examined") then return end
if not L.must(blob, '"cross_builds_ok":2', "report the cross-build count you observed") then return end
if not L.must(blob, '"disallowed":0', "report the dependency scanner's count") then return end
if not L.must(blob, '"upstream_baseline":"upstream/main"', "name the baseline you were diffed against") then return end
