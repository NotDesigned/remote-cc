// Package procjob makes sure that everything rcc starts on machine A dies
// with rcc, however rcc ends (normal exit, console window closed, crash,
// killed from Task Manager).
//
// On Windows, rcc puts itself into a Job Object with KILL_ON_JOB_CLOSE.
// Children inherit the job automatically — there is no window between a
// process starting and being assigned — and when rcc's process ends the
// kernel closes the job handle and terminates every process still in it.
//
// On Unix this is a no-op: commands run in their own sessions and the agent
// kills a command's process group when its shim disconnects; ssh children
// exit when their pipes close.
package procjob

// KillOnExit arranges for all current and future child processes to be
// terminated when this process exits. It is safe to call more than once.
func KillOnExit() error { return killOnExit() }
