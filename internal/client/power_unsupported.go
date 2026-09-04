//go:build !windows && !darwin && !linux

package client

// noopInhibitor is a fallback for platforms without a sleep-inhibition implementation.
type noopInhibitor struct{}

func newSleepInhibitor() sleepInhibitor {
	return &noopInhibitor{}
}

func (i *noopInhibitor) inhibit() error { return nil }
func (i *noopInhibitor) release() error { return nil }
