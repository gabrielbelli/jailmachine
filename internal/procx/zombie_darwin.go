package procx

import (
	"errors"

	"golang.org/x/sys/unix"
)

// sZomb is SZOMB from <sys/proc.h>: the process has exited and waits for its
// parent to reap it.
const sZomb = 5

// zombie reads the process state with sysctl kern.proc.pid, which avoids
// running ps on every poll.
func zombie(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// A zero-length answer (EIO) means the process has gone.
		return errors.Is(err, unix.EIO)
	}
	return kp.Proc.P_stat == sZomb
}
