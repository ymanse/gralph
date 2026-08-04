#!/usr/bin/env python
"""Windows process aliveness + tree helpers for the lifecycle probes.
HARNESS-OWNED: do not edit.

Aliveness is decided by `tasklist`, which also gives the image name -- so a PID that
has been recycled onto a different image is reported as gone rather than as a
survivor. That is the conservative direction for a *control* run (it can only
under-report leaks, never invent them) and the strict direction for the UUT run.
"""
import subprocess
import sys
import time

CREATE_NEW_PROCESS_GROUP = 0x00000200


def image_of(pid: int):
    """Image name for a live PID, or None when the PID is not running."""
    try:
        out = subprocess.run(
            ["tasklist", "/FI", f"PID eq {int(pid)}", "/FO", "CSV", "/NH"],
            capture_output=True, text=True, timeout=30,
        ).stdout
    except (subprocess.SubprocessError, OSError):
        return None
    line = out.strip().splitlines()[0].strip() if out.strip() else ""
    # A miss prints an INFO banner rather than a CSV row.
    if not line.startswith('"'):
        return None
    parts = [p.strip('"') for p in line.split('","')]
    if len(parts) < 2 or parts[1].strip() != str(int(pid)):
        return None
    return parts[0]


def alive(pid: int) -> bool:
    return image_of(pid) is not None


def survivors(pids, expect_images=None):
    """PIDs still running, as [{'pid':N,'image':'python.exe'}].

    expect_images: when given, a PID whose image is not in this set is treated as a
    recycled PID (not a survivor).
    """
    out = []
    for pid in pids:
        img = image_of(pid)
        if img is None:
            continue
        if expect_images and img.lower() not in {i.lower() for i in expect_images}:
            continue
        out.append({"pid": int(pid), "image": img})
    return out


def wait_gone(pids, timeout_s=20.0, expect_images=None):
    """Poll until no PID survives, or the timeout expires. Returns the survivors."""
    deadline = time.time() + timeout_s
    left = survivors(pids, expect_images)
    while left and time.time() < deadline:
        time.sleep(0.4)
        left = survivors(pids, expect_images)
    return left


def hard_terminate(pid: int) -> None:
    """TerminateProcess on ONE process. Deliberately no /T -- killing the tree by
    walking it would prove nothing about whether gralph reaps its own descendants."""
    subprocess.run(["taskkill", "/F", "/PID", str(int(pid))],
                   capture_output=True, timeout=60)


def kill_tree(pid: int) -> None:
    """Cleanup only (between probe runs). Never used to produce evidence."""
    subprocess.run(["taskkill", "/F", "/T", "/PID", str(int(pid))],
                   capture_output=True, timeout=60)


if __name__ == "__main__":
    for p in sys.argv[1:]:
        print(p, image_of(int(p)))
