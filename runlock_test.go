//go:build windows

// F4-run-lock: `gralph run` must take an exclusive run-level lock on the
// instance, holding the orchestrator's PID. A second `gralph run` against a
// live instance is refused -- not queued, not raced -- with a message naming
// the holder, and it leaves `state.json` byte-identical. A lock whose holder
// PID is dead is reclaimed, so a crashed run never wedges the instance.
//
// The two halves pull against each other: reclaiming too eagerly (any lock
// file, or one past some age) turns the exclusion into a no-op and lets two
// loops drive one state dir. So the reaping is measured against a holder that
// is genuinely alive as well as one that genuinely crashed.
//
// Like the F1/F2/F3 tests these drive real `gralph run` processes and measure
// files and live PIDs rather than calling into the fix, so the package compiles
// both before and after it. Process-table helpers live in
// lifecycle_jobobject_test.go; countTurns/readAll/waitForRun in stop_test.go.
//
// Scoped by construction: every PID these tests touch is one they started.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const runLockProbeInstance = "runlockprobe"

// One stage whose agent never finishes on its own: the holding run stays parked
// in the middle of iteration 1, which makes it unambiguously live and makes
// state.json unambiguously stable while a second instance is attempted.
const runLockProbeProfile = `agent:
  command: ['cmd', '/c', '%s']
  timeout: 600s
prompt: "run lock probe"
commands:
  - name: spin
`

// One line per agent session started, then park. turns.log is how these tests
// tell "a second loop got in" from "a second loop was refused" without reading
// a single message.
const runLockProbeAgent = `@echo off
echo turn>>"%~dp0turns.log"
ping -n 300 127.0.0.1 >nul
`

const (
	// How long a run gets to reach the agent turn it is expected to start.
	runLockStartWait = 45 * time.Second
	// How long a second `gralph run` gets to refuse. A refusal is a file read
	// and a liveness check; it does not need seconds.
	runLockRefusalWait = 20 * time.Second
	// How long the live holder is watched after a refused attempt.
	runLockSettleWait = 4 * time.Second
	// How long a killed process tree gets to disappear.
	runLockReapWait = 15 * time.Second
)

// newRunLockProbe builds the binary under test and writes the probe profile and
// agent into a fresh working directory. Every run in a test shares that
// directory, so they all contend for the same instance state dir.
//
// It builds into a temp dir on purpose: bin/gralph-orch.exe is the running
// orchestrator and must never be rebuilt.
func newRunLockProbe(t *testing.T) (uut, dir string) {
	t.Helper()
	dir = t.TempDir()
	uut = filepath.Join(dir, "gralph-uut.exe")
	if out, err := exec.Command("go", "build", "-o", uut, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	agent := filepath.Join(dir, "agent.cmd")
	if err := os.WriteFile(agent, []byte(runLockProbeAgent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runLockProfilePath(dir), []byte(fmt.Sprintf(runLockProbeProfile, agent)), 0o644); err != nil {
		t.Fatal(err)
	}
	return uut, dir
}

func runLockProfilePath(dir string) string { return filepath.Join(dir, "runlock.yaml") }

func runLockStatePath(dir string) string {
	return filepath.Join(dir, ".gralph", runLockProbeInstance, "state.json")
}

// startRunLockHolder starts a `gralph run` on the probe instance and returns
// once turns.log has reached wantTurns lines -- i.e. once this run is parked
// inside an agent turn of its own. why explains what the caller was proving, so
// a run that never got in reports the reason it mattered.
func startRunLockHolder(t *testing.T, uut, dir string, wantTurns int, why string) *exec.Cmd {
	t.Helper()
	// A file, never a pipe: an agent that outlives the run would hold a pipe
	// open and make Wait() block on the leak instead of on gralph.
	logPath := filepath.Join(dir, fmt.Sprintf("run-%d.log", wantTurns))
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	cmd := exec.Command(uut, "run", runLockProfilePath(dir), "--name", runLockProbeInstance)
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gralph run: %v", err)
	}
	t.Cleanup(func() { reapRunLockProbe(t, cmd) })

	deadline := time.Now().Add(runLockStartWait)
	for time.Now().Before(deadline) {
		if countTurns(t, dir, "turns.log") >= wantTurns {
			return cmd
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("agent turn %d never started within %s (turns so far: %d). %s\nrun log: %s",
		wantTurns, runLockStartWait, countTurns(t, dir, "turns.log"), why, readAll(t, logPath))
	return nil
}

// attemptSecondRun starts a competing `gralph run` on the same instance and
// waits for it to refuse. exited is false when it was still running at the
// deadline -- meaning it was not refused, it joined.
func attemptSecondRun(t *testing.T, uut, dir, tag string) (code int, out string, exited bool) {
	t.Helper()
	logPath := filepath.Join(dir, "second-"+tag+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	cmd := exec.Command(uut, "run", runLockProfilePath(dir), "--name", runLockProbeInstance)
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the second gralph run: %v", err)
	}
	code, exited = waitForRun(t, cmd, runLockRefusalWait)
	if !exited {
		reapRunLockProbe(t, cmd)
	}
	return code, readAll(t, logPath), exited
}

// reapRunLockProbe terminates a probe run and everything under it. Used both
// for cleanup and, in the reclaim test, as the crash itself.
func reapRunLockProbe(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.Process == nil {
		return
	}
	tree := descendantsOf(t, uint32(cmd.Process.Pid))
	_ = cmd.Process.Kill()
	for pid := range tree {
		if p, err := os.FindProcess(int(pid)); err == nil {
			_ = p.Kill()
		}
	}
	waitForExit(t, tree, runLockReapWait)
}

// The acceptance case: a second `gralph run` on a live instance is refused,
// names the holder's PID, and leaves state.json byte-identical.
func TestRunLockRefusesASecondInstance(t *testing.T) {
	uut, dir := newRunLockProbe(t)
	holder := startRunLockHolder(t, uut, dir, 1,
		"the holder must own the instance before a second run is attempted")

	before := readAll(t, runLockStatePath(dir))
	if before == "" {
		t.Fatalf("no state.json at %s once the holder's turn was in flight", runLockStatePath(dir))
	}

	code, out, exited := attemptSecondRun(t, uut, dir, "refuse")
	if !exited {
		t.Fatalf("a second `gralph run` was still running %s after it started -- it was not refused, it joined "+
			"the live instance. F4: `run` must take an exclusive run-level lock on the instance state dir",
			runLockRefusalWait)
	}
	if code == 0 {
		t.Fatalf("the second `gralph run` exited 0; a refusal must be non-zero (output: %s)", out)
	}
	if pid := strconv.Itoa(holder.Process.Pid); !strings.Contains(out, pid) {
		t.Fatalf("the refusal never names the holder PID %s (output: %s). "+
			"F4: the lock records the orchestrator PID so the message can say which process to go look at",
			pid, out)
	}
	if after := readAll(t, runLockStatePath(dir)); after != before {
		t.Fatalf("state.json changed across a refused run:\nbefore: %s\nafter:  %s\n"+
			"F4: a run that is refused must touch nothing -- not the cursor, not the session id",
			before, after)
	}
}

// The other half of the contract: a crashed holder must not wedge the instance.
// The holder is killed with TerminateProcess, so it runs no Go code on the way
// out and nothing it deferred can release the lock -- exactly the crash the
// reaping exists for. The refusal is asserted first: reclaiming a dead holder's
// lock only means something if a live one is actually held against.
func TestRunLockReclaimsADeadHoldersLock(t *testing.T) {
	uut, dir := newRunLockProbe(t)
	holder := startRunLockHolder(t, uut, dir, 1,
		"the holder must own the instance before it is crashed")

	if _, out, exited := attemptSecondRun(t, uut, dir, "live"); !exited {
		t.Fatalf("a second `gralph run` was not refused while the holder was alive (output: %s). "+
			"F4: exclusion has to work before reclaiming can be tested", out)
	}

	reapRunLockProbe(t, holder)

	startRunLockHolder(t, uut, dir, 2,
		"F4: the holder's lock was left behind by a TerminateProcess -- a lock whose PID is dead must be "+
			"reclaimed automatically, or one crash wedges the instance forever")
}

// The failure direction: a lock whose holder is ALIVE must not be reaped. A fix
// that reclaimed on any lock file it found -- or on the lock's age -- would pass
// the reclaim test above while letting two loops drive the same state dir, which
// is the race the lock exists to prevent. So the refused run must leave the
// holder running, its agent tree intact, and no second turn started.
func TestRunLockDoesNotReapALiveHolder(t *testing.T) {
	uut, dir := newRunLockProbe(t)
	holder := startRunLockHolder(t, uut, dir, 1,
		"the holder must be mid-turn so the lock it holds is provably live")
	tree := descendantsOf(t, uint32(holder.Process.Pid))
	if len(tree) < 2 {
		t.Fatalf("the holder has %d descendant(s); the launcher hop and the agent must both be up so a "+
			"stolen lock would be visible as a disturbed tree", len(tree))
	}

	if _, out, exited := attemptSecondRun(t, uut, dir, "alive"); !exited {
		t.Fatalf("a second `gralph run` was still running %s after it started (output: %s). "+
			"F4: a live holder's lock must be refused, never reclaimed", runLockRefusalWait, out)
	}

	time.Sleep(runLockSettleWait)
	if alive := stillAlive(t, tree); len(alive) != len(tree) {
		t.Fatalf("only %d of %d process(es) in the live holder's tree survived %s after the refused attempt "+
			"(alive: %v, tree: %v). F4: a refused run must not evict the holder it lost to",
			len(alive), len(tree), runLockSettleWait, alive, tree)
	}
	if _, exited := waitForRun(t, holder, time.Second); exited {
		t.Fatalf("the holding `gralph run` exited while a second instance was attempted. " +
			"F4: the loser of the lock is the second run, never the holder")
	}
	if turns := countTurns(t, dir, "turns.log"); turns != 1 {
		t.Fatalf("%d agent turn(s) have started on this instance, want 1. "+
			"F4: the refused run took the lock off a live holder and started a competing loop -- "+
			"only a DEAD holder's lock may be reclaimed", turns)
	}
}
