//go:build !windows

package main

import (
	"os"
	"syscall"
)

// becomeNewAgent replaces this process with the binary now at exePath: same
// PID, same arguments, same environment. The supervisor (systemd, launchd)
// sees nothing happen, and there is no moment when no agent is running — the
// new one announces itself with a check-in as its first act.
func becomeNewAgent(exePath string) error {
	return syscall.Exec(exePath, os.Args, os.Environ())
}
