//go:build !linux && !darwin

package reporter

import "errors"

// The crash monitor runs on Linux and macOS only, as in bugsnag-go, which
// supports Windows through a different mechanism this SDK does not copy.
const monitorAvailable = false

func ignoreShutdownSignals() {}

func (c *client) spawnMonitor() error {
	return errors.New("crash reporting is not supported on this platform")
}
