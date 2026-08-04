#!/usr/bin/env python
"""Live proof of the shutdown contract (spec/shutdown-contract.md).
HARNESS-OWNED: do not edit.

Three measurements, each against the real binary under test:

  graceful   stop is requested WHILE an agent turn is in flight. The contract says
             that turn must finish, its gate must commit, the cursor must advance,
             the next stage must NOT start, and the exit code must be 0.
  lock       a second `gralph run` on a live instance must be refused without
             touching state.json; and a lock whose holder died must be reaped so a
             crash cannot wedge the instance forever.
  blocked    `gralph block <reason>` must end the loop with the blocked exit code
             instead of respawning the same command.

Usage:
  python scripts/proof_shutdown.py --uut bin/gralph-uut.exe \
      --out artifacts/shutdown_proof.json
"""
import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import proc  # noqa: E402

HARNESS = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PIDFILE = os.path.join(HARNESS, "artifacts", "probe", "agent_pids.json")
SENTINEL = os.path.join(HARNESS, "artifacts", "probe", "agent_done.json")
EXPECT = ["python.exe", "python3.exe", "pythonw.exe"]

BLOCKED_EXIT = 3          # per spec/shutdown-contract.md
GRACEFUL_EXIT = 0


def clean(instance):
    shutil.rmtree(os.path.join(HARNESS, ".gralph", instance), ignore_errors=True)
    for f in (PIDFILE, PIDFILE + ".tmp", SENTINEL, SENTINEL + ".tmp"):
        try:
            os.remove(f)
        except OSError:
            pass


def start(binary, profile, instance, iters="6"):
    return subprocess.Popen(
        [binary, "run", profile, "--name", instance, "--max-iterations", iters],
        cwd=HARNESS, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
        creationflags=proc.CREATE_NEW_PROCESS_GROUP,
    )


def state_of(instance):
    p = os.path.join(HARNESS, ".gralph", instance, "state.json")
    try:
        raw = open(p, "rb").read()
    except OSError:
        return None, None
    try:
        return json.loads(raw.decode("utf-8")), hashlib.sha256(raw).hexdigest()
    except ValueError:
        return None, hashlib.sha256(raw).hexdigest()


def await_tree(timeout_s=90.0):
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            d = json.load(open(PIDFILE, encoding="utf-8"))
        except (OSError, ValueError):
            time.sleep(0.3)
            continue
        pids = [int(d["agent"])] + [int(p) for p in d.get("children", [])]
        if len(pids) >= 2 and all(proc.alive(p) for p in pids):
            return d, pids
        time.sleep(0.3)
    return None, []


def measure_graceful(binary):
    inst = "probe-complete"
    rec: dict = {"name": "graceful", "supported": True}
    r = subprocess.run([binary, "help"], cwd=HARNESS, capture_output=True,
                       text=True, timeout=60)
    if "gralph stop" not in (r.stdout + r.stderr):
        return {**rec, "supported": False, "note": "no `stop` subcommand (pre-fix)"}

    clean(inst)
    g = start(binary, "probe-complete.yaml", inst)
    try:
        first, pids = await_tree()
        if not first:
            return {**rec, "note": "agent tree never came up", "agent_completed": False}
        rec["descendants_spawned"] = len(pids)

        # Request the stop while the turn is genuinely in flight.
        s = subprocess.run([binary, "stop", "probe-complete.yaml", "--name", inst],
                           cwd=HARNESS, capture_output=True, text=True, timeout=120)
        rec["stop_exit_code"] = s.returncode
        rec["requested_at_agent_pid"] = first["agent"]

        try:
            g.wait(timeout=180)
        except subprocess.TimeoutExpired:
            proc.kill_tree(g.pid)
            return {**rec, "note": "loop did not exit after graceful stop",
                    "agent_completed": False, "graceful_exit_code": None}

        rec["graceful_exit_code"] = g.returncode
        st, _ = state_of(inst)
        rec["cursor_after"] = (st or {}).get("cursor")
        rec["cursor_advanced"] = rec["cursor_after"] == "step-two"

        done = {}
        try:
            done = json.load(open(SENTINEL, encoding="utf-8"))
        except (OSError, ValueError):
            pass
        rec["agent_completed"] = bool(done.get("completed"))
        rec["agent_advanced_cursor"] = bool(done.get("advanced"))
        # The turn that finished must be the one that was in flight when we asked to
        # stop -- otherwise the loop started a fresh stage after the stop request.
        rec["no_extra_turn_started"] = done.get("pid") == first["agent"]
        rec["survivors"] = proc.wait_gone(pids, 25.0, EXPECT)
        return rec
    finally:
        proc.kill_tree(g.pid)
        clean(inst)


def measure_lock(binary):
    inst = "probe-hang"
    rec: dict = {"name": "lock"}
    clean(inst)
    g1 = start(binary, "probe-hang.yaml", inst, iters="40")
    try:
        _, pids = await_tree()
        if not pids:
            return {**rec, "note": "first instance never came up",
                    "second_instance_rejected": False}

        _, sha_before = state_of(inst)
        g2 = subprocess.run([binary, "run", "probe-hang.yaml", "--name", inst,
                             "--max-iterations", "2"],
                            cwd=HARNESS, capture_output=True, text=True, timeout=180)
        rec["second_exit_code"] = g2.returncode
        rec["second_instance_rejected"] = g2.returncode != 0
        msg = (g2.stdout or "") + (g2.stderr or "")
        rec["second_message"] = msg.strip()[-400:]
        rec["second_names_holder_pid"] = str(g1.pid) in msg

        _, sha_after = state_of(inst)
        rec["state_sha_unchanged"] = (sha_before is not None
                                      and sha_before == sha_after)

        # Stale-lock reap: kill the holder hard, then a fresh run must acquire.
        proc.hard_terminate(g1.pid)
        proc.wait_gone([g1.pid], 20.0)
        time.sleep(1.0)
        g3 = start(binary, "probe-hang.yaml", inst, iters="2")
        try:
            _, pids3 = await_tree(timeout_s=90)
            rec["stale_lock_reaped"] = bool(pids3)
        finally:
            proc.kill_tree(g3.pid)
        return rec
    finally:
        proc.kill_tree(g1.pid)
        clean(inst)


def measure_blocked(binary):
    inst = "probe-block"
    rec: dict = {"name": "blocked"}
    r = subprocess.run([binary, "help"], cwd=HARNESS, capture_output=True,
                       text=True, timeout=60)
    if "gralph block" not in (r.stdout + r.stderr):
        return {**rec, "supported": False, "note": "no `block` subcommand (pre-fix)"}
    rec["supported"] = True

    clean(inst)
    g = start(binary, "probe-block.yaml", inst, iters="6")
    try:
        try:
            out, _ = g.communicate(timeout=240)
        except subprocess.TimeoutExpired:
            proc.kill_tree(g.pid)
            return {**rec, "blocked_exit_code": None, "respawn_count": None,
                    "note": "loop never exited after `gralph block`"}
        rec["blocked_exit_code"] = g.returncode
        # One session start per respawn; blocking must stop after the first.
        rec["respawn_count"] = max(0, (out or "").count("[gralph] iteration") - 1)
        rec["tail"] = (out or "").strip()[-400:]
        return rec
    finally:
        proc.kill_tree(g.pid)
        clean(inst)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--uut", default="bin/gralph-uut.exe")
    ap.add_argument("--out", default="artifacts/shutdown_proof.json")
    a = ap.parse_args()

    uut = os.path.join(HARNESS, a.uut)
    if not os.path.exists(uut):
        print(f"missing binary: {uut}", file=sys.stderr)
        return 2
    os.makedirs(os.path.join(HARNESS, "artifacts", "probe"), exist_ok=True)

    graceful = measure_graceful(uut)
    lock = measure_lock(uut)
    blocked = measure_blocked(uut)

    out = {
        "proof_source": "live-orchestrator-run",
        "os": "windows",
        "uut_binary": a.uut,
        "graceful": graceful,
        "lock": lock,
        "blocked": blocked,
        # flattened for the gate's substring checks
        "agent_completed": bool(graceful.get("agent_completed")),
        "cursor_advanced": bool(graceful.get("cursor_advanced")),
        "no_extra_turn_started": bool(graceful.get("no_extra_turn_started")),
        "graceful_exit_code": graceful.get("graceful_exit_code"),
        "graceful_survivors": graceful.get("survivors", []),
        "descendants_spawned": graceful.get("descendants_spawned", 0),
        "second_instance_rejected": bool(lock.get("second_instance_rejected")),
        "second_names_holder_pid": bool(lock.get("second_names_holder_pid")),
        "state_sha_unchanged": bool(lock.get("state_sha_unchanged")),
        "stale_lock_reaped": bool(lock.get("stale_lock_reaped")),
        "blocked_exit_code": blocked.get("blocked_exit_code"),
        "respawn_count": blocked.get("respawn_count"),
        "expected_blocked_exit": BLOCKED_EXIT,
        "expected_graceful_exit": GRACEFUL_EXIT,
    }
    dest = os.path.join(HARNESS, a.out)
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    with open(dest, "w", encoding="utf-8") as f:
        json.dump(out, f, indent=2)

    print(f"graceful_exit={out['graceful_exit_code']} cursor_advanced="
          f"{out['cursor_advanced']} lock_rejected={out['second_instance_rejected']} "
          f"blocked_exit={out['blocked_exit_code']} respawns={out['respawn_count']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
