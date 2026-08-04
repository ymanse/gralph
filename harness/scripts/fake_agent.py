#!/usr/bin/env python
"""Stand-in for `claude -p` in the lifecycle proofs. HARNESS-OWNED: do not edit.

Reproduces the shape that leaks in production -- an agent process that itself owns
several long-lived children (the MCP servers). The probe watches these PIDs, so this
file's behaviour IS the measurement; changing it changes what "no zombies" means.

  --mode hang      spawn children, never exit on its own (must be killed by gralph)
  --mode complete  spawn children, work for <lifetime>s, advance the cursor via
                   `gralph do <cmd>`, reap own children, write the completion
                   sentinel, exit 0     (a well-behaved agent turn)

`--advance` matters for the graceful-stop proof: a gralph cursor only moves when the
agent itself runs `gralph do`, so a fake agent that merely exits would make
"cursor_advanced" untestable. The profile and instance come from the environment the
orchestrator already exports (GRALPH_PROFILE / GRALPH_INSTANCE_NAME).

The PID file is written *before* the agent starts working, so the probe never has to
guess when the tree is up.
"""
import argparse
import json
import os
import subprocess
import sys
import time

# A child that outlives its parent unless something kills the tree: exactly the
# failure mode of an orphaned MCP server.
#
# It ignores console control events on purpose. Many real MCP servers do (they are
# stdio servers, not console apps), and if the stand-in died on Ctrl-C by itself it
# would mask the very defect the sigint path is meant to detect -- the probe would
# credit gralph for a cleanup the child did to itself.
CHILD_CODE = (
    "import signal, time\n"
    "for s in ('SIGBREAK', 'SIGINT'):\n"
    "    h = getattr(signal, s, None)\n"
    "    if h is not None:\n"
    "        try:\n"
    "            signal.signal(h, signal.SIG_IGN)\n"
    "        except Exception:\n"
    "            pass\n"
    "time.sleep(900)\n"
)


def write_json_atomic(path, obj):
    os.makedirs(os.path.dirname(os.path.abspath(path)) or ".", exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(obj, f)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", choices=["hang", "complete"], required=True)
    ap.add_argument("--pidfile", required=True)
    ap.add_argument("--sentinel", default="")
    ap.add_argument("--advance", default="", help="gralph command name to `do`")
    ap.add_argument("--block", default="", help="reason to pass to `gralph block`")
    ap.add_argument("--gralph", default="", help="path to the gralph binary under test")
    ap.add_argument("--children", type=int, default=2)
    ap.add_argument("--lifetime", type=float, default=3.0)
    # The loop passes the ralph prompt positionally; accept and ignore it.
    ap.add_argument("rest", nargs="*")
    a = ap.parse_args()

    kids = []
    for _ in range(a.children):
        p = subprocess.Popen([sys.executable, "-c", CHILD_CODE],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        kids.append(p)

    write_json_atomic(a.pidfile, {
        "agent": os.getpid(),
        "children": [k.pid for k in kids],
        "mode": a.mode,
        "started": time.time(),
    })

    if a.mode == "hang":
        # Never finish. Only an external kill ends this, which is the point.
        while True:
            time.sleep(1)

    time.sleep(a.lifetime)

    advanced = False
    blocked_rc = None
    if a.gralph and (a.advance or a.block):
        prof = os.environ.get("GRALPH_PROFILE", "")
        inst = os.environ.get("GRALPH_INSTANCE_NAME", "")
        cmd = [a.gralph, "block", a.block] if a.block else [a.gralph, "do", a.advance]
        if prof:
            cmd += ["--profile", prof]
        if inst:
            cmd += ["--name", inst]
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=120)
        if a.block:
            blocked_rc = r.returncode
        else:
            advanced = r.returncode == 0
        sys.stderr.write(f"[fake_agent] {' '.join(cmd)} -> rc={r.returncode}\n")
        if r.stderr:
            sys.stderr.write(r.stderr[-2000:] + "\n")

    for k in kids:                      # a clean turn takes its children with it
        try:
            k.kill()
        except OSError:
            pass
    for k in kids:
        try:
            k.wait(timeout=10)
        except Exception:
            pass

    if a.sentinel:
        write_json_atomic(a.sentinel, {"completed": True, "advanced": advanced,
                                       "block_rc": blocked_rc, "pid": os.getpid(),
                                       "at": time.time()})
    return 0


if __name__ == "__main__":
    sys.exit(main())
