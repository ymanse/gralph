//go:build windows

// F1-jobobject: `gralph run` must join a Windows job object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so that killing gralph *alone*
// (TerminateProcess -- `taskkill /F /PID` with no `/T`) leaves no descendants.
//
// The property only exists at the OS level: TerminateProcess runs no Go code in
// the target, so no amount of deferred cleanup can reap the tree in that path.
// That is why these tests drive a real `gralph run` and measure live PIDs
// instead of calling into the fix -- they reference nothing the fix introduces,
// so the package compiles both before and after it.
//
// Scoped by construction: every PID these tests touch is one they started
// themselves (other harnesses run their own gralph instances on this machine).

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// An agent that never exits on its own: the tree must still be up at the moment
// gralph is terminated. `ping -n 300 127.0.0.1` is the smallest always-present
// Windows program that simply waits.
const jobObjectProfile = `agent:
  command: ["ping", "-n", "300", "127.0.0.1"]
prompt: "job object probe"
commands:
  - name: spin
`

// jobObjectSpawnWait is how long the launcher hop and the agent get to come up,
// jobObjectReapWait how long the OS gets to reap the tree after the kill.
const (
	jobObjectSpawnWait = 30 * time.Second
	jobObjectReapWait  = 15 * time.Second
)

type procInfo struct {
	ppid uint32
	exe  string
}

// procSnapshot is one instant of the live process table (pid -> parent, image).
func procSnapshot(t *testing.T) map[uint32]procInfo {
	t.Helper()
	h, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatalf("process snapshot: %v", err)
	}
	defer syscall.CloseHandle(h)

	out := map[uint32]procInfo{}
	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = syscall.Process32First(h, &e); err == nil; err = syscall.Process32Next(h, &e) {
		out[e.ProcessID] = procInfo{
			ppid: e.ParentProcessID,
			exe:  syscall.UTF16ToString(e.ExeFile[:]),
		}
	}
	return out
}

// descendantsOf returns every transitive child of root (pid -> image name).
func descendantsOf(t *testing.T, root uint32) map[uint32]string {
	t.Helper()
	snap := procSnapshot(t)
	out := map[uint32]string{}
	for grew := true; grew; {
		grew = false
		for pid, info := range snap {
			if pid == root || pid == 0 {
				continue
			}
			if _, known := out[pid]; known {
				continue
			}
			_, parentIsOurs := out[info.ppid]
			if info.ppid == root || parentIsOurs {
				out[pid] = info.exe
				grew = true
			}
		}
	}
	return out
}

// stillAlive reports which of pids are still running. The image name is matched
// too, so a recycled PID does not read as a survivor.
func stillAlive(t *testing.T, pids map[uint32]string) []uint32 {
	t.Helper()
	snap := procSnapshot(t)
	var alive []uint32
	for pid, exe := range pids {
		if info, ok := snap[pid]; ok && info.exe == exe {
			alive = append(alive, pid)
		}
	}
	sort.Slice(alive, func(i, j int) bool { return alive[i] < alive[j] })
	return alive
}

func waitForExit(t *testing.T, pids map[uint32]string, within time.Duration) []uint32 {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		alive := stillAlive(t, pids)
		if len(alive) == 0 || time.Now().After(deadline) {
			return alive
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// startJobObjectRun builds the binary under test, starts `gralph run` on the
// hanging profile and returns it once its tree is up. The caller does the
// terminating -- that is the measurement.
//
// It builds into a temp dir on purpose: bin/gralph-orch.exe is the running
// orchestrator and must never be rebuilt.
func startJobObjectRun(t *testing.T) (*exec.Cmd, map[uint32]string) {
	t.Helper()
	dir := t.TempDir()
	uut := filepath.Join(dir, "gralph-uut.exe")
	if out, err := exec.Command("go", "build", "-o", uut, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	profile := filepath.Join(dir, "spin.yaml")
	if err := os.WriteFile(profile, []byte(jobObjectProfile), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(uut, "run", profile, "--name", "jobobject", "--max-iterations", "1")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gralph run: %v", err)
	}
	root := uint32(cmd.Process.Pid)

	kids := map[uint32]string{}
	deadline := time.Now().Add(jobObjectSpawnWait)
	for time.Now().Before(deadline) {
		// The default GALP launcher is gralph re-invoking itself, so a live tree
		// is at least the launcher hop plus the agent.
		if kids = descendantsOf(t, root); len(kids) >= 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		for pid := range kids {
			if p, err := os.FindProcess(int(pid)); err == nil {
				_ = p.Kill()
			}
		}
	})
	if len(kids) < 2 {
		t.Fatalf("`gralph run` has %d descendant(s) after %s; the launcher hop and the agent must both be up before gralph is terminated",
			len(kids), jobObjectSpawnWait)
	}
	return cmd, kids
}

// The acceptance case: gralph is killed on its own and takes its tree with it.
func TestJobObjectReapsDescendantsOnTerminate(t *testing.T) {
	cmd, kids := startJobObjectRun(t)

	// Deliberately NOT a tree kill: `/T` would walk the tree itself and prove
	// nothing about gralph. Only a job object can explain the tree dying here.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("terminate gralph: %v", err)
	}
	_ = cmd.Wait()

	if survivors := waitForExit(t, kids, jobObjectReapWait); len(survivors) != 0 {
		t.Fatalf("%d of %d descendant process(es) survived a TerminateProcess on gralph alone: %v (tree: %v). "+
			"F1: `run` must assign itself to a job object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so the OS reaps the tree",
			len(survivors), len(kids), survivors, kids)
	}
}

// The failure direction: the reap must be scoped to the tree gralph started.
// A "fix" that blanket-killed by image name (taskkill /IM) would satisfy the
// test above while killing another harness's processes on the same machine, so
// an unrelated process of the *same image* as the agent must be left alone.
func TestJobObjectDoesNotReapProcessesItDidNotStart(t *testing.T) {
	bystander := exec.Command("ping", "-n", "300", "127.0.0.1")
	if err := bystander.Start(); err != nil {
		t.Fatalf("start bystander: %v", err)
	}
	t.Cleanup(func() {
		_ = bystander.Process.Kill()
		_ = bystander.Wait()
	})
	outsider := uint32(bystander.Process.Pid)

	cmd, kids := startJobObjectRun(t)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("terminate gralph: %v", err)
	}
	_ = cmd.Wait()

	if survivors := waitForExit(t, kids, jobObjectReapWait); len(survivors) != 0 {
		t.Fatalf("%d of %d descendant process(es) survived a TerminateProcess on gralph alone: %v (tree: %v). "+
			"F1: the job object must reap gralph's own tree",
			len(survivors), len(kids), survivors, kids)
	}
	if _, ok := procSnapshot(t)[outsider]; !ok {
		t.Fatalf("the bystander %s process (pid %d), which gralph never started, was killed too -- "+
			"the reap must be scoped to gralph's job, never a blanket kill by image name",
			bystander.Path, outsider)
	}
}
