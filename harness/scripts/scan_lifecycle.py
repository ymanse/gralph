#!/usr/bin/env python
"""Structural scan of the lifecycle fix in the Go sources. HARNESS-OWNED: do not edit.

The runtime proofs (proof_zombie / proof_shutdown) say the tree died. This says the
code is actually built the way the contract requires -- so a fix that happens to pass
one probe path by luck, or that patches only one of the two cancellation sites, is
still rejected.

The two cancel sites are the ones measured in spec/defect-analysis.md:
  launcher.go        host -> launcher
  galp_subprocess.go launcher -> agent

Prints:
  files_scanned=N legacy_cancel_sites=N cancel_sites=N killtree_refs=N
  jobobject_refs=N build_tagged_windows=N build_tagged_other=N
"""
import argparse
import json
import os
import re
import sys

HARNESS = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPO = os.path.dirname(HARNESS)

# The exact defect: SIGTERM (a no-op on Windows) falling back to a single-process Kill.
LEGACY = re.compile(r"Process\.Signal\(\s*syscall\.SIGTERM\s*\)")
# Whatever the fix names its tree-killer, it must be referenced by both cancel sites.
KILLTREE = re.compile(r"\bkillTree\b|\bterminateTree\b|\bkillProcessTree\b")
JOBOBJ = re.compile(r"JobObject|JOB_OBJECT|AssignProcessToJobObject|superviseTree")
CANCEL = re.compile(r"cmd\.Cancel\s*=")

# Files that must contain a tree-kill reference once the fix lands.
CANCEL_SITES = ["launcher.go", "galp_subprocess.go"]


def go_files():
    out = []
    for name in sorted(os.listdir(REPO)):
        if name.endswith(".go"):
            out.append(name)
    return out


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--json", default="")
    a = ap.parse_args()

    files = go_files()
    legacy_sites, killtree_files, jobobj_files, cancel_files = [], [], [], []
    win_tagged, other_tagged = [], []

    for name in files:
        path = os.path.join(REPO, name)
        try:
            src = open(path, encoding="utf-8", errors="replace").read()
        except OSError:
            continue
        # A file's own name decides its build constraint on Windows (_windows.go), and
        # an explicit //go:build tag counts too.
        head = src[:400]
        if name.endswith("_windows.go") or re.search(r"//go:build[^\n]*\bwindows\b", head):
            win_tagged.append(name)
        elif re.search(r"//go:build[^\n]*!windows", head) or name.endswith("_other.go"):
            other_tagged.append(name)

        if LEGACY.search(src):
            legacy_sites.append(name)
        if KILLTREE.search(src):
            killtree_files.append(name)
        if JOBOBJ.search(src):
            jobobj_files.append(name)
        if CANCEL.search(src):
            cancel_files.append(name)

    # Every cancel site must route through the tree-killer, not a bare Kill().
    unfixed_sites = [f for f in CANCEL_SITES
                     if f in cancel_files and f not in killtree_files]

    res = {
        "files_scanned": len(files),
        "legacy_cancel_sites": len(legacy_sites),
        "legacy_cancel_files": legacy_sites,
        "cancel_sites": len(cancel_files),
        # int form so a Lua gate can bind to it (the key=value line carries ints only)
        "unfixed_cancel_sites_n": len(unfixed_sites),
        "unfixed_cancel_sites": unfixed_sites,
        "killtree_refs": len(killtree_files),
        "killtree_files": killtree_files,
        "jobobject_refs": len(jobobj_files),
        "jobobject_files": jobobj_files,
        "build_tagged_windows": len(win_tagged),
        "build_tagged_other": len(other_tagged),
    }
    if a.json:
        dest = a.json if os.path.isabs(a.json) else os.path.join(HARNESS, a.json)
        os.makedirs(os.path.dirname(dest), exist_ok=True)
        with open(dest, "w", encoding="utf-8", newline="\n") as f:
            json.dump(res, f, indent=2)

    print(" ".join(f"{k}={v}" for k, v in res.items()
                   if isinstance(v, int)))
    if legacy_sites or unfixed_sites:
        sys.stderr.write(f"legacy={legacy_sites} unfixed_cancel_sites={unfixed_sites}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
