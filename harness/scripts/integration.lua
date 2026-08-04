-- integration: the WHOLE suite, not the per-fix selections.
-- Per-fix gates run `go test -run <pattern>`, so they are structurally blind to a fix
-- that satisfies its own test while breaking someone else's. This runs everything and
-- binds the pass count to the baseline recorded at preflight, so "green by deletion"
-- is not available.
local L = dofile(gralph.profile_dir .. "/scripts/lib.lua")

local q = io.popen("python scripts/check_go.py --test")
local g = q:read("*a"); q:close()
local ran  = L.num(g, "gotest_ran=(%d+)")
local pass = L.num(g, "gotest_pass=(%d+)")
local fail = L.num(g, "gotest_fail=(%d+)")
if not ran or ran == 0 then
  gralph.fail("`go test ./...` produced no test events — the package does not build")
  return
end
if fail == nil or pass == nil then
  gralph.fail("check_go.py printed no test counts — run `python scripts/check_go.py --test` and report its output")
  return
end
if fail ~= 0 then
  gralph.fail(fail .. " test(s) failing in the full suite. Per-fix tests passing while the suite fails means a fix broke another one — fix the code, never skip or delete a test")
  return
end
if pass == 0 then
  gralph.fail("0 passing tests in the full suite — a hollow green")
  return
end

local base = tonumber(gralph.store.get("base_pass")) or 0
if base > 0 and pass < base then
  gralph.fail("the suite now passes " .. pass .. " tests but the pre-change baseline passed " .. base ..
              ". Tests were removed or stopped running. Restore them; the fix must ADD tests, never subtract")
  return
end

-- The referees must still be the ones preflight pinned.
local m = io.popen("python scripts/probe_manifest.py")
local ms = m:read("*a"); m:close()
local mism, miss = L.num(ms, "mismatched=(%d+)"), L.num(ms, "missing=(%d+)")
if mism == nil or miss == nil then gralph.fail("probe_manifest.py printed no counts"); return end
if mism ~= 0 or miss ~= 0 then
  gralph.fail("HARNESS-OWNED probe files changed during the build (" .. mism .. " changed, " .. miss ..
              " missing) — restore them with `git checkout -- harness/` and re-run")
  return
end

local blob = L.slurp("artifacts/integration.json")
if not blob then
  gralph.fail("artifacts/integration.json not found — report {\"gotest_ran\":1,\"gotest_pass\":N,\"gotest_fail\":0,\"baseline_pass\":" .. base .. ",\"assertions_preserved\":true,\"probes_unmodified\":true}")
  return
end
if not L.must(blob, '"gotest_fail":0', "report the full-suite failure count") then return end
if not L.must(blob, '"assertions_preserved":true',
  "state that no existing assertion was weakened to make the suite green") then return end
if not L.must(blob, '"probes_unmodified":true', "state that the referees are untouched") then return end
