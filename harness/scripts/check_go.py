#!/usr/bin/env python
"""Go toolchain facts for the gates. HARNESS-OWNED: do not edit.

Runs the real toolchain in the repo root (the harness's parent) and prints a flat
`key=value` line the Lua gates read back. Counts, not just exit codes -- an exit code
cannot tell a gate whether anything was actually compiled or run (law 1/5).

  python scripts/check_go.py [--test] [--fmt] [--vet] [--build <out>]

Prints e.g.:
  gotest_ran=1 gotest_pass=214 gotest_fail=0 gotest_pkgs=1 gofmt_unformatted=0 vet_issues=0
"""
import argparse
import json
import os
import re
import subprocess
import sys

HARNESS = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPO = os.path.dirname(HARNESS)


def run(args, timeout=1800):
    return subprocess.run(args, cwd=REPO, capture_output=True, text=True,
                          timeout=timeout, encoding="utf-8", errors="replace")


def go_test(run_pattern=""):
    """`go test -json ./...` -> (ran, passed, failed, pkgs). ran=0 means the toolchain
    itself failed, which must never be confused with 'zero failures'."""
    args = ["go", "test", "-json", "-count=1"]
    if run_pattern:
        args += ["-run", run_pattern]
    args += ["./..."]
    r = run(args)
    passed = failed = 0
    pkgs = set()
    saw_any = False
    for line in r.stdout.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            ev = json.loads(line)
        except ValueError:
            continue
        saw_any = True
        act, test = ev.get("Action"), ev.get("Test")
        if ev.get("Package"):
            pkgs.add(ev["Package"])
        if not test:
            continue
        if act == "pass":
            passed += 1
        elif act == "fail":
            failed += 1
    # A build error yields no test events at all: report ran=0 rather than fail=0.
    return (1 if saw_any else 0), passed, failed, len(pkgs), r.stdout[-4000:] + r.stderr[-4000:]


def go_fmt():
    r = run(["gofmt", "-l", "."])
    files = [x for x in r.stdout.splitlines() if x.strip()]
    return len(files), files


def go_vet():
    r = run(["go", "vet", "./..."])
    issues = [x for x in r.stderr.splitlines() if x.strip() and not x.startswith("#")]
    return (0 if r.returncode == 0 else max(1, len(issues))), issues


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--test", action="store_true")
    ap.add_argument("--fmt", action="store_true")
    ap.add_argument("--vet", action="store_true")
    ap.add_argument("--build", default="")
    ap.add_argument("--run", default="", help="go test -run regex (subset)")
    ap.add_argument("--cross", action="store_true",
                    help="verify the non-Windows build still compiles (POSIX parity)")
    ap.add_argument("--expect", choices=["pass", "fail", "any"], default="any",
                    help="required outcome; sets the exit code so a gate can bind to it")
    ap.add_argument("--verbose", action="store_true")
    a = ap.parse_args()
    if a.run:
        a.test = True
    if not any([a.test, a.fmt, a.vet, a.build, a.cross]):
        a.test = a.fmt = a.vet = True
    # `-run` selecting nothing yields ran=1/pass=0/fail=0, which would satisfy a naive
    # "no failures" check. Shape-validate here and count-bind in the gate.
    if a.run and not re.fullmatch(r"[A-Za-z0-9_./|^$()\-]+", a.run):
        print("bad --run pattern", file=sys.stderr)
        return 2

    out, detail, rc = {}, [], 0

    if a.build:
        dest = a.build if os.path.isabs(a.build) else os.path.join(HARNESS, a.build)
        os.makedirs(os.path.dirname(dest), exist_ok=True)
        r = run(["go", "build", "-o", dest, "."])
        out["build_ok"] = 1 if r.returncode == 0 else 0
        if r.returncode != 0:
            rc = 1
            detail.append(r.stderr[-4000:])

    if a.test:
        ran, p, f, pkgs, raw = go_test(a.run)
        out.update(gotest_ran=ran, gotest_pass=p, gotest_fail=f, gotest_pkgs=pkgs)
        if ran == 0:
            rc = 1
            detail.append(raw)
        elif a.expect == "fail":
            # RED: the selected tests must actually run AND fail.
            if f == 0 or (p + f) == 0:
                rc = 1
                detail.append("expected RED (>=1 failing test) but got "
                              f"pass={p} fail={f}\n" + raw)
        elif a.expect in ("pass", "any"):
            if f:
                rc = 1
                detail.append(raw)
            if a.expect == "pass" and p == 0:
                rc = 1
                detail.append(f"expected GREEN with >=1 passing test but got pass=0\n{raw}")

    if a.cross:
        # A Windows-only fix that breaks the POSIX build is a regression, and CI would
        # only catch it after the fact. Build (not test) for each: no cross execution.
        ok = 0
        for goos in ("linux", "darwin"):
            env = dict(os.environ, GOOS=goos, GOARCH="amd64", CGO_ENABLED="0")
            r = subprocess.run(["go", "build", "-o", os.devnull, "./..."], cwd=REPO,
                               capture_output=True, text=True, timeout=1800, env=env)
            if r.returncode == 0:
                ok += 1
            else:
                detail.append(f"GOOS={goos}: {r.stderr[-2000:]}")
        out["cross_builds_ok"] = ok
        out["cross_targets"] = 2
        if ok != 2:
            rc = 1

    if a.fmt:
        n, files = go_fmt()
        out["gofmt_unformatted"] = n
        if n:
            rc = 1
            detail.append("gofmt: " + ", ".join(files[:40]))

    if a.vet:
        n, issues = go_vet()
        out["vet_issues"] = n
        if n:
            rc = 1
            detail.append("\n".join(issues[:40]))

    print(" ".join(f"{k}={v}" for k, v in out.items()))
    if detail and (a.verbose or rc):
        sys.stderr.write("\n".join(detail) + "\n")
    return rc


if __name__ == "__main__":
    sys.exit(main())
