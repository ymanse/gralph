package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// runGALPSubprocess is gralph's built-in, default GALP launcher, invoked as
//
//	gralph __galp-subprocess -- <agent argv...>
//
// It is the subprocess launcher: it reproduces the pre-GALP launch behavior --
// spawn the agent as a subprocess, inherit stdio, substitute {{prompt}}, enforce
// the timeout, SIGTERM then hard-kill on cancellation. It reports only completed
// / crashed / timed_out / unstartable -- never rate_limited (quota detection is
// non-trivial and is the job of an opt-in launcher, by design). The editable
// `subprocess` example launcher is a shell copy of exactly this behavior; this
// is the only launcher baked into the binary, so the non-interactive path needs
// no external files.
//
// The return value is this launcher's own exit code: 0 when a valid result was
// written, non-zero only when the launcher itself failed (transport broken).
func runGALPSubprocess(args []string) int {
	resultFile := os.Getenv("GALP_RESULT_FILE")
	if resultFile == "" {
		fmt.Fprintln(os.Stderr, "gralph __galp-subprocess: GALP_RESULT_FILE is not set")
		return 1
	}
	write := func(res galpResult) int {
		res.Protocol = GALPVersion
		data, err := json.Marshal(res)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gralph __galp-subprocess: marshal result: %v\n", err)
			return 1
		}
		if err := os.WriteFile(resultFile, data, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "gralph __galp-subprocess: write result: %v\n", err)
			return 1
		}
		return 0
	}

	// The agent argv is everything after the "--" separator.
	agentArgv := args
	for i, a := range args {
		if a == "--" {
			agentArgv = args[i+1:]
			break
		}
	}
	if len(agentArgv) == 0 {
		fmt.Fprintln(os.Stderr, "gralph __galp-subprocess: no agent command after --")
		return 1
	}

	var prompt string
	if pf := os.Getenv("GALP_PROMPT_FILE"); pf != "" {
		b, err := os.ReadFile(pf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gralph __galp-subprocess: read prompt file: %v\n", err)
			return 1
		}
		prompt = string(b)
	}

	// {{prompt}} substitution is the launcher's responsibility.
	argv := make([]string, len(agentArgv))
	for i, a := range agentArgv {
		argv[i] = strings.ReplaceAll(a, "{{prompt}}", prompt)
	}

	// Because the default launcher adds a process hop between the orchestrator
	// and the agent, it must forward termination itself: when the host cancels
	// this launcher (SIGTERM), cancel the agent's context too so it is signaled
	// and hard-killed rather than orphaned.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	timeout := time.Duration(parseMillis(os.Getenv("GALP_TIMEOUT_MS"))) * time.Millisecond
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// cwd is already the profile dir (the host set it for this launcher); the
	// agent inherits it. Env carries GRALPH_PROFILE / GRALPH_INSTANCE_NAME
	// through to the session so in-session `gralph next/do` work.
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	// Agent timeout and host cancellation both land here. The agent is usually
	// a shell that spawned the real work, so killing the agent alone orphans
	// its children -- kill the tree.
	cmd.Cancel = func() error { return killTree(cmd.Process) }
	cmd.WaitDelay = agentKillGrace

	err := cmd.Run()
	switch {
	case err == nil:
		return write(galpResult{Outcome: OutcomeCompleted})
	case errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist):
		return write(galpResult{
			Outcome: OutcomeUnstartable,
			Message: fmt.Sprintf("agent command %q cannot be started: %v", argv[0], err),
		})
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return write(galpResult{
			Outcome: OutcomeTimedOut,
			Message: fmt.Sprintf("agent timed out after %s: %v", timeout, err),
		})
	default:
		return write(galpResult{
			Outcome: OutcomeCrashed,
			Message: fmt.Sprintf("agent exited with error: %v", err),
		})
	}
}

// parseMillis parses a non-negative millisecond count, returning 0 on any
// problem (0 means "no timeout").
func parseMillis(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
