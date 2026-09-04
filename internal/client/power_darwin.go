//go:build darwin

package client

import (
	"context"
	"os"
	"os/exec"
	"strconv"
)

// `caffeinate -i -w <pid>` blocks until <pid> exits, inhibiting idle sleep.
func newSleepInhibitor() sleepInhibitor {
	return &cmdInhibitor{
		newCmd: func(ctx context.Context) *exec.Cmd {
			return exec.CommandContext(ctx, "caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
		},
	}
}
