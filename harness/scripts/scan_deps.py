#!/usr/bin/env python
"""Dependency provenance for dep-verify. HARNESS-OWNED: do not edit.

Diffs the working go.mod against the upstream baseline this fork branched from, so a
"small lifecycle fix" cannot quietly pull in a process-management library. The job
object is reachable from the standard library (`syscall.NewLazyDLL("kernel32.dll")`)
and from golang.org/x/sys/windows; anything else needs a human decision.

Prints:
  baseline_found=1 deps_added=N deps_removed=N disallowed=N
"""
import argparse
import os
import re
import subprocess
import sys

HARNESS = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPO = os.path.dirname(HARNESS)

ALLOWED_NEW = {"golang.org/x/sys"}
REQ = re.compile(r"^\s*([\w./\-]+)\s+v[\w.\-+]+", re.M)


def parse_requires(text):
    """Module paths inside require blocks/lines, ignoring the module/go directives."""
    out = set()
    for m in REQ.finditer(text):
        p = m.group(1)
        if p in ("module", "go", "toolchain", "require", "replace", "exclude"):
            continue
        if "." in p.split("/")[0]:          # a module path always has a dotted host
            out.add(p)
    return out


def baseline_gomod(ref):
    r = subprocess.run(["git", "show", f"{ref}:go.mod"], cwd=REPO,
                       capture_output=True, text=True, timeout=120)
    return r.stdout if r.returncode == 0 else None


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--baseline", default="upstream/main",
                    help="git ref to compare go.mod against")
    a = ap.parse_args()

    try:
        cur = open(os.path.join(REPO, "go.mod"), encoding="utf-8").read()
    except OSError:
        print("baseline_found=0 deps_added=0 deps_removed=0 disallowed=0")
        print("go.mod unreadable", file=sys.stderr)
        return 2

    base_text = baseline_gomod(a.baseline)
    if base_text is None:
        # Never silently pass: an unresolvable baseline is a fail-closed condition.
        print("baseline_found=0 deps_added=0 deps_removed=0 disallowed=0")
        print(f"cannot read {a.baseline}:go.mod -- `git fetch upstream`", file=sys.stderr)
        return 2

    base, now = parse_requires(base_text), parse_requires(cur)
    added, removed = sorted(now - base), sorted(base - now)
    disallowed = [d for d in added
                  if not any(d == x or d.startswith(x + "/") for x in ALLOWED_NEW)]

    print(f"baseline_found=1 deps_added={len(added)} deps_removed={len(removed)} "
          f"disallowed={len(disallowed)}")
    if added:
        print("added: " + ", ".join(added), file=sys.stderr)
    if removed:
        print("removed: " + ", ".join(removed), file=sys.stderr)
    if disallowed:
        print("disallowed: " + ", ".join(disallowed), file=sys.stderr)
    return 1 if (disallowed or removed) else 0


if __name__ == "__main__":
    sys.exit(main())
