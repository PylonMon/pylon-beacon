//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
)

// becomeNewAgent starts the binary now at exePath and then does nothing but
// wait for it.
//
// Windows has no exec-in-place, and the obvious alternative — exit and let the
// scheduled task restart us — leaves a gap: the installer's restart interval is
// one minute, and a node that pushes every 20-30s is reported silent well
// inside a minute. An updater that pages the customer is worse than no updater.
// So the old process stays as the new one's parent: the task is still
// "running", Stop-ScheduledTask (which the installer uses to upgrade) still
// stops the whole tree, and when the child exits this process exits with its
// code so the task's own restart policy still applies. The cost is one idle
// ~10 MB process until the next restart or reboot.
func becomeNewAgent(exePath string) error {
	cmd := exec.Command(exePath, os.Args[1:]...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	handedOver.Store(true)
	err := cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	}
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
	return nil
}
