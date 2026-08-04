-- decompose: the five fixes are FIXED by spec, not invented by the agent. This gate
-- exists to make the agent state, per fix, which Go test name will prove it and which
-- files it will touch -- so `write-tests` and `implement` have something to bind to.
local L = dofile(gralph.profile_dir .. "/scripts/lib.lua")
local blob = L.slurp("artifacts/fixes.json")
if not blob then
  gralph.fail("artifacts/fixes.json not found — read PRD.md and spec/, then emit one record per fix id with test_pattern + files")
  return
end

-- The ids come from spec/shutdown-contract.md + spec/defect-analysis.md. Renaming or
-- dropping one silently narrows the build, so each is required by name.
local ids = {"F1-jobobject", "F2-treekill", "F3-graceful-stop", "F4-run-lock", "F5-block-verb"}
for _, id in ipairs(ids) do
  if not L.must(blob, '"id":"' .. id .. '"',
    "every fix in the spec must appear; do not merge, rename or drop " .. id) then return end
end

local n = L.num(blob, '"count":(%d+)')
if not n then gralph.fail('report "count":5 — the number of fix records you emitted'); return end
if n ~= 5 then gralph.fail("count=" .. n .. " but the spec defines exactly 5 fixes"); return end

-- Each fix must name the Go test that will prove it, or write-tests has no target.
local pats = 0
for _ in blob:gmatch('"test_pattern":"') do pats = pats + 1 end
if pats < 5 then
  gralph.fail("only " .. pats .. "/5 fixes declare a \"test_pattern\" (the `go test -run` regex that proves that fix) — add the missing ones")
  return
end
if not L.must(blob, '"acceptance"', "each fix needs an \"acceptance\" string naming the observable it must produce") then return end

gralph.store.set("fix_total", 5)
gralph.store.set("fix_done", 0)
gralph.store.set("green_done", 0)
