package main

// F4-run-lock: one `gralph run` per instance.
//
// Two loops driving one state dir interleave their cursor writes and their gate
// commits, and the damage is silent -- the second run just picks up whatever the
// first left behind. withStateLock (lock.go) cannot prevent that: it is
// deliberately scoped to the commit phase so parallel gates stay parallel, so it
// serializes writes rather than refusing a second driver.
//
// The lock is a file the state dir either has or does not, created with O_EXCL
// so the create IS the acquisition, carrying the PID of the orchestrator that
// owns it. LIVENESS -- never age -- decides whether a lock left behind may be
// taken: gralph can die to a TerminateProcess (that is the premise of F1) and
// then nothing it deferred ever runs, so a crashed holder must not wedge the
// instance forever. A lock whose holder still answers is refused, because that
// is the case the lock exists for.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// runLockInfo is the lock file: who holds the instance, and since when.
type runLockInfo struct {
	PID      int    `json:"pid"`
	Instance string `json:"instance"`
	At       string `json:"at"`
}

func runLockPath(dir string) string { return filepath.Join(dir, "run.lock") }

// acquireRunLock claims the instance whose state lives in dir, returning the
// release. On refusal it names the live holder and has written nothing: a run
// that lost the lock must leave the instance exactly as it found it.
func acquireRunLock(dir, instance string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := runLockPath(dir)
	self := os.Getpid()
	body, err := json.MarshalIndent(runLockInfo{
		PID: self, Instance: instance, At: time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	// Bounded: every turn either wins the create or reclaims one dead holder, so
	// a handful covers any real contention and a pathological loser still exits
	// rather than spinning.
	for attempt := 0; attempt < 5; attempt++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			_, werr := f.Write(body)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				_ = os.Remove(path)
				return nil, fmt.Errorf("write run lock %s: %w", path, werr)
			}
			return func() { releaseRunLock(path, self) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("take run lock %s: %w", path, err)
		}
		held := readRunLock(path)
		if held.PID != 0 && processAlive(held.PID) {
			return nil, fmt.Errorf("instance %q is already being run by gralph (PID %d, lock %s); "+
				"stop that loop with `gralph stop` or run this profile under a different --name",
				instance, held.PID, path)
		}
		// Nobody is behind this lock any more. Reclaim it and race for the
		// create again -- refusing forever on a dead holder's leftovers would
		// make a single crash permanent.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("reclaim the run lock of dead PID %d (%s): %w", held.PID, path, err)
		}
	}
	return nil, fmt.Errorf("could not take the run lock for instance %q (%s): it kept changing hands", instance, path)
}

// readRunLock reads the holder record. The file is created before it is written,
// so an unparseable read can be that gap rather than an abandoned lock; it is
// retried briefly before the caller is told there is no holder, because "no
// holder" is exactly what authorises reclaiming it.
func readRunLock(path string) runLockInfo {
	var info runLockInfo
	for i := 0; i < 10; i++ {
		data, err := readFileRetry(path)
		if os.IsNotExist(err) {
			return info
		}
		if err == nil && json.Unmarshal(data, &info) == nil && info.PID != 0 {
			return info
		}
		time.Sleep(20 * time.Millisecond)
	}
	return info
}

// releaseRunLock drops the lock, but only while it is still ours: once another
// run has reclaimed it the file belongs to that run, not to this one.
func releaseRunLock(path string, self int) {
	if held := readRunLock(path); held.PID != self {
		return
	}
	_ = os.Remove(path)
}
