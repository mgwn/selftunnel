package client

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
)

// sleepInhibitor abstracts OS-specific mechanisms to keep the system awake
// while a tunnel connection is active.
type sleepInhibitor interface {
	inhibit() error
	release() error
}

var pm = sync.OnceValue(newSleepInhibitor)

// PreventSleep asks the OS not to idle-sleep while the relay tunnel is
// active (spec §3.7). Idempotent: calling it while already inhibited is a
// no-op. Errors are non-fatal and are logged by the caller.
func PreventSleep() error {
	return pm().inhibit()
}

// AllowSleep restores normal idle sleep behavior after the tunnel goes
// offline or the client exits (spec §3.7). Idempotent.
func AllowSleep() error {
	return pm().release()
}

// cmdInhibitor prevents sleep by keeping a helper process alive
// (macOS caffeinate, Linux systemd-inhibit). Cancelling the context
// terminates the helper and releases the inhibition.
type cmdInhibitor struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	newCmd func(ctx context.Context) *exec.Cmd
}

// inhibit starts the helper subprocess if it is not already running and
// marks the inhibition active; a second call is a no-op.
func (i *cmdInhibitor) inhibit() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := i.newCmd(ctx)
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	i.cancel = cancel
	go func() { _ = cmd.Wait() }()
	return nil
}

// release cancels the helper's context, terminating the subprocess and
// lifting the inhibition; a second call is a no-op.
func (i *cmdInhibitor) release() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cancel == nil {
		return nil
	}
	i.cancel()
	i.cancel = nil
	return nil
}
