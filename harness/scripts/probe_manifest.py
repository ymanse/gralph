#!/usr/bin/env python
"""Referee integrity. HARNESS-OWNED: do not edit.

The lifecycle proofs are only worth what their referees are worth. The build agent
edits Go code; it must not be able to soften the probes that judge it. This records a
sha256 for every harness-owned probe file, and re-verifies them from the gate.

  --write   write probe_manifest.json (author-time, ONCE, before the loop runs)
  (default) verify; prints `checked=N mismatched=M missing=K` and exits non-zero on
            any mismatch, so a Lua gate can bind to the counts.
"""
import argparse
import hashlib
import json
import os
import sys

HARNESS = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MANIFEST = os.path.join(HARNESS, "probe_manifest.json")

# Everything that decides whether a proof passes. If the agent needs one of these
# changed, that is a harness-author decision, not something the loop may do.
PROBES = [
    "scripts/proof_zombie.py",
    "scripts/proof_shutdown.py",
    "scripts/proc.py",
    "scripts/fake_agent.py",
    "scripts/probe_manifest.py",
    # Also referees: the lint/integration/implement gates read these counts, so an
    # agent that could edit them could make every check report success.
    "scripts/check_go.py",
    "scripts/scan_lifecycle.py",
    "scripts/sha256_of.py",
    "probe-hang.yaml",
    "probe-timeout.yaml",
    "probe-complete.yaml",
    "probe-block.yaml",
    "spec/shutdown-contract.md",
    # The gates themselves. A gate cannot protect its own bytes -- an edited gate could
    # simply delete the check. What this buys is CROSS-protection: four different gates
    # verify this manifest, so weakening any one of them is caught by the others.
    "lifecycle.yaml",
    "scripts/preflight.lua",
    "scripts/decompose.lua",
    "scripts/write_tests.lua",
    "scripts/implement.lua",
    "scripts/lint.lua",
    "scripts/integration.lua",
    "scripts/zombie_proof.lua",
    "scripts/shutdown_proof.lua",
    "scripts/dep_verify.lua",
    "scripts/lib.lua",
]


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 16), b""):
            h.update(chunk)
    return h.hexdigest()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--write", action="store_true")
    a = ap.parse_args()

    if a.write:
        m = {}
        for rel in PROBES:
            m[rel] = sha256(os.path.join(HARNESS, rel))
        with open(MANIFEST, "w", encoding="utf-8", newline="\n") as f:
            json.dump({"algo": "sha256", "files": m}, f, indent=2, sort_keys=True)
            f.write("\n")
        print(f"wrote {MANIFEST} ({len(m)} files)")
        return 0

    try:
        man = json.load(open(MANIFEST, encoding="utf-8"))["files"]
    except (OSError, ValueError, KeyError):
        print("checked=0 mismatched=0 missing=0 manifest=absent")
        return 2

    checked = mismatched = missing = 0
    bad = []
    for rel, want in sorted(man.items()):
        p = os.path.join(HARNESS, rel)
        if not os.path.exists(p):
            missing += 1
            bad.append(f"MISSING {rel}")
            continue
        checked += 1
        got = sha256(p)
        if got != want:
            mismatched += 1
            bad.append(f"CHANGED {rel}\n  want {want}\n  got  {got}")

    print(f"checked={checked} mismatched={mismatched} missing={missing} manifest=present")
    for b in bad:
        print(b, file=sys.stderr)
    return 1 if (mismatched or missing) else 0


if __name__ == "__main__":
    sys.exit(main())
