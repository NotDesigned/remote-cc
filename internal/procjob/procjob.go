// Package procjob makes sure that everything rcc starts on machine A dies
// with rcc, however rcc ends (normal exit, terminal closed, crash, kill -9,
// Task Manager).
//
// Windows: rcc puts itself into a Job Object with KILL_ON_JOB_CLOSE.
// Children inherit the job automatically, and when rcc's process ends the
// kernel terminates every process still in it.
//
// Unix: rcc starts a guard process (rcc __rcc_guard) in its own session that
// ignores HUP/INT/TERM, connected by a pipe whose write end only rcc holds.
// rcc reports each command process group and each ssh process it starts.
// When rcc ends for any reason the kernel closes the pipe; the guard sees
// EOF, sends TERM then KILL to everything still registered, and exits.
package procjob

// GuardArg is the hidden subcommand that runs the Unix guard.
const GuardArg = "__rcc_guard"

// KillOnExit arranges for tracked (Unix) or all (Windows) child processes to
// be terminated when this process exits. Safe to call more than once.
func KillOnExit() error { return killOnExit() }

// TrackGroup registers a process group (pgid) to kill when rcc ends.
// No-op on Windows, or if KillOnExit was not called.
func TrackGroup(pgid int) { track('g', pgid) }

// RemoveDirOnExit asks the guard to delete rcc's private temp dir if rcc
// dies without cleaning up. Only an "rcc-*" dir directly under the system
// temp dir is accepted. No-op on Windows.
func RemoveDirOnExit(dir string) { trackDir(dir) }

// TrackProcess registers a single process to kill when rcc ends.
// No-op on Windows, or if KillOnExit was not called.
func TrackProcess(pid int) { track('p', pid) }
