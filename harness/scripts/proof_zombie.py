#!/usr/bin/env python
"""Live process-tree proof for the gralph lifecycle fix. HARNESS-OWNED: do not edit.

Runs a real gralph orchestrator against a fake agent that owns long-lived children,
terminates it by each supported path, and reports which of those PIDs are still alive
afterwards. This is the referee for the `zombie-proof` gate.

Positive control (the reason this probe can be trusted)
-------------------------------------------------------
Every run also exercises the SAME paths against the pinned pre-fix binary
(`bin/gralph-orch.exe`). A probe that always reports `survivors: []` would be
indistinguishable from a correct fix, so the gate additionally requires the control
run to LEAK. If the control stops leaking, the probe -- not gralph -- is broken.

Usage:
  python scripts/proof_zombie.py --uut bin/gralph-uut.exe \
      --control bin/gralph-orch.exe --out artifacts/zombie_proof.json
"""
import argparse
import json
import os
import shutil
import signal
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import proc  # noqa: E402

HARNESS = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PIDFILE = os.path.join(HARNESS, "artifacts", "probe", "agent_pids.json")
# The stand-in agent and its children are python; a recycled PID landing on a
# different image is not a survivor.
EXPECT = ["python.exe", "python3.exe", "pythonw.exe"]

# path -> (probe profile, instance name)
PATHS = {
    "agent_timeout":    ("probe-timeout.yaml", "probe-timeout"),
    "sigint_double":    ("probe-hang.yaml", "probe-hang"),
    "terminate_gralph": ("probe-hang.yaml", "probe-hang"),
    "graceful_stop":    ("probe-hang.yaml", "probe-hang"),
}


def clean_instance(instance):
    shutil.rmtree(os.path.join(HARNESS, ".gralph", instance), ignore_errors=True)
    for f in (PIDFILE, PIDFILE + ".tmp"):
        try:
            os.remove(f)
        except OSError:
            pass


def start(binary, profile, instance):
    """Launch gralph in its own process group so CTRL_BREAK can be targeted at it."""
    return subprocess.Popen(
        [binary, "run", profile, "--name", instance, "--max-iterations", "40"],
        cwd=HARNESS,
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        creationflags=proc.CREATE_NEW_PROCESS_GROUP,
    )


def await_tree(timeout_s=90.0):
    """Wait until the fake agent has published its PIDs and they are all running."""
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            with open(PIDFILE, encoding="utf-8") as f:
                d = json.load(f)
        except (OSError, ValueError):
            time.sleep(0.3)
            continue
        pids = [int(d["agent"])] + [int(p) for p in d.get("children", [])]
        if len(pids) >= 2 and all(proc.alive(p) for p in pids):
            return pids
        time.sleep(0.3)
    return []


def supports_stop(binary):
    r = subprocess.run([binary, "help"], cwd=HARNESS, capture_output=True,
                       text=True, timeout=60)
    return "gralph stop" in (r.stdout + r.stderr)


def run_path(binary, path, wait_s):
    """One (binary, termination path) measurement."""
    profile, instance = PATHS[path]
    rec = {"path": path, "profile": profile, "descendants_spawned": 0,
           "survivors": [], "supported": True, "note": ""}

    if path == "graceful_stop" and not supports_stop(binary):
        rec.update(supported=False, note="binary has no `stop` subcommand (pre-fix)")
        return rec

    clean_instance(instance)
    g = start(binary, profile, instance)
    try:
        pids = await_tree()
        rec["descendants_spawned"] = len(pids)
        if not pids:
            rec["note"] = "fake agent never published a live tree"
            return rec

        if path == "agent_timeout":
            # Nothing to send: the launcher's own GALP_TIMEOUT_MS cancels the agent
            # while gralph keeps looping. The tree must be gone anyway.
            pass
        elif path == "sigint_double":
            os.kill(g.pid, signal.CTRL_BREAK_EVENT)
            time.sleep(1.5)
            try:
                os.kill(g.pid, signal.CTRL_BREAK_EVENT)
            except OSError:
                pass
        elif path == "terminate_gralph":
            # No /T. Killing gralph ALONE and finding the tree gone can only be
            # explained by an OS-level job object -- that is the whole proof.
            proc.hard_terminate(g.pid)
        elif path == "graceful_stop":
            subprocess.run([binary, "stop", PATHS[path][0], "--name", instance,
                            "--now"], cwd=HARNESS, capture_output=True, timeout=120)

        rec["survivors"] = proc.wait_gone(pids, timeout_s=wait_s, expect_images=EXPECT)
        rec["gralph_exit_code"] = g.poll()
        return rec
    finally:
        proc.kill_tree(g.pid)
        try:
            g.wait(timeout=30)
        except Exception:
            pass
        # Never let one measurement's leftovers pollute the next one. This cleanup
        # runs AFTER survivors were recorded, so it cannot hide a leak.
        for s in rec["survivors"]:
            proc.kill_tree(s["pid"])
        # ...and the recorded set is not the whole story on the CONTROL binary: after
        # its agent is timed out, gralph retries and spawns a FRESH agent, which orphans
        # another batch that was never in `survivors`. Re-read the pidfile (the newest
        # agent overwrote it) and reap that tree too, or every control run dribbles a
        # couple of sleepers onto the machine -- which is a poor look for a harness whose
        # entire subject is leaked processes.
        try:
            with open(PIDFILE, encoding="utf-8") as f:
                last = json.load(f)
            for pid in [last.get("agent")] + list(last.get("children") or []):
                if pid:
                    proc.kill_tree(int(pid))
        except (OSError, ValueError):
            pass
        clean_instance(instance)


def run_binary(binary, paths, wait_s):
    return [run_path(binary, p, wait_s) for p in paths]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--uut", default="bin/gralph-uut.exe")
    ap.add_argument("--control", default="bin/gralph-orch.exe")
    ap.add_argument("--out", default="artifacts/zombie_proof.json")
    ap.add_argument("--wait", type=float, default=25.0)
    a = ap.parse_args()

    uut = os.path.join(HARNESS, a.uut)
    ctl = os.path.join(HARNESS, a.control)
    for p in (uut, ctl):
        if not os.path.exists(p):
            print(f"missing binary: {p}", file=sys.stderr)
            return 2

    os.makedirs(os.path.join(HARNESS, "artifacts", "probe"), exist_ok=True)

    uut_paths = ["agent_timeout", "sigint_double", "terminate_gralph", "graceful_stop"]
    # The pre-fix binary has no `stop`; its job is only to show the probe sees leaks.
    ctl_paths = ["agent_timeout", "sigint_double", "terminate_gralph"]

    uut_recs = run_binary(uut, uut_paths, a.wait)
    ctl_recs = run_binary(ctl, ctl_paths, a.wait)

    uut_live = [r for r in uut_recs if r["supported"]]
    spawned = [r["descendants_spawned"] for r in uut_live]
    all_surv = [s for r in uut_live for s in r["survivors"]]
    ctl_surv = sum(len(r["survivors"]) for r in ctl_recs)

    out = {
        "proof_source": "live-pid-snapshot",
        "os": "windows",
        "uut_binary": a.uut,
        "control_binary": a.control,
        "uut_paths_tested": len(uut_live),
        "control_paths_tested": len(ctl_recs),
        "descendants_spawned_min": min(spawned) if spawned else 0,
        "survivors": all_surv,
        "control_survivors_total": ctl_surv,
        "control_leaked": ctl_surv > 0,
        "uut": uut_recs,
        "control": ctl_recs,
    }
    dest = os.path.join(HARNESS, a.out)
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    with open(dest, "w", encoding="utf-8") as f:
        json.dump(out, f, indent=2)

    print(f"uut_paths={len(uut_live)} spawned_min={out['descendants_spawned_min']} "
          f"survivors={len(all_surv)} control_survivors={ctl_surv}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
