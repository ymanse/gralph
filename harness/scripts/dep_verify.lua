-- dep-verify (final node -> DONE): the fix must be shippable back upstream.
-- Two things a passing test suite on Windows cannot tell you: whether the package
-- still builds for everyone else, and whether the fix quietly took a dependency with
-- it. Both are recomputed here.
local L = dofile(gralph.profile_dir .. "/scripts/lib.lua")

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
