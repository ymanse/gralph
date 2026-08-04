-- implement (self-loop over the 5 fixes): GREEN anti-tamper.
-- The cheapest way to turn RED into GREEN is to weaken the test, so the hash recorded
-- at RED time is recomputed here FROM DISK with the same method (sha256 of raw bytes).
-- Deleting, skipping or relaxing the test changes those bytes and the gate rejects it.
local L = dofile(gralph.profile_dir .. "/scripts/lib.lua")
local fix = gralph.args.fix
if not fix or fix == "" then gralph.fail("--fix <id> required"); return end
if not fix:match("^[%w%-]+$") then gralph.fail("--fix must be alphanumeric/dash only"); return end

local blob = L.slurp("artifacts/impl/" .. fix .. ".json")
if not blob then
  gralph.fail("artifacts/impl/" .. fix .. ".json not found — implement the fix, run its tests, and report {\"failed\":0,\"skipped\":0,\"passed\":N,\"tests_baseline\":\"red\",\"tests_unmodified\":true,\"test_files\":\"...\",\"tests_hash\":\"<sha256>\",\"tests_hash_red\":\"<sha256>\"}")
  return
end

if not L.must(blob, '"failed":0', fix .. " tests are not GREEN") then return end
if not L.must(blob, '"skipped":0', "skipping is not passing — skipped must be 0") then return end
if blob:find('"passed":0', 1, true) then gralph.fail(fix .. ": 0 passing tests — GREEN needs >=1 real pass"); return end
if not L.must(blob, '"passed":', "report the passed count") then return end

-- tests-integrity, declared baseline (this build has no certified refactor path: the
-- RED tests are written against a contract that does not change mid-build).
if not L.must(blob, '"tests_baseline":"red"',
  'declare "tests_baseline":"red" — the RED tests must be byte-identical through implement') then return end
if not L.must(blob, '"tests_unmodified":true', "the RED tests must be unchanged") then return end

local files = blob:match('"test_files":"([^"]+)"')
if not files then gralph.fail('report "test_files" — the same list you recorded at RED'); return end
if not files:match("^[%w_%./,%-]+$") then gralph.fail("test_files may only contain [A-Za-z0-9_./,-]"); return end

local h = io.popen("python scripts/sha256_of.py " .. files:gsub(",", " "))
local digest = h:read("*a"):gsub("%s+", ""); h:close()
if not digest:match("^%x%x%x+$") then gralph.fail("could not hash test_files (" .. files .. ")"); return end

-- The authoritative RED hash is the one the write-tests gate stored, not one the agent
-- can restate here. A missing store entry means this fix never passed RED.
local red = gralph.store.get("redhash:" .. fix)
if not red or red == "" then
  gralph.fail(fix .. ": no RED hash on record — this fix has not passed `write-tests`; do that first")
  return
end
if digest ~= red then
  gralph.fail(fix .. ": the test files changed since RED (on disk " .. digest .. ", RED " .. tostring(red) ..
              "). Revert the test and make the PRODUCT code satisfy it — weakening the test is the one thing this gate exists to stop")
  return
end
if not L.must(blob, '"tests_hash_red":"' .. red .. '"', "cite the RED hash you were measured against") then return end

-- in-gate recompute: the same selection that was RED must now actually be GREEN.
local pat = gralph.store.get("redpat:" .. fix)
if not pat or pat == "" then gralph.fail(fix .. ": no test_pattern on record from RED"); return end
local p = io.popen("python scripts/check_go.py --run " .. pat .. " --expect pass")
local s = p:read("*a"); p:close()
local ran  = L.num(s, "gotest_ran=(%d+)")
local pass = L.num(s, "gotest_pass=(%d+)")
local fail = L.num(s, "gotest_fail=(%d+)")
if not ran or ran == 0 then gralph.fail(fix .. ": the package does not build"); return end
if not pass or pass == 0 then
  gralph.fail(fix .. ": `go test -run " .. pat .. "` selected 0 tests. `go test` prints \"ok [no tests to run]\" for an empty selection — that is not GREEN")
  return
end
if fail ~= 0 then
  gralph.fail(fix .. ": " .. tostring(fail) .. " test(s) still failing under `go test -run " .. pat .. "`")
  return
end

-- Structural: a fix that passes its own test but leaves a cancel site unpatched has
-- not fixed the defect. Checked per-fix so the gap is caught early, not at lint.
local q = io.popen("python scripts/scan_lifecycle.py")
local g = q:read("*a"); q:close()
local scanned = L.num(g, "files_scanned=(%d+)")
if not scanned or scanned < 20 then
  gralph.fail("scan_lifecycle.py examined " .. tostring(scanned) .. " files — a hollow scan; run it from the harness dir")
  return
end
if not L.must(blob, '"scan_source":"scan_lifecycle"',
  'report "scan_source":"scan_lifecycle" and the counts it printed — a claim without the scanner behind it proves nothing') then return end

local total = tonumber(gralph.store.get("fix_total")) or 0
local done  = tonumber(gralph.store.get("green_done")) or 0
if gralph.store.get("green:" .. fix) ~= 1 then
  gralph.store.set("green:" .. fix, 1); done = done + 1; gralph.store.set("green_done", done)
end
if done < total then gralph.route("implement") else gralph.route("lint") end
