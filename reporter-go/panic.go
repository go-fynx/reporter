package reporter

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// Crash reporting works like bugsnag-go's: the program runs itself a second
// time as a monitor process and sends its standard error through a pipe to
// that monitor. The monitor copies everything to the real standard error and,
// when the program exits right after printing a Go crash report, sends that
// report as an event.
//
// Unlike bugsnag-go, which uses github.com/bugsnag/panicwrap, the program
// keeps track of its monitor. If the monitor dies, the program gets its
// standard error back instead of writing into a pipe nobody reads.

const (
	// The environment variable that tells a process it is the monitor. It is
	// not panicwrap's, so it cannot be confused with bugsnag-go's monitor.
	monitorCookieKey   = "REPORTER_GO_CRASH_MONITOR"
	monitorCookieValue = "8d1c4f6e2b7a4093"

	// crashWindow is how soon after a crash header the program must exit for
	// the text to count as a crash. The Go runtime exits immediately after
	// printing one; a program that merely logs "panic: ..." keeps running.
	crashWindow = time.Second

	// maxCrashText bounds how much of a crash report is kept.
	maxCrashText = 1 << 20

	// bugsnag-go also re-runs the program as a crash monitor, through
	// github.com/bugsnag/panicwrap, which marks its monitor with this
	// environment variable. When both SDKs run in one program, each monitor
	// would otherwise start the other SDK's monitor too, and crashes would
	// be reported twice.
	panicwrapCookieKey   = "cccf35992f8f3cd8d1d28f0109dd953e26664531"
	panicwrapCookieValue = "7c28215aca87789f95b406b8dd91aa5198406750"
)

// startPanicMonitor is the default PanicHandler. In the program it starts the
// monitor; in the monitor it watches the program and never returns.
func (c *client) startPanicMonitor() {
	if !monitorAvailable || testing.Testing() {
		// A test binary would re-run its tests in the monitor.
		return
	}
	if os.Getenv(monitorCookieKey) == monitorCookieValue {
		ignoreShutdownSignals()
		if crash := scanForCrash(os.Stdin, os.Stderr, time.Now); crash != "" {
			c.reportCrash(crash)
		}
		os.Exit(0)
	}
	if os.Getenv(panicwrapCookieKey) == panicwrapCookieValue {
		// This process is bugsnag-go's crash monitor, not the program.
		return
	}
	if err := c.spawnMonitor(); err != nil {
		c.logf("could not start crash reporting: %v", err)
	}
}

// scanForCrash copies in to out until in ends, which happens when the program
// exits. It returns the crash report if the program exited within crashWindow
// of printing a line that starts with "panic: " or "fatal error: ".
func scanForCrash(in io.Reader, out io.Writer, now func() time.Time) string {
	r := bufio.NewReaderSize(in, 64<<10)
	var crash bytes.Buffer
	var crashStarted time.Time
	tracking, atLineStart := false, true
	for {
		line, err := r.ReadSlice('\n')
		if len(line) > 0 {
			_, _ = out.Write(line)
			t := now()
			if tracking && t.Sub(crashStarted) > crashWindow {
				tracking = false // the program kept running, so it was not a crash
				crash.Reset()
			}
			if !tracking && atLineStart && (bytes.HasPrefix(line, []byte("panic: ")) || bytes.HasPrefix(line, []byte("fatal error: "))) {
				tracking, crashStarted = true, t
			}
			if tracking && crash.Len() < maxCrashText {
				crash.Write(line)
			}
			atLineStart = line[len(line)-1] == '\n'
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			break
		}
	}
	if !tracking || now().Sub(crashStarted) > crashWindow {
		return ""
	}
	return crash.String()
}

// reportCrash runs in the monitor process after the program crashed.
func (c *client) reportCrash(output string) {
	crash, ok := parsePanic(output)
	if !ok {
		crash = parsedPanic{kind: "panic", message: strings.TrimSpace(output)}
	}
	_ = c.notify(notice{
		errorType:   crash.kind,
		message:     crash.message,
		severity:    SeverityError,
		unhandled:   true,
		synchronous: true,
		frames:      crash.frames,
	})
}
