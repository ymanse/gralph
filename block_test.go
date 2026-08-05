//go:build windows

// F5-block-verb: `gralph block "<reason>"` from inside an agent session records a
// TERMINAL blocked state. The loop stops respawning the stage and exits 3 -- a
// code of its own, distinct from DONE/stopped (0) and from failed (1).
//
// The failure this closes is recorded in spec/defect-analysis.md §D5: a stage
// that needs a human decision has no way to say so, so the agent re-asks the
// same question every session and burns the whole iteration budget on one
// cursor (measured: 10+ sessions).
//
// Both halves are measured on real `gralph run` processes -- exit codes and the
// count of agent turns actually started -- rather than by calling into the fix,
// so the package compiles both before and after it. countTurns/readAll/
// waitForRun live in stop_test.go; process helpers in
// lifecycle_jobobject_test.go.
//
// Scoped by construction: every PID these tests touch is one they started.

package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const blockProbeInstance = "blockprobe"

// The reason a human has to act on. It must survive into the terminal state:
// "blocked" with no reason is a dead loop nobody can unblock.
const blockProbeReason = "probe: a human must choose the migration order"

// One stage, and its gate can never be satisfied. That is what lets the two
// tests below differ in exactly one variable: the verb the agent runs. Same
// profile, same failing gate -- `do` is retryable, `block` is terminal.
const blockProbeProfile = `agent:
  command: ['cmd', '/c', '%s']
  timeout: 180s
prompt: "block verb probe"
commands:
  - name: needs-human
    lua: fail.lua
    guidance: |
      A stage the agent cannot satisfy on its own.
      RUN: gralph block "<reason>"
`

// The gate the agent cannot pass, no matter how many sessions it spends.
const blockProbeGate = `gralph.fail("reason: only a human can decide this; the agent cannot satisfy this gate")
`

// The blocking turn: announce the turn, block, announce a clean finish. The
// trailing echo is deliberate -- it makes the agent session exit 0 whatever
// `block` itself returns, so "the loop stopped" can only be the blocked state's
// doing and never a crash-loop giving up.
const blockProbeBlockAgent = `@echo off
echo turn>>"%%~dp0turns.log"
"%s" block "%s" --profile "%s" --name ` + blockProbeInstance + `
echo turn>>"%%~dp0done.log"
`

// The failing turn: run the gate and let its non-zero exit be the session's.
const blockProbeGateAgent = `@echo off
echo turn>>"%%~dp0turns.log"
"%s" do needs-human --profile "%s" --name ` + blockProbeInstance + `
`

const (
	// How long a blocked run gets to notice and exit. One agent turn plus the
	// launcher hop.
	blockProbeExitWait = 120 * time.Second
	// How long a crash-looping run gets to spend its whole failure budget:
	// MaxConsecutiveAgentFailures turns plus the exponential backoff between
	// them.
	blockProbeBudgetWait = 240 * time.Second
	// Iteration budget for the blocked runs. Bounded on purpose: the claim is
	// "the stage is not respawned", and a loop that had room for four
	// iterations and used one states that far more sharply than a loop that
	// had nowhere else to go.
	blockProbeMaxIterations = 4
)

// newBlockProbe builds the binary under test and writes the probe profile and
// its unsatisfiable gate into a fresh working directory. The agent script is
// written separately: the tests swap it to change the verb under measurement.
//
// It builds into a temp dir on purpose: bin/gralph-orch.exe is the running
// orchestrator and must never be rebuilt.
func newBlockProbe(t *testing.T) (uut, dir string) {
	t.Helper()
	dir = t.TempDir()
	uut = filepath.Join(dir, "gralph-uut.exe")
	if out, err := exec.Command("go", "build", "-o", uut, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "fail.lua"), []byte(blockProbeGate), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(dir, "agent.cmd")
	if err := os.WriteFile(blockProbeProfilePath(dir), []byte(fmt.Sprintf(blockProbeProfile, agent)), 0o644); err != nil {
		t.Fatal(err)
	}
	return uut, dir
}

func blockProbeProfilePath(dir string) string { return filepath.Join(dir, "block.yaml") }

// writeBlockProbeAgent installs the agent turn the next run will execute.
func writeBlockProbeAgent(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "agent.cmd"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func blockProbeBlocksAgent(uut, dir string) string {
	return fmt.Sprintf(blockProbeBlockAgent, uut, blockProbeReason, blockProbeProfilePath(dir))
}

func blockProbeFailsAgent(uut, dir string) string {
	return fmt.Sprintf(blockProbeGateAgent, uut, blockProbeProfilePath(dir))
}

// startBlockProbeRun starts a `gralph run` on the probe instance. maxIterations
// of 0 means unlimited. tag names the run's log file so a failure can point at
// the right one.
func startBlockProbeRun(t *testing.T, uut, dir, tag string, maxIterations int) (cmd *exec.Cmd, logPath string) {
	t.Helper()
	// A file, never a pipe: an agent that outlives the run would hold a pipe
	// open and make Wait() block on the leak instead of on gralph.
	logPath = filepath.Join(dir, "run-"+tag+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	args := []string{"run", blockProbeProfilePath(dir), "--name", blockProbeInstance}
	if maxIterations > 0 {
		args = append(args, "--max-iterations", fmt.Sprint(maxIterations))
	}
	cmd = exec.Command(uut, args...)
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gralph run: %v", err)
	}
	t.Cleanup(func() {
		tree := descendantsOf(t, uint32(cmd.Process.Pid))
		_ = cmd.Process.Kill()
		for pid := range tree {
			if p, err := os.FindProcess(int(pid)); err == nil {
				_ = p.Kill()
			}
		}
	})
	return cmd, logPath
}

// blockProbeStateHasReason reports whether the reason survived into the
// instance state dir, and lists what was there when it did not.
func blockProbeStateHasReason(t *testing.T, dir string) (bool, []string) {
	t.Helper()
	root := filepath.Join(dir, ".gralph", blockProbeInstance)
	var seen []string
	found := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		seen = append(seen, rel)
		if strings.Contains(readAll(t, path), blockProbeReason) {
			found = true
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", root, err)
	}
	return found, seen
}

// The acceptance case: one `gralph block` ends the flow with exit 3, the stage
// is not respawned even though the loop had iterations left, and the reason is
// on disk for the human who has to act on it.
func TestBlockVerbEndsTheLoopWithTheBlockedExitCode(t *testing.T) {
	uut, dir := newBlockProbe(t)
	writeBlockProbeAgent(t, dir, blockProbeBlocksAgent(uut, dir))

	cmd, logPath := startBlockProbeRun(t, uut, dir, "blocked", blockProbeMaxIterations)
	code, exited := waitForRun(t, cmd, blockProbeExitWait)
	if !exited {
		t.Fatalf("`gralph run` was still looping %s after the agent blocked (turns started: %d). "+
			"F5: `gralph block` records a terminal state the loop must honour at the top of the next iteration",
			blockProbeExitWait, countTurns(t, dir, "turns.log"))
	}
	if code != 3 {
		t.Fatalf("the blocked run exited %d, want 3. F5: blocked is its own exit code -- 0 is DONE/stopped "+
			"and 1 is failed, and a human who must act cannot tell those apart (run log: %s)",
			code, readAll(t, logPath))
	}
	if turns := countTurns(t, dir, "turns.log"); turns != 1 {
		t.Fatalf("%d agent turn(s) started with an iteration budget of %d, want 1. "+
			"F5: after a block the same command must not be respawned -- re-asking a question only a human "+
			"can answer is the defect this closes", turns, blockProbeMaxIterations)
	}
	if done := countTurns(t, dir, "done.log"); done != 1 {
		t.Fatalf("the blocking agent turn finished %d time(s), want 1 -- the turn that ran `gralph block` "+
			"never got to the end (run log: %s)", done, readAll(t, logPath))
	}
	if ok, seen := blockProbeStateHasReason(t, dir); !ok {
		t.Fatalf("the block reason %q is in none of the instance state files %v. "+
			"F5: the reason is recorded in the terminal state so a human knows what to act on",
			blockProbeReason, seen)
	}
}

// The failure direction: blocked must never be INFERRED. A gate that keeps
// failing is retryable -- the loop respawns the stage and, once the failure
// budget is spent, gives up with 1. A fix that promoted a failing gate to
// blocked would satisfy the exit-3 test above while making every ordinary flaky
// gate terminal, which is strictly worse than the defect it replaced.
//
// The two phases run the SAME stage against the SAME unsatisfiable gate and
// differ in one thing: the verb the agent runs. So exit 3 is attributable to
// `block` and to nothing else.
func TestBlockVerbIsNotInferredFromAFailingGate(t *testing.T) {
	uut, dir := newBlockProbe(t)

	// Phase 1: the agent only ever runs the gate, and the gate always fails.
	writeBlockProbeAgent(t, dir, blockProbeFailsAgent(uut, dir))
	gateRun, gateLog := startBlockProbeRun(t, uut, dir, "gate", 0)
	code, exited := waitForRun(t, gateRun, blockProbeBudgetWait)
	if !exited {
		t.Fatalf("`gralph run` was still retrying the failing gate %s later (turns: %d); it must give up "+
			"once the failure budget is spent", blockProbeBudgetWait, countTurns(t, dir, "turns.log"))
	}
	if code == 3 {
		t.Fatalf("a stage whose gate kept failing exited 3. F5: 3 means blocked -- a human must act -- and it "+
			"must come only from an explicit `gralph block`, never be inferred from a failing gate (run log: %s)",
			readAll(t, gateLog))
	}
	if code != 1 {
		t.Fatalf("a stage whose gate kept failing exited %d, want 1 (gave up after the failure budget) (run log: %s)",
			code, readAll(t, gateLog))
	}
	retried := countTurns(t, dir, "turns.log")
	if retried < 2 {
		t.Fatalf("the failing stage ran %d time(s); a gate failure must stay retryable -- only an explicit "+
			"block is terminal", retried)
	}

	// Phase 2: same stage, same failing gate, the agent blocks instead.
	writeBlockProbeAgent(t, dir, blockProbeBlocksAgent(uut, dir))
	blockRun, blockLog := startBlockProbeRun(t, uut, dir, "blocked", blockProbeMaxIterations)
	code, exited = waitForRun(t, blockRun, blockProbeExitWait)
	if !exited {
		t.Fatalf("`gralph run` was still looping %s after the agent blocked the same stage", blockProbeExitWait)
	}
	if code != 3 {
		t.Fatalf("the same stage exited %d when the agent ran `gralph block` and %d when its gate merely "+
			"failed. F5: the two must be distinguishable -- blocked is 3, failed is 1 (run log: %s)",
			code, 1, readAll(t, blockLog))
	}
	if turns := countTurns(t, dir, "turns.log") - retried; turns != 1 {
		t.Fatalf("%d agent turn(s) started after the block, want 1: a blocked stage must not be respawned", turns)
	}
}
