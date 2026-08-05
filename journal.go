package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ---------------------------------------------------------------------------
// Append-only event journal (journal.jsonl).
//
// Every major transition is appended as one JSON line, so a finished (or
// stuck) loop can be analyzed after the fact instead of from a single stderr
// line. Commit events are written inside the already-held state lock, so
// lines never interleave and their order matches the order the transitions
// were committed in.
//
// Writes are best-effort: observability must never block the main flow, so
// a failed append is reported on stderr and otherwise ignored.
// ---------------------------------------------------------------------------

// Journal event kinds.
const (
	EvSessionStart     = "session_start"     // orchestrator rotated the session
	EvCommandSucceeded = "command_succeeded" // cursor advanced
	EvCommandFailed    = "command_failed"    // gate failed or script error
	EvSubitemRecorded  = "subitem_recorded"  // one fork/join work item committed
	EvRateLimited      = "rate_limited"      // launcher reported a quota wait
	EvLoopDone         = "loop_done"         // cursor reached DONE
	EvLoopStopped      = "loop_stopped"      // a human asked the loop to stop
	EvLoopBlocked      = "loop_blocked"      // the agent blocked: a human must act
)

// JournalEvent is one journal.jsonl line. Only the fields relevant to the
// event kind are set; the rest are omitted from the JSON.
type JournalEvent struct {
	At    string `json:"at"` // RFC3339, filled by appendJournal
	Event string `json:"event"`

	Session    string `json:"session,omitempty"`
	Cursor     string `json:"cursor,omitempty"`      // session_start
	Iteration  int    `json:"iteration,omitempty"`   // session_start, loop_done
	Command    string `json:"command,omitempty"`     // command_* (label for failures)
	Next       string `json:"next,omitempty"`        // command_succeeded: routed / next cursor
	GateMs     int64  `json:"gate_ms,omitempty"`     // command_succeeded: lua gate duration
	Failure    int    `json:"failure,omitempty"`     // command_failed: failure number
	Reason     string `json:"reason,omitempty"`      // command_failed: fail reason or script error
	Subcommand string `json:"subcommand,omitempty"`  // subitem_recorded
	Key        string `json:"key,omitempty"`         // subitem_recorded: work-item key
	RetryAfter string `json:"retry_after,omitempty"` // rate_limited: RFC3339 wake time
}

func journalPath(dir string) string { return filepath.Join(dir, "journal.jsonl") }

// appendJournal appends one event line, stamping the timestamp. Best-effort:
// any error is a stderr warning, never a returned failure.
func appendJournal(dir string, ev JournalEvent) {
	ev.At = time.Now().UTC().Format(time.RFC3339)
	if err := appendJournalLine(dir, ev); err != nil {
		fmt.Fprintf(os.Stderr, "[gralph] journal write failed (ignored): %v\n", err)
	}
}

func appendJournalLine(dir string, ev JournalEvent) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(journalPath(dir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(data, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}
