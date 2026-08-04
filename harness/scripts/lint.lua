-- lint: formatting, vet, and the STRUCTURAL shape of the fix.
-- The runtime proofs come later; this catches the class of "fix" that makes one probe
-- path pass while leaving the other cancel site on the old bare-Kill path. Both sites
-- were measured in spec/defect-analysis.md, so both are named here.
local L = dofile(gralph.profile_dir .. "/scripts/lib.lua")

local q = io.popen("python scripts/check_go.py --fmt --vet")
local g = q:read("*a"); q:close()
local fmtn = L.num(g, "gofmt_unformatted=(%d+)")
local vet  = L.num(g, "vet_issues=(%d+)")
if fmtn == nil or vet == nil then
  gralph.fail("check_go.py printed no fmt/vet counts — run `python scripts/check_go.py --fmt --vet` and report its output")
  return
end
if fmtn ~= 0 then gralph.fail("gofmt reports " .. fmtn .. " unformatted file(s) — run `gofmt -w .` in the repo root"); return end
if vet ~= 0 then gralph.fail("`go vet` reports " .. vet .. " issue(s)"); return end

local p = io.popen("python scripts/scan_lifecycle.py")
local s = p:read("*a"); p:close()
local scanned  = L.num(s, "files_scanned=(%d+)")
local legacy   = L.num(s, "legacy_cancel_sites=(%d+)")
local unfixed  = L.num(s, "unfixed_cancel_sites_n=(%d+)")
local killtree = L.num(s, "killtree_refs=(%d+)")
local jobobj   = L.num(s, "jobobject_refs=(%d+)")
local winfiles = L.num(s, "build_tagged_windows=(%d+)")
local othfiles = L.num(s, "build_tagged_other=(%d+)")

-- law 1/5: an empty scan must never read as a clean one.
if not scanned or scanned < 20 then
  gralph.fail("scan_lifecycle.py examined " .. tostring(scanned) .. " Go files (expected >=20) — a hollow scan, not a clean result")
  return
end
if legacy == nil or unfixed == nil or killtree == nil or jobobj == nil then
  gralph.fail("scan_lifecycle.py did not print all counts — run it and report its output verbatim")
  return
end

if legacy ~= 0 then
  gralph.fail(legacy .. " site(s) still call Process.Signal(syscall.SIGTERM) and fall back to a single-process Kill(). Measured: that Signal ALWAYS errors on Windows, so the fallback is the only path taken and it orphans every grandchild. Route both cancel sites through the tree-killer")
  return
end
if unfixed ~= 0 then
  gralph.fail(unfixed .. " cmd.Cancel site(s) (launcher.go / galp_subprocess.go) do not reference the tree-killer. Patching only one leaves the other leak path live — the agent-timeout path and the host-cancel path are different bugs")
  return
end
if killtree < 2 then
  gralph.fail("only " .. killtree .. " file(s) reference a tree-killer; both cancel sites must use it (expected >=2)")
  return
end
if jobobj < 1 then
  gralph.fail("no Job Object reference found. A TerminateProcess on gralph runs no Go code at all, so nothing but an OS-level job with KILL_ON_JOB_CLOSE can reap the tree in that path")
  return
end
-- POSIX parity: a Windows-only file must be matched by a non-Windows counterpart, or
-- the package stops building on linux/darwin (proved for real in dep-verify).
if not winfiles or winfiles < 3 then
  gralph.fail("expected at least 3 windows-tagged files (the 2 pre-existing + the new lifecycle one); found " .. tostring(winfiles))
  return
end
if not othfiles or othfiles < 1 then
  gralph.fail("no !windows-tagged counterpart found — the new Windows lifecycle code needs a POSIX stub or the package will not build on linux/darwin")
  return
end

local blob = L.slurp("artifacts/lint.json")
if not blob then
  gralph.fail("artifacts/lint.json not found — report {\"gofmt_unformatted\":0,\"vet_issues\":0,\"scan_source\":\"scan_lifecycle\",\"legacy_cancel_sites\":0,\"unfixed_cancel_sites\":0,\"killtree_refs\":N,\"jobobject_refs\":N}")
  return
end
if not L.must(blob, '"scan_source":"scan_lifecycle"',
  "the structural verdict must cite the scanner that produced it") then return end
if not L.must(blob, '"legacy_cancel_sites":0', "report the scanner's count") then return end
if not L.must(blob, '"unfixed_cancel_sites":0', "report the scanner's count") then return end
