//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
)

// POSIX counterpart of lifecycle_windows.go. There is nothing to join here: a
// process group already ties the tree together, so superviseTree is a no-op and
// `run` behaves exactly as it did before F1.
func superviseTree() error { return nil }

// killTree is deliberately the pre-F2 cancel body, unchanged: on POSIX SIGTERM
// actually arrives, the process group already ties the tree together, and the
// hard Kill is only the fallback for a signal that could not be delivered.
// F2 fixes Windows; regressing POSIX to a tree walk it does not need is not
// part of it.
func killTree(proc *os.Process) error {
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return proc.Kill()
	}
	return nil
}

// processAlive reports whether pid is a process that is still running, for the
// run lock (F4). Signal 0 delivers nothing: it only runs the kernel's existence
// and permission checks, and EPERM is an answer -- the process is there, it just
// is not ours.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
