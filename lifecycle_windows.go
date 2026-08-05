//go:build windows

package main

// Windows process-lifecycle support for the orchestrator.
//
// Windows has no process groups: a child outlives its parent by default, and
// TerminateProcess (`taskkill /F /PID` without `/T`) runs no code in the target,
// so no deferred cleanup in gralph can reap the tree on that path. A job object
// can: the kernel closes every handle when the process dies, and
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE turns that close into a kill of everything
// still in the job.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobHandle is the run-level job object, held for the lifetime of the process.
// It is deliberately never closed on the normal path: the kernel's close at
// process exit is the reap.
var jobHandle windows.Handle

// superviseTree assigns the current process to a kill-on-close job object.
// Descendants inherit job membership, so this one assignment covers the
// launcher hop, the agent, and any MCP children the agent starts.
//
// No CREATE_BREAKAWAY_FROM_JOB: nested gralph instances (the harness's own
// probes) must stay inside their parent's job. Nested jobs are fine on Win8+.
func superviseTree() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("set JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("assign process to job object: %w", err)
	}
	jobHandle = job
	return nil
}

// killTree terminates proc and every process descended from it.
//
// A bare Process.Kill() is TerminateProcess on one pid: on Windows the children
// it started are not attached to it in any way the kernel will follow, so they
// are orphaned rather than reaped. `taskkill /T` is the platform's own walk of
// that tree, and it is scoped by pid -- never by image name, which would reach
// processes gralph never started (another harness's agent on the same machine).
//
// The job object from superviseTree does not cover this path: it only reaps
// when its handle closes, i.e. when gralph itself dies, and an agent timeout
// leaves gralph very much alive.
func killTree(proc *os.Process) error {
	if err := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(proc.Pid)).Run(); err == nil {
		return nil
	}
	// taskkill exits non-zero for a pid that is already gone or was never
	// there. That is not a failure of cancellation, so fall back to the
	// single-process kill and let "already finished" pass as success -- the
	// cancel path must never turn a won race into an error.
	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// processAlive reports whether pid is a process that is still running. Used by
// the run lock (F4) to tell a holder that crashed from one that is working.
//
// OpenProcess succeeding is NOT liveness on Windows: a process object outlives
// the process itself for as long as anyone holds a handle to it -- a parent that
// killed a child without waiting on it keeps exactly that handle -- so the pid
// stays openable after it is gone. The handle is SIGNALLED once the process
// exits, so a zero-timeout wait is the question that has only one answer:
// WAIT_TIMEOUT means still running.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE,
		false, uint32(pid))
	if err != nil {
		// Gone, or belonging to someone we may not query. Either way this
		// process cannot be the live holder of a lock in our own state dir.
		return false
	}
	defer windows.CloseHandle(h)
	state, err := windows.WaitForSingleObject(h, 0)
	return err == nil && state == uint32(windows.WAIT_TIMEOUT)
}
