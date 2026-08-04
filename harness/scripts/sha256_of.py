#!/usr/bin/env python
"""Print ONLY the sha256 of a file, so a Lua gate can recompute a recorded hash
without any quoting. HARNESS-OWNED: do not edit.

Paths are resolved relative to the repo root (the harness's parent), which is where
the Go sources under test live. sha256-of-raw-bytes is deliberate: it is the method
the evidence recorder must use too. `git hash-object` is sha1 of "blob <len>\\0"+data
and can never match -- using it would permanently brick the RED baseline.
"""
import hashlib
import os
import sys

HARNESS = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPO = os.path.dirname(HARNESS)


def main() -> int:
    if len(sys.argv) < 2:
        print("usage: sha256_of.py <path> [<path>...]", file=sys.stderr)
        return 2
    h = hashlib.sha256()
    for rel in sys.argv[1:]:                       # multiple files hash in argv order
        p = rel if os.path.isabs(rel) else os.path.join(REPO, rel)
        if not os.path.exists(p):
            print(f"missing: {p}", file=sys.stderr)
            return 1
        with open(p, "rb") as f:
            for chunk in iter(lambda: f.read(1 << 16), b""):
                h.update(chunk)
    print(h.hexdigest())
    return 0


if __name__ == "__main__":
    sys.exit(main())
