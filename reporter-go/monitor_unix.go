//go:build linux || darwin

package reporter

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

const monitorAvailable = true

// monitorEnv is added to the monitor process's environment. Besides marking
// it as the monitor, it stops bugsnag-go from starting a monitor or a session
// of its own inside it.
var monitorEnv = []string{
	monitorCookieKey + "=" + monitorCookieValue,
	"BUGSNAG_DISABLE_PANIC_HANDLER=1",
	"BUGSNAG_AUTO_CAPTURE_SESSIONS=0",
}

// ignoreShutdownSignals keeps the monitor alive when Ctrl-C or systemd
// signals the whole program. The monitor exits when the program does, once
// it has seen whether the program crashed.
func ignoreShutdownSignals() {
	signal.Ignore(os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)
}

// spawnMonitor starts the monitor process and points this process's standard
// error at it.
func (c *client) spawnMonitor() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	originalFD, err := syscall.Dup(int(os.Stderr.Fd()))
	if err != nil {
		return fmt.Errorf("copy standard error: %w", err)
	}
	original := os.NewFile(uintptr(originalFD), "stderr")
	r, w, err := os.Pipe()
	if err != nil {
		original.Close()
		return err
	}

	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), monitorEnv...)
	cmd.Stdin = r
	cmd.Stderr = original
	// Anything main prints before Configure would be printed twice; the
	// monitor's standard output is discarded instead.
	cmd.Stdout = nil
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		original.Close()
		return fmt.Errorf("start monitor: %w", err)
	}
	if err := dupTo(int(w.Fd()), int(os.Stderr.Fd())); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		r.Close()
		w.Close()
		original.Close()
		return fmt.Errorf("redirect standard error: %w", err)
	}
	go c.watchMonitor(cmd, r, w, original)
	return nil
}

// watchMonitor waits for the monitor process. The monitor only exits after
// this process does, so if Wait returns, the monitor has died: standard error
// goes back to where it was, and anything left in the pipe is written there.
// This process keeps the pipe's read end open until then, so its writes never
// fail while the monitor is gone.
func (c *client) watchMonitor(cmd *exec.Cmd, r, w, original *os.File) {
	err := cmd.Wait()
	if restoreErr := dupTo(int(original.Fd()), int(os.Stderr.Fd())); restoreErr != nil {
		c.logf("crash monitor stopped (%v) and standard error could not be restored: %v", err, restoreErr)
		return
	}
	w.Close()
	_, _ = io.Copy(original, r)
	r.Close()
	c.logf("crash monitor stopped unexpectedly (%v); crashes will not be reported", err)
}
