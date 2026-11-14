package node

import (
	"golang.org/x/sys/unix"
)

// Rebooter restarts the machine.
type Rebooter interface {
	Reboot() error
}

// SyscallRebooter flushes filesystems and calls reboot(2). The agent must run
// in the host PID namespace with CAP_SYS_BOOT: from inside a container PID
// namespace the kernel only signals that namespace's init instead.
type SyscallRebooter struct{}

// Reboot implements Rebooter.
func (SyscallRebooter) Reboot() error {
	unix.Sync()
	return unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
}
