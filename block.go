package main

// F5-block-verb: the third exit of spec/shutdown-contract.md.
//
// A stage that needs a human decision has no way to say so, so the agent
// re-asks the same question in every fresh session until the iteration budget
// is gone (recorded in spec/defect-analysis.md §D5: 10+ sessions on one
// cursor). `gralph block "<reason>"` records a TERMINAL blocked state: the loop
// stops respawning the command and exits 3 -- a code of its own, because DONE
// and stopped (0) say "finished" and failed (1) says "try again", and both tell
// the human waiting on the decision the wrong thing.
//
// Blocked is never INFERRED. A gate that keeps failing stays retryable and ends
// on the failure budget with 1; only this verb writes the record. Promoting a
// failing gate to blocked would make every flaky gate terminal, which is worse
// than the defect it replaced.
//
// Terminal means terminal: unlike the stop intent of F3, the record is NOT
// cleared when a new `gralph run` starts. An outer wrapper that respawns
// `gralph run` round after round (run-until-done.sh) would otherwise walk
// straight back into the stage a human was asked to look at. Removing the file
// -- named in the message -- is what unblocks it, and `gralph reset` clears it
// along with the rest of the state dir.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// errBlocked is what runLoop returns once it honours a block record; main
// turns it into exit 3. Not a failure, so it does not go through fatal().
var errBlocked = errors.New("blocked")

// blockRecord is the block file: what a human has to act on, and where the flow
// stood when the agent gave up on it.
type blockRecord struct {
	Reason string `json:"reason"`
	Cursor string `json:"cursor,omitempty"`
	At     string `json:"at"`
}

func blockPath(dir string) string { return filepath.Join(dir, "blocked.json") }

// recordBlock writes the terminal blocked state for the instance whose state
// lives in dir. The cursor is stamped in as well: "blocked" without the stage
// it blocked on makes the human read the journal to find out what to act on.
func recordBlock(dir, reason string) error {
	cursor := ""
	if st, err := LoadState(dir); err == nil {
		cursor = st.Cursor
	}
	return atomicWriteJSON(blockPath(dir), blockRecord{
		Reason: reason,
		Cursor: cursor,
		At:     time.Now().UTC().Format(time.RFC3339),
	})
}

// blockedOn reports whether the instance is blocked and on what. A record that
// exists but does not parse still blocks: the agent said a human is needed, and
// respawning the stage over a short read is the wrong failure direction.
func blockedOn(dir string) (blocked bool, reason string) {
	data, err := readFileRetry(blockPath(dir))
	if err != nil {
		return false, ""
	}
	var br blockRecord
	if json.Unmarshal(data, &br) != nil || br.Reason == "" {
		return true, "(unreadable block record " + blockPath(dir) + ")"
	}
	return true, br.Reason
}

// runBlock is the `gralph block "<reason>"` verb, called from inside an agent
// session. It records the intent and returns; the loop is what acts on it, at
// the top of its next iteration.
func runBlock(args []string) {
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		fatal(fmt.Errorf(`usage: gralph block "<reason>" [--profile p] [--name instance]`))
	}
	reason := args[0]
	p, rest, err := profileFromSessionArgs(args[1:])
	if err != nil {
		fatal(err)
	}
	if len(rest) > 0 {
		fatal(fmt.Errorf("block takes exactly one reason; quote it as a single argument"))
	}
	if err := recordBlock(p.StateDir, reason); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "[gralph] instance %q: blocked -- %s\n", p.Name, reason)
	fmt.Fprintf(os.Stderr, "[gralph] the loop will stop with exit 3; remove %s once a human has acted\n", blockPath(p.StateDir))
}
