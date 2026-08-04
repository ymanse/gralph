//go:build windows

// F2-treekill: both `cmd.Cancel` bodies (launcher.go, galp_subprocess.go) must
// kill the process TREE, not one process.
//
// Measured tree under `gralph run` with `cmd /c ping` as the agent:
//
//	gralph run -> gralph __galp-subprocess -> cmd.exe -> PING.EXE
//
// The launcher's context deadline fires at agent.timeout and runs
// galp_subprocess.go's cmd.Cancel. Process.Signal(syscall.SIGTERM) ALWAYS
// errors on Windows, so the Kill() fallback is the only path ever taken -- and
// it terminates cmd.exe alone, orphaning PING.EXE.
//
// gralph itself stays alive across this path, so F1's job object cannot mask
// the leak: a job with KILL_ON_JOB_CLOSE only reaps when the job handle closes.
// These tests therefore measure the agent-timeout path specifically.
//
// Like the F1 tests, these drive a real `gralph run` and measure live PIDs
// instead of calling into the fix, so the package compiles both before and
// after it. The process-table helpers live in lifecycle_jobobject_test.go.
//
// Scoped by construction: every PID these tests touch is one they started.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `ping -n 300` is the smallest always-present Windows program that just waits,
// and running it under `cmd /c` is what gives the agent a child of its own --
// the grandchild that a single-process Kill() leaves behind.
const treeKillProfile = `agent:
  command: ["cmd", "/c", "ping", "-n", "300", "127.0.0.1"]
  timeout: 8s
prompt: "tree kill probe"
commands:
  - name: spin
`

const (
	// treeKillAgentTimeout must match the `timeout:` in treeKillProfile.
	treeKillAgentTimeout = 8 * time.Second
	treeKillSpawnWait    = 30 * time.Second
	// Counted from when the tree is up, so it has to cover the rest of the
	// agent timeout plus the launcher's kill grace.
	treeKillReapWait = treeKillAgentTimeout + 20*time.Second
)

// hasImage reports whether any process in the tree runs the named image.
func hasImage(tree map[uint32]string, image string) bool {
	for _, exe := range tree {
		if strings.EqualFold(exe, image) {
			return true
		}
	}
	return false
}

// startTreeKillRun builds the binary under test, starts `gralph run` on the
// timing-out profile and returns it once the whole tree -- launcher hop, agent,
// and the agent's own child -- is up. The caller waits for the timeout; that is
// the measurement.
//
// It builds into a temp dir on purpose: bin/gralph-orch.exe is the running
// orchestrator and must never be rebuilt.
func startTreeKillRun(t *testing.T) (*exec.Cmd, map[uint32]string) {
	t.Helper()
	dir := t.TempDir()
	uut := filepath.Join(dir, "gralph-uut.exe")
	if out, err := exec.Command("go", "build", "-o", uut, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	profile := filepath.Join(dir, "spin.yaml")
	if err := os.WriteFile(profile, []byte(treeKillProfile), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file, never a pipe: the orphaned grandchild inherits the handle and
	// holds it open, so a pipe would make Wait() block on the leak instead of
	// on gralph -- hiding the very thing under test.
	logFile, err := os.Create(filepath.Join(dir, "run.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	cmd := exec.Command(uut, "run", profile, "--name", "treekill", "--max-iterations", "1")
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gralph run: %v", err)
	}
	root := uint32(cmd.Process.Pid)

	tree := map[uint32]string{}
	deadline := time.Now().Add(treeKillSpawnWait)
	for time.Now().Before(deadline) {
		// The grandchild is the whole point, so wait for the full depth rather
		// than for "something started".
		if tree = descendantsOf(t, root); len(tree) >= 3 && hasImage(tree, "PING.EXE") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		for pid := range tree {
			if p, err := os.FindProcess(int(pid)); err == nil {
				_ = p.Kill()
			}
		}
	})
	if len(tree) < 3 || !hasImage(tree, "PING.EXE") {
		t.Fatalf("`gralph run` tree is %v after %s; the launcher hop, the agent and the agent's own child must all be up before the agent times out",
			tree, treeKillSpawnWait)
	}
	return cmd, tree
}

// The acceptance case: the agent times out and its whole subtree goes with it.
func TestKillTreeReapsGrandchildrenOnAgentTimeout(t *testing.T) {
	_, tree := startTreeKillRun(t)

	if survivors := waitForExit(t, tree, treeKillReapWait); len(survivors) != 0 {
		t.Fatalf("%d of %d process(es) in the agent tree survived the agent timeout: %v (tree: %v). "+
			"F2: cmd.Cancel must kill the process TREE -- a bare Kill() terminates the agent alone and orphans every grandchild",
			len(survivors), len(tree), survivors, tree)
	}
}

// The failure direction, in two parts:
//
//   - the tree kill must stay scoped to processes gralph started, so a
//     same-image process this test owns must be left alone (a "fix" reaching
//     for `taskkill /F /IM` would pass the test above and still take out
//     another harness's agents on the same machine);
//   - killing an already-exited or unknown pid must not error out the cancel
//     path, so the run still reports a timed-out session and exits 0.
func TestKillTreeSparesProcessesItDidNotStart(t *testing.T) {
	bystander := exec.Command("ping", "-n", "300", "127.0.0.1")
	if err := bystander.Start(); err != nil {
		t.Fatalf("start bystander: %v", err)
	}
	t.Cleanup(func() {
		_ = bystander.Process.Kill()
		_ = bystander.Wait()
	})
	outsider := uint32(bystander.Process.Pid)

	cmd, tree := startTreeKillRun(t)

	if err := cmd.Wait(); err != nil {
		t.Fatalf("`gralph run` exited with %v after the agent timeout; the tree kill must not fail the cancel path "+
			"-- an already-exited or unknown pid is not an error", err)
	}
	if survivors := waitForExit(t, tree, treeKillReapWait); len(survivors) != 0 {
		t.Fatalf("%d of %d process(es) in the agent tree survived the agent timeout: %v (tree: %v). "+
			"F2: cmd.Cancel must kill the process TREE",
			len(survivors), len(tree), survivors, tree)
	}
	if _, ok := procSnapshot(t)[outsider]; !ok {
		t.Fatalf("the bystander ping (pid %d), which gralph never started, was killed too -- "+
			"the tree kill must be scoped to the agent's own descendants, never a blanket kill by image name",
			outsider)
	}
}
