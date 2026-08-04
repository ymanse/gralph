-- write-tests (self-loop over the 5 fixes): RED integrity.
-- A fresh test must COMPILE, RUN and FAIL. `go test` reports "ok ... [no tests to run]"
-- when a -run pattern selects nothing, so an empty selection would otherwise read as a
-- clean pass -- the gate binds to counts, and check_go.py --expect fail rejects pass=0.
local L = dofile(gralph.profile_dir .. "/scripts/lib.lua")
local fix = gralph.args.fix
if not fix or fix == "" then gralph.fail("--fix <id> required (e.g. F1-jobobject)"); return end
if not fix:match("^[%w%-]+$") then gralph.fail("--fix must be alphanumeric/dash only"); return end

local blob = L.slurp("artifacts/tests_red/" .. fix .. ".json")
if not blob then
  gralph.fail("artifacts/tests_red/" .. fix .. ".json not found — write the failing Go test, RUN it, and report {\"errors\":0,\"collected\":N,\"passed\":0,\"failed\":N,\"negative_paths\":N,\"test_pattern\":\"...\",\"test_files\":\"a_test.go,b_test.go\",\"tests_hash\":\"<sha256>\"}")
  return
end

if not L.must(blob, '"errors":0', "compile/build errors must be 0 — a test that does not build is not a valid RED") then return end
if blob:find('"collected":0', 1, true) then gralph.fail(fix .. ": 0 tests collected — define real tests"); return end
if not L.must(blob, '"collected":', "report the collected test count") then return end
if not L.must(blob, '"passed":0', "a fresh test for an unimplemented fix must NOT already pass") then return end
if blob:find('"failed":0', 1, true) then gralph.fail(fix .. ": the test did not FAIL — it must be RED before implementing"); return end
if not L.must(blob, '"failed":', "report the failed count") then return end
if blob:find('"negative_paths":0', 1, true) then
  gralph.fail(fix .. ": no negative/error-path test. Each lifecycle fix needs at least one test for the FAILURE direction (e.g. a lock that is NOT stale must not be reaped)")
  return
end
if not L.must(blob, '"negative_paths":', "report negative_paths count") then return end

-- in-gate recompute: the decisive claim is not the agent's number, it is the runner's.
local pat = blob:match('"test_pattern":"([^"]+)"')
if not pat then gralph.fail('report "test_pattern" — the `go test -run` regex that selects this fix\'s tests'); return end
if not pat:match("^[%w_/|%^%$%(%)%.%-]+$") then
  gralph.fail("test_pattern has characters that cannot be passed to `go test -run` safely: " .. pat)
  return
end
local p = io.popen("python scripts/check_go.py --run " .. pat .. " --expect fail")
local s = p:read("*a"); p:close()
local ran  = L.num(s, "gotest_ran=(%d+)")
local pass = L.num(s, "gotest_pass=(%d+)")
local fail = L.num(s, "gotest_fail=(%d+)")
if not ran or ran == 0 then
  gralph.fail(fix .. ": `go test -run " .. pat .. "` produced no test events — the package does not build")
  return
end
if not fail or fail == 0 then
  gralph.fail(fix .. ": re-running `go test -run " .. pat .. "` found " .. tostring(pass) ..
              " passing and 0 failing. The recorded RED does not reproduce — the pattern selects the wrong tests, or the test does not actually assert the missing behaviour")
  return
end

-- Pin the test bytes so `implement` can prove it did not weaken them.
local files = blob:match('"test_files":"([^"]+)"')
if not files then gralph.fail('report "test_files" — comma-separated Go test files, repo-root relative, no spaces'); return end
if not files:match("^[%w_%./,%-]+$") then gralph.fail("test_files may only contain [A-Za-z0-9_./,-]: " .. files); return end
local h = io.popen("python scripts/sha256_of.py " .. files:gsub(",", " "))
local digest = h:read("*a"):gsub("%s+", ""); h:close()
if not digest:match("^%x%x%x+$") then
  gralph.fail("could not hash test_files (" .. files .. ") — do those paths exist relative to the repo root?")
  return
end
local recorded = blob:match('"tests_hash":"(%x+)"')
if not recorded then gralph.fail('report "tests_hash" — sha256 from `python scripts/sha256_of.py ' .. files:gsub(",", " ") .. '`'); return end
if recorded ~= digest then
  gralph.fail(fix .. ": recorded tests_hash does not match the files on disk (" .. digest ..
              "). Re-run the hash command and report its exact output; never hand-write it")
  return
end
gralph.store.set("redhash:" .. fix, digest)
gralph.store.set("redpat:" .. fix, pat)

-- self-loop counter (success path only -- the store commits only on success)
local total = tonumber(gralph.store.get("fix_total")) or 0
local done  = tonumber(gralph.store.get("fix_done"))  or 0
if gralph.store.get("red:" .. fix) ~= 1 then
  gralph.store.set("red:" .. fix, 1); done = done + 1; gralph.store.set("fix_done", done)
end
if done < total then gralph.route("write-tests") else gralph.route("implement") end
