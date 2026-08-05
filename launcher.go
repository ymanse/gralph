package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Gralph Agent Launcher Protocol V1 (GALP V1).
//
// gralph (the host) never spawns an agent directly. It execs a *launcher*
// (a plugin: a separate process) and reads back a structured result. The only
// thing the host knows about a launcher is "one command line to exec"; all
// agent-spawning variability lives behind this process boundary. The built-in
// default launcher is gralph re-invoking itself (`gralph __galp-subprocess`), so
// the zero-config path needs no external files (see galp_subprocess.go).
//
// Host <-> launcher communication is files + env + exit code:
//   - host writes a request JSON (GALP_REQUEST_FILE) and a prompt file,
//   - launcher writes a result JSON (GALP_RESULT_FILE),
//   - launcher exit 0 means "transport healthy, valid result written";
//     exit != 0 means the launcher itself broke (treated as crashed).
// ---------------------------------------------------------------------------

// GALPVersion is the protocol version this gralph build speaks.
const GALPVersion = 1

// SessionOutcome is the process-level result a launcher reports for one agent
// session. It is NOT a statement about graph/cursor progress: the host decides
// progress independently with resolveNext. A launcher only reports what
// happened to the process.
type SessionOutcome string

const (
	// OutcomeCompleted: the session exited normally. The host still rechecks
	// cursor progress independently.
	OutcomeCompleted SessionOutcome = "completed"
	// OutcomeCrashed: the session exited abnormally. Retried in a fresh
	// session, with backoff and the give-up budget applied.
	OutcomeCrashed SessionOutcome = "crashed"
	// OutcomeTimedOut: the session ran past its time budget. Handled like
	// crashed (retryable) but distinguished in logs.
	OutcomeTimedOut SessionOutcome = "timed_out"
	// OutcomeRateLimited: the agent hit a usage/quota limit. The host waits
	// until RetryAfter (honoring ctx cancellation) without spending the
	// give-up budget or the agent timeout, then retries the same cursor.
	OutcomeRateLimited SessionOutcome = "rate_limited"
	// OutcomeUnstartable: the agent binary itself could not be started (e.g.
	// not found). Retrying cannot help, so the host gives up immediately --
	// preserving the pre-GALP behavior of failing fast on a missing agent
	// command.
	OutcomeUnstartable SessionOutcome = "unstartable"
)

func (o SessionOutcome) valid() bool {
	switch o {
	case OutcomeCompleted, OutcomeCrashed, OutcomeTimedOut, OutcomeRateLimited, OutcomeUnstartable:
		return true
	}
	return false
}

// galpRequest is the JSON written to GALP_REQUEST_FILE (host -> launcher). It
// is the authoritative source of all inputs; the mirrored GALP_* env scalars
// are a convenience. Future fields are additive.
type galpRequest struct {
	Protocol       int               `json:"protocol"`
	SessionID      string            `json:"session_id"`
	Instance       string            `json:"instance"`
	Profile        string            `json:"profile"`
	Dir            string            `json:"dir"`
	PromptFile     string            `json:"prompt_file"`
	ResultFile     string            `json:"result_file"`
	AgentCommand   []string          `json:"agent_command"`
	TimeoutMS      int64             `json:"timeout_ms"`
	EnvPassthrough map[string]string `json:"env_passthrough"`
}

// galpResult is the JSON written to GALP_RESULT_FILE (launcher -> host).
type galpResult struct {
	Protocol   int            `json:"protocol"`
	Outcome    SessionOutcome `json:"outcome"`
	RetryAfter string         `json:"retry_after,omitempty"`
	Message    string         `json:"message,omitempty"`
}

// SessionResult is the parsed, validated outcome the host acts on.
type SessionResult struct {
	Outcome    SessionOutcome
	RetryAfter time.Time
	Message    string
}

// errProtocolMismatch marks a launcher result whose protocol version does not
// match the host's. It is fatal (not a transient crash): the host stops with a
// clear error rather than crash-looping.
var errProtocolMismatch = errors.New("galp protocol mismatch")

// parseGALPResult validates and decodes a launcher result. A protocol mismatch
// is returned wrapped in errProtocolMismatch (fatal); any other malformation
// is a normal error that the caller maps to a crashed outcome.
func parseGALPResult(data []byte) (SessionResult, error) {
	var r galpResult
	if err := json.Unmarshal(data, &r); err != nil {
		return SessionResult{}, fmt.Errorf("launcher result is not valid JSON: %w", err)
	}
	if r.Protocol != GALPVersion {
		return SessionResult{}, fmt.Errorf("%w: host speaks GALP v%d but launcher reported v%d",
			errProtocolMismatch, GALPVersion, r.Protocol)
	}
	if !r.Outcome.valid() {
		return SessionResult{}, fmt.Errorf("launcher result has unknown outcome %q", r.Outcome)
	}
	res := SessionResult{Outcome: r.Outcome, Message: r.Message}
	if r.Outcome == OutcomeRateLimited {
		if r.RetryAfter == "" {
			return SessionResult{}, fmt.Errorf("launcher reported rate_limited without retry_after")
		}
		t, err := time.Parse(time.RFC3339, r.RetryAfter)
		if err != nil {
			return SessionResult{}, fmt.Errorf("launcher rate_limited retry_after %q is not RFC3339: %w", r.RetryAfter, err)
		}
		res.RetryAfter = t
	}
	return res, nil
}

// selfExe locates the gralph executable for the default self-invoking
// launcher. It is a var so tests can substitute the test binary.
var selfExe = os.Executable

// resolveLauncher picks the launcher argv for a node: an explicit profile/node
// launcher when set, otherwise the built-in default (gralph __galp-subprocess).
// A relative launcher path that contains a separator resolves against the
// profile dir; a bare name is left for PATH lookup.
func resolveLauncher(p *Profile, node *CommandSpec) ([]string, error) {
	if argv := p.LauncherFor(node); len(argv) > 0 {
		out := append([]string{}, argv...)
		if first := out[0]; !filepath.IsAbs(first) && strings.ContainsAny(first, `/\`) {
			out[0] = filepath.Join(p.Dir, first)
		}
		return out, nil
	}
	exe, err := selfExe()
	if err != nil {
		return nil, fmt.Errorf("cannot locate gralph executable for the default launcher: %w", err)
	}
	return []string{exe, "__galp-subprocess"}, nil
}

// runLauncher execs one launcher for one agent session and returns the parsed
// result. The error return is reserved for host-side failures that cannot be
// fixed by retrying (the launcher binary cannot be started, or it reports an
// incompatible protocol); a launcher that runs but reports a bad/missing
// result is mapped to a crashed SessionResult instead.
func runLauncher(ctx context.Context, p *Profile, launcherArgv, agentArgv []string, prompt, sessionID string) (SessionResult, error) {
	tmp, err := os.MkdirTemp("", "galp-")
	if err != nil {
		return SessionResult{}, fmt.Errorf("galp: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	promptFile := filepath.Join(tmp, "prompt.txt")
	requestFile := filepath.Join(tmp, "request.json")
	resultFile := filepath.Join(tmp, "result.json")

	if err := os.WriteFile(promptFile, []byte(prompt), 0o600); err != nil {
		return SessionResult{}, fmt.Errorf("galp: write prompt: %w", err)
	}

	var timeoutMS int64
	if p.Agent.timeout > 0 {
		timeoutMS = p.Agent.timeout.Milliseconds()
	}

	// GRALPH_* are session passthrough vars: the launcher must forward them to
	// the agent so in-session `gralph next/do` find the same instance state.
	passthrough := map[string]string{
		"GRALPH_PROFILE":       p.Path,
		"GRALPH_INSTANCE_NAME": p.Name,
	}
	req := galpRequest{
		Protocol:       GALPVersion,
		SessionID:      sessionID,
		Instance:       p.Name,
		Profile:        p.Path,
		Dir:            p.Dir,
		PromptFile:     promptFile,
		ResultFile:     resultFile,
		AgentCommand:   agentArgv,
		TimeoutMS:      timeoutMS,
		EnvPassthrough: passthrough,
	}
	reqData, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return SessionResult{}, fmt.Errorf("galp: marshal request: %w", err)
	}
	if err := os.WriteFile(requestFile, reqData, 0o600); err != nil {
		return SessionResult{}, fmt.Errorf("galp: write request: %w", err)
	}

	argv := append([]string{}, launcherArgv...)
	argv = append(argv, "--")
	argv = append(argv, agentArgv...)

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = p.Dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("GALP_VERSION=%d", GALPVersion),
		"GALP_REQUEST_FILE="+requestFile,
		"GALP_RESULT_FILE="+resultFile,
		"GALP_PROMPT_FILE="+promptFile,
		fmt.Sprintf("GALP_TIMEOUT_MS=%d", timeoutMS),
		"GALP_SESSION_ID="+sessionID,
		"GRALPH_PROFILE="+p.Path,
		"GRALPH_INSTANCE_NAME="+p.Name,
	)
	// The launcher is a process hop: whatever it spawned (agent, and whatever
	// the agent spawned) has to go with it, or cancelling the host leaves the
	// agent running. WaitDelay still hard-kills the launcher if it lingers.
	cmd.Cancel = func() error { return killTree(cmd.Process) }
	cmd.WaitDelay = agentKillGrace

	runErr := cmd.Run()
	// Host-side problem: the launcher binary itself cannot be started. The
	// caller fails fast (retrying cannot help).
	if runErr != nil && (errors.Is(runErr, exec.ErrNotFound) || errors.Is(runErr, fs.ErrNotExist)) {
		return SessionResult{}, runErr
	}
	if runErr != nil {
		// Launcher exited non-zero: transport-level failure. The result file
		// is ignored and the session counts as crashed.
		return SessionResult{Outcome: OutcomeCrashed, Message: fmt.Sprintf("launcher exited with error: %v", runErr)}, nil
	}

	data, readErr := os.ReadFile(resultFile)
	if readErr != nil {
		return SessionResult{Outcome: OutcomeCrashed, Message: fmt.Sprintf("launcher wrote no result file: %v", readErr)}, nil
	}
	res, parseErr := parseGALPResult(data)
	if parseErr != nil {
		if errors.Is(parseErr, errProtocolMismatch) {
			return SessionResult{}, parseErr
		}
		return SessionResult{Outcome: OutcomeCrashed, Message: parseErr.Error()}, nil
	}
	return res, nil
}
