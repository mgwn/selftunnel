//go:build linux

package client

import (
	"context"
	"os/exec"
)

// If systemd-inhibit is not available, inhibit() returns an error and sleep
// is not prevented.
func newSleepInhibitor() sleepInhibitor {
	return &cmdInhibitor{
		newCmd: func(ctx context.Context) *exec.Cmd {
			return exec.CommandContext(ctx, "systemd-inhibit",
				"--what=sleep:idle",
				"--who=selftunnel",
				"--why=Keeping tunnel relay connection alive",
				"--mode=block",
				"sleep", "infinity",
			)
		},
	}
}
