package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"
)

// MaxConsecutiveAgentFailures: the loop gives up after this many abnormal
// agent exits in a row without any cursor progress (a crash-looping agent
// would otherwise spin forever).
const MaxConsecutiveAgentFailures = 5

// Backoff between consecutive abnormal agent exits: 2s, 4s, 8s, ... capped.
// Vars (not consts) so tests can shrink them.
var (
	agentBackoffBase = 2 * time.Second
	agentBackoffMax  = 30 * time.Second
)

// agentKillGrace is how long a cancelled agent process (signal or timeout)
// gets to exit after SIGTERM before it is hard-killed.
const agentKillGrace = 5 * time.Second

// runLoop is the orchestrator. Each iteration is a fresh session / fresh
// context: it rotates the session id (which resets the session-scoped
// failure counters), then launches the non-interactive agent command from
// the YAML profile with the ralph prompt.
//
// Termination: at the top of every iteration the loop calls resolveNext()
// directly (a function call, not the CLI); if the cursor is DONE it breaks.
// The agent never observes the loop's stop signal.
//
// The loop also stops with an error when:
//   - ctx is cancelled (SIGINT/SIGTERM); the cursor is preserved on disk,
//   - the agent binary cannot be started at all (retrying is pointless),
//   - the agent exits abnormally MaxConsecutiveAgentFailures times in a row
//     without the cursor moving (with exponential backoff between retries).
func runLoop(ctx context.Context, p *Profile, maxIterations int) error {
	if len(p.Agent.Command) == 0 {
		return fmt.Errorf("profile: agent.command is required to run the loop")
	}

	// One driver per instance. Taken before anything in the state dir is read
	// or written, so a run that loses the lock leaves the instance byte-for-byte
	// as it found it -- not a half-rotated session id, not a moved cursor.
	unlock, err := acquireRunLock(p.StateDir, p.Name)
	if err != nil {
		return err
	}
	defer unlock()

	// State the instance up front: a mistyped --name silently lands on an
	// empty state dir, and "starting a fresh flow" is the only visible
	// difference from resuming the one the user meant.
	if _, err := os.Stat(statePath(p.StateDir)); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "[gralph] instance %q: no existing state in %s; starting a fresh flow\n", p.Name, p.StateDir)
	} else {
		fmt.Fprintf(os.Stderr, "[gralph] instance %q: resuming from %s\n", p.Name, p.StateDir)
	}

	// A stop intent left behind by a previous run is not an instruction to this
	// one, so it is cleared before anything can observe it. The `--now` tier
	// cannot wait for the iteration boundary the graceful one is defined at, so
	// it gets a watcher that lives exactly as long as the loop.
	clearStop(p.StateDir)
	watchCtx, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	go watchStopNow(watchCtx, p.StateDir)

	consecutiveFailures := 0
	for i := 1; ; i++ {
		if maxIterations > 0 && i > maxIterations {
			fmt.Fprintf(os.Stderr, "[gralph] reached max iterations (%d); stopping\n", maxIterations)
			return nil
		}

		cursor, err := resolveNext(p)
		if err != nil {
			return err
		}
		if cursor == DoneCursor {
			fmt.Fprintf(os.Stderr, "[gralph] cursor is DONE; loop finished after %d iteration(s)\n", i-1)
			appendJournal(p.StateDir, JournalEvent{Event: EvLoopDone, Iteration: i - 1})
			return nil
		}
		if ctx.Err() != nil {
			return interrupted(i, cursor)
		}
		// The graceful stop is honoured HERE and nowhere else: the stage before
		// this one is committed and nothing is in flight, so stopping costs no
		// work. A human asking the loop to stop is not a failure -- exit 0.
		if requested, _ := stopRequested(p.StateDir); requested {
			clearStop(p.StateDir)
			fmt.Fprintf(os.Stderr, "[gralph] stopped cleanly at %s\n", cursor)
			appendJournal(p.StateDir, JournalEvent{Event: EvLoopStopped, Cursor: cursor, Iteration: i - 1})
			return nil
		}
		// Same boundary, terminal outcome: an agent that ran `gralph block` said
		// this stage needs a human, so respawning it is exactly the waste F5
		// exists to stop. The record is left on disk for whoever has to act.
		if blocked, reason := blockedOn(p.StateDir); blocked {
			fmt.Fprintf(os.Stderr, "[gralph] blocked at %s: %s\n", cursor, reason)
			fmt.Fprintf(os.Stderr, "[gralph] not respawning; remove %s once a human has acted\n", blockPath(p.StateDir))
			appendJournal(p.StateDir, JournalEvent{Event: EvLoopBlocked, Cursor: cursor, Iteration: i - 1, Reason: reason})
			return errBlocked
		}

		// New session: rotate id, reset per-command failure counters.
		st, err := LoadState(p.StateDir)
		if err != nil {
			return err
		}
		st.SessionID = newSessionID()
		st.Failures = map[string]int{}
		if err := st.Save(p.StateDir); err != nil {
			return err
		}

		fmt.Fprintf(os.Stderr, "[gralph] iteration %d | session %s | cursor %s\n", i, st.SessionID, cursor)
		appendJournal(p.StateDir, JournalEvent{
			Event:     EvSessionStart,
			Session:   st.SessionID,
			Cursor:    cursor,
			Iteration: i,
		})

		// Per-node overrides: the cursor's command may carry its own agent
		// command and/or ralph prompt; otherwise the profile-level ones apply.
		// The agent is never spawned directly: a GALP launcher (the built-in
		// default unless the profile overrides it) execs it and reports a
		// structured result (see launcher.go).
		node := p.Command(cursor)
		launcherArgv, err := resolveLauncher(p, node)
		if err != nil {
			return err
		}
		res, runErr := runLauncher(ctx, p, launcherArgv, p.AgentCommandFor(node), p.PromptFor(node), st.SessionID)
		if ctx.Err() != nil {
			return interrupted(i, cursor)
		}
		if runErr != nil {
			// Host-side failure: the launcher binary cannot be started, or it
			// reported an incompatible protocol. Retrying cannot help.
			return fmt.Errorf("launcher %q: %w", launcherArgv[0], runErr)
		}

		switch res.Outcome {
		case OutcomeRateLimited:
			// The agent hit a usage limit: wait out the window without
			// spending the give-up budget or applying the agent timeout, then
			// retry the same cursor in a fresh session.
			wait := time.Until(res.RetryAfter)
			if wait < 0 {
				wait = 0
			}
			fmt.Fprintf(os.Stderr, "[gralph] rate limited; waiting %s until %s before retry\n",
				wait.Round(time.Second), res.RetryAfter.UTC().Format(time.RFC3339))
			appendJournal(p.StateDir, JournalEvent{
				Event:      EvRateLimited,
				Session:    st.SessionID,
				Cursor:     cursor,
				Iteration:  i,
				RetryAfter: res.RetryAfter.UTC().Format(time.RFC3339),
				Reason:     res.Message,
			})
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return interrupted(i, cursor)
			}
			continue
		case OutcomeUnstartable:
			// The agent binary itself cannot be started: retrying is pointless
			// (preserves the pre-GALP fail-fast on a missing agent command).
			return fmt.Errorf("agent command %q cannot be started: %s", p.AgentCommandFor(node)[0], res.Message)
		}

		agentFailed := res.Outcome != OutcomeCompleted
		if agentFailed {
			// An agent process dying is not a graph failure; report and keep
			// looping (the cursor did not move, so the work is retried in a
			// fresh session).
			fmt.Fprintf(os.Stderr, "[gralph] agent session %s: %s\n", res.Outcome, res.Message)
		}

		after, err := resolveNext(p)
		if err != nil {
			return err
		}
		if after != cursor {
			// Cursor progressed; the agent is alive enough.
			consecutiveFailures = 0
		} else if agentFailed {
			consecutiveFailures++
			if consecutiveFailures >= MaxConsecutiveAgentFailures {
				return fmt.Errorf("agent exited abnormally %d times in a row without cursor progress (cursor: %s); giving up",
					consecutiveFailures, cursor)
			}
			// Exponential backoff before the next attempt.
			d := agentBackoff(consecutiveFailures)
			fmt.Fprintf(os.Stderr, "[gralph] backing off %s before retry (%d/%d consecutive abnormal exits)\n",
				d, consecutiveFailures, MaxConsecutiveAgentFailures)
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return interrupted(i, cursor)
			}
		}
	}
}

// interrupted reports a SIGINT/SIGTERM stop. The cursor is already preserved
// on disk, so a plain `gralph run` resumes where the loop left off.
func interrupted(iteration int, cursor string) error {
	fmt.Fprintf(os.Stderr, "[gralph] interrupted at iteration %d (cursor: %s)\n", iteration, cursor)
	fmt.Fprintf(os.Stderr, "[gralph] state is preserved; rerun `gralph run` to resume from this cursor\n")
	return fmt.Errorf("interrupted")
}

// agentBackoff returns the wait before retry n (1-based): 2s, 4s, 8s, ...
// capped at agentBackoffMax.
func agentBackoff(n int) time.Duration {
	d := agentBackoffBase
	for ; n > 1 && d < agentBackoffMax; n-- {
		d *= 2
	}
	if d > agentBackoffMax {
		d = agentBackoffMax
	}
	return d
}

func newSessionID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%d-%s", time.Now().Unix(), hex.EncodeToString(b[:]))
}
