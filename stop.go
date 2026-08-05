package main

// F3-graceful-stop: the two-tier stop of spec/shutdown-contract.md.
//
// Graceful is a recorded INTENT, never a cancellation. `gralph stop` (and the
// first Ctrl-C) drops a stop file in the instance state dir and returns; the
// loop reads it at the top of its next iteration -- after resolveNext, before
// the launcher is exec'd. That is the only point where the previous stage is
// fully committed and nothing is in flight, so stopping there loses no work.
// An agent with 40 minutes left on its turn keeps all 40.
//
// Immediate (`--now`, or a second Ctrl-C) is the tier for when that wait is not
// acceptable, so it cannot be checked at the iteration boundary -- it is polled.
// It exits 130 on the spot: the cursor is already durable on disk, and the
// process exit closes the last handle to the kill-on-close job object from F1,
// which is what hands the rest of the tree to the kernel to reap.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// stopIntent is the stop file: which tier was asked for, and when.
type stopIntent struct {
	Now bool   `json:"now"`
	At  string `json:"at"`
}

func stopPath(dir string) string { return filepath.Join(dir, "stop.json") }

// requestStop records a stop intent for the instance whose state lives in dir.
func requestStop(dir string, now bool) error {
	return atomicWriteJSON(stopPath(dir), stopIntent{
		Now: now,
		At:  time.Now().UTC().Format(time.RFC3339),
	})
}

// stopRequested reports whether a stop intent is on record and which tier it
// asks for. A stop file that exists but does not parse still counts as a stop:
// a human asked the loop to stop, and refusing that over a short read is the
// wrong failure direction.
func stopRequested(dir string) (requested, now bool) {
	data, err := readFileRetry(stopPath(dir))
	if err != nil {
		return false, false
	}
	var si stopIntent
	_ = json.Unmarshal(data, &si)
	return true, si.Now
}

func clearStop(dir string) { _ = os.Remove(stopPath(dir)) }

// forceStop is the immediate tier. It does not unwind: the cursor was made
// durable by the last commit (atomic write + fsync + rename), so there is
// nothing left to save, and every process still in the job dies with this one.
func forceStop(dir string) {
	cursor := "?"
	if st, err := LoadState(dir); err == nil && st.Cursor != "" {
		cursor = st.Cursor
	}
	clearStop(dir)
	fmt.Fprintf(os.Stderr, "[gralph] force-stopped at %s\n", cursor)
	os.Exit(130)
}

// watchStopNow polls for an immediate stop request. The graceful tier is read
// once per iteration by the loop itself; `--now` exists precisely because
// waiting for that boundary is not acceptable, so it gets its own watcher.
func watchStopNow(ctx context.Context, dir string) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if requested, now := stopRequested(dir); requested && now {
			forceStop(dir)
		}
	}
}

// parseStopArgs parses `gralph stop <profile.yaml> [--name I] [--now]`. As with
// `run`, the profile path may come before or after the flags.
func parseStopArgs(args []string) (profilePath, instance string, now bool, err error) {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	name := fs.String("name", "", "instance name (default: profile filename stem)")
	immediate := fs.Bool("now", false, "stop immediately instead of at the next iteration boundary")
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		profilePath = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", "", false, err
	}
	rest := fs.Args()
	if profilePath == "" && len(rest) > 0 {
		profilePath, rest = rest[0], rest[1:]
	}
	if profilePath == "" || len(rest) > 0 {
		return "", "", false, fmt.Errorf("usage: gralph stop <profile.yaml> [--name <instance>] [--now]")
	}
	return profilePath, *name, *immediate, nil
}
