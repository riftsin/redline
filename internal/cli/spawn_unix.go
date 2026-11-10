//go:build !windows

package cli

import "syscall"

// detached starts the server in its own session, so it survives the CLI
// and the terminal that ran it.
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
