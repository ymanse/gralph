//go:build windows

// F3-graceful-stop: `gralph stop <profile> --name <instance>` must record a stop
// intent that the loop honours at the top of the NEXT iteration -- never in the
// middle of the turn that is already in flight.
//
// The contract (spec/shutdown-contract.md) makes graceful stop the tier that
// loses nothing: the running agent session runs to completion, its gate commits,
// the cursor advances, the loop exits 0, and the next stage is never started.
// "Stop as soon as possible" is explicitly NOT the contract -- that is what the
// second Ctrl-C / `--now` tier is for.
//
// Like the F1/F2 tests these drive a real `gralph run` and measure files and
// live PIDs rather than calling into the fix, so the package compiles both
// before and after it. Process-table helpers live in lifecycle_jobobject_test.go.
//
// Scoped by construction: every PID these tests touch is one they started.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const stopProbeInstance = "stopprobe"

// Two chained stages with no lua: `gralph do step-one` advances the cursor to
// step-two on its own, which is what makes "the finished turn committed" and
// "the next stage never started" two separable observations.
const stopProbeProfile = `agent:
  command: ['cmd', '/c', '%s']
  timeout: 180s
prompt: "graceful stop probe"
commands:
  - name: step-one
    guidance: |
      RUN: gralph do step-one
    next: [step-two]
  - name: step-two
    guidance: |
      Reaching this stage means the graceful stop failed to stop the loop.
      RUN: gralph do step-two
`

// The agent turn: announce that it started, work for a while, advance the
// cursor, then announce a clean finish. turns.log counts sessions started,
// done.log counts sessions that ended on their own terms -- a truncated agent
// leaves the first without the second.
const stopProbeAgent = `@echo off
echo turn>>"%%~dp0turns.log"
ping -n %d 127.0.0.1 >nul
"%s" do step-one --profile "%s" --name ` + stopProbeInstance + `
echo turn>>"%%~dp0done.log"
`

const (
	// stopProbeAgentPings sets the agent's turn length: `ping -n N` waits N-1
	// seconds. Long enough that the stop request lands mid-turn with room to
	// spare, short enough that a passing run stays quick.
	stopProbeAgentPings = 16
	stopProbeTurn       = (stopProbeAgentPings - 1) * time.Second
	// How long the first agent turn gets to start (the build + launcher hop).
	stopProbeStartWait = 45 * time.Second
	// How long the loop gets to exit after the in-flight turn finishes.
	stopProbeExitWait = stopProbeTurn + 60*time.Second
	// How long after the stop request the in-flight agent must still be alive.
	stopProbeMidTurnWait = 4 * time.Second
)

// startStopProbeRun builds the binary under test, starts an unbounded
// `gralph run` on the two-stage probe profile and returns once the first agent
// turn is genuinely in flight. It deliberately passes no --max-iterations:
// "the loop does not start another iteration" has to be the stop's doing.
//
// It builds into a temp dir on purpose: bin/gralph-orch.exe is the running
// orchestrator and must never be rebuilt.
func startStopProbeRun(t *testing.T) (uut, dir string, cmd *exec.Cmd) {
	t.Helper()
	dir = t.TempDir()
	uut = filepath.Join(dir, "gralph-uut.exe")
	if out, err := exec.Command("go", "build", "-o", uut, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	profile := filepath.Join(dir, "stop.yaml")
	agent := filepath.Join(dir, "agent.cmd")
	if err := os.WriteFile(agent, []byte(fmt.Sprintf(stopProbeAgent, stopProbeAgentPings, uut, profile)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profile, []byte(fmt.Sprintf(stopProbeProfile, agent)), 0o644); err != nil {
		t.Fatal(err)
	}

	// A file, never a pipe: an agent that outlives the run would hold a pipe
	// open and make Wait() block on the leak instead of on gralph.
	logFile, err := os.Create(filepath.Join(dir, "run.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	cmd = exec.Command(uut, "run", profile, "--name", stopProbeInstance)
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start gralph run: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		for pid := range descendantsOf(t, uint32(cmd.Process.Pid)) {
			if p, err := os.FindProcess(int(pid)); err == nil {
				_ = p.Kill()
			}
		}
	})

	deadline := time.Now().Add(stopProbeStartWait)
	for time.Now().Before(deadline) {
		if countTurns(t, dir, "turns.log") >= 1 {
			return uut, dir, cmd
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no agent turn started within %s; the stop has to be requested while a turn is in flight (run log: %s)",
		stopProbeStartWait, readAll(t, filepath.Join(dir, "run.log")))
	return "", "", nil
}

// requestGracefulStop runs the stop verb and fails the test if it is not
// accepted. This is the call that does not exist yet.
func requestGracefulStop(t *testing.T, uut, dir string) {
	t.Helper()
	stop := exec.Command(uut, "stop", filepath.Join(dir, "stop.yaml"), "--name", stopProbeInstance)
	stop.Dir = dir
	out, err := stop.CombinedOutput()
	if err != nil {
		t.Fatalf("`gralph stop` was refused: %v\n%s\nF3: stop must record the stop intent (a stop file in the state dir) and exit 0",
			err, out)
	}
}

// countTurns counts the lines the probe agent appended to one of its logs.
func countTurns(t *testing.T, dir, name string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(readAll(t, filepath.Join(dir, name)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// stopProbeCursor reads the committed cursor out of the instance state dir.
func stopProbeCursor(t *testing.T, dir string) string {
	t.Helper()
	blob := readAll(t, filepath.Join(dir, ".gralph", stopProbeInstance, "state.json"))
	if blob == "" {
		return ""
	}
	var st struct {
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal([]byte(blob), &st); err != nil {
		t.Fatalf("state.json is not parseable after a graceful stop: %v (%s)", err, blob)
	}
	return st.Cursor
}

// waitForRun waits for the run to exit, returning its exit code. ok is false if
// it was still running at the deadline.
func waitForRun(t *testing.T, cmd *exec.Cmd, within time.Duration) (code int, ok bool) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		switch {
		case err == nil:
			return 0, true
		case errors.As(err, &ee):
			return ee.ExitCode(), true
		default:
			t.Fatalf("wait for `gralph run`: %v", err)
		}
	case <-time.After(within):
	}
	return 0, false
}

// The acceptance case: the in-flight turn completes, its gate commits, the
// cursor advances, the loop exits 0 and never starts the next stage.
func TestGracefulStopFinishesTheTurnThenExits(t *testing.T) {
	uut, dir, cmd := startStopProbeRun(t)

	requestGracefulStop(t, uut, dir)

	code, exited := waitForRun(t, cmd, stopProbeExitWait)
	if !exited {
		t.Fatalf("`gralph run` was still looping %s after the graceful stop (turns started: %d). "+
			"F3: the stop intent must be checked at the top of every iteration so the loop stops after the in-flight turn",
			stopProbeExitWait, countTurns(t, dir, "turns.log"))
	}
	if code != 0 {
		t.Fatalf("graceful stop exited %d, want 0 -- a human asking the loop to stop is not a failure (run log: %s)",
			code, readAll(t, filepath.Join(dir, "run.log")))
	}
	if done := countTurns(t, dir, "done.log"); done != 1 {
		t.Fatalf("the in-flight agent turn finished %d time(s), want 1: the graceful tier must let the running session complete", done)
	}
	if cursor := stopProbeCursor(t, dir); cursor != "step-two" {
		t.Fatalf("cursor is %q after the graceful stop, want %q: the turn that completed must have its gate committed", cursor, "step-two")
	}
	if turns := countTurns(t, dir, "turns.log"); turns != 1 {
		t.Fatalf("%d agent turn(s) started, want 1: after a graceful stop the loop must not start the next stage", turns)
	}
}

// The failure direction: a stop intent recorded mid-turn must NOT truncate the
// running agent. The check belongs at the top of the iteration -- after
// resolveNext and before the launcher is exec'd -- so a stop that arrives while
// an agent is working changes nothing about that agent. A "fix" that cancelled
// the run context on stop would satisfy the exit-code half of the test above
// and silently throw away the work in flight.
func TestGracefulStopDoesNotTruncateTheRunningAgent(t *testing.T) {
	uut, dir, cmd := startStopProbeRun(t)
	tree := descendantsOf(t, uint32(cmd.Process.Pid))
	if len(tree) < 2 {
		t.Fatalf("`gralph run` has %d descendant(s); the launcher hop and the agent must both be up before the stop is requested", len(tree))
	}

	requestGracefulStop(t, uut, dir)

	// The turn has seconds left to run. Nothing in it may die because of the
	// stop request.
	time.Sleep(stopProbeMidTurnWait)
	if alive := stillAlive(t, tree); len(alive) != len(tree) {
		t.Fatalf("only %d of %d process(es) in the in-flight agent tree survived %s after the stop request (alive: %v, tree: %v). "+
			"F3: graceful stop records an intent -- it must never cancel the session that is already running",
			len(alive), len(tree), stopProbeMidTurnWait, alive, tree)
	}

	code, exited := waitForRun(t, cmd, stopProbeExitWait)
	if !exited {
		t.Fatalf("`gralph run` was still looping %s after the graceful stop", stopProbeExitWait)
	}
	if code != 0 {
		t.Fatalf("graceful stop exited %d, want 0 (run log: %s)", code, readAll(t, filepath.Join(dir, "run.log")))
	}
	if done := countTurns(t, dir, "done.log"); done != 1 {
		t.Fatalf("the agent that was in flight when the stop arrived finished %d time(s), want 1 -- it was cut off mid-turn", done)
	}
}
