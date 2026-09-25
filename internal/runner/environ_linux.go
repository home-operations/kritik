package runner

import "syscall"

// hideEnviron makes the runner process non-dumpable, which leaves its
// /proc/<pid> entries owned by root. A command the run tool executes
// shares the runner's uid, and could otherwise read the git token, the
// model key and the database DSN from the runner's /proc/<pid>/environ,
// whatever environment the command itself was given.
func hideEnviron() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
