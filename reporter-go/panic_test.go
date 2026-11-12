package reporter

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// clock returns times from a list, then repeats the last one.
func clock(times ...time.Time) func() time.Time {
	return func() time.Time {
		t := times[0]
		if len(times) > 1 {
			times = times[1:]
		}
		return t
	}
}

func TestScanForCrash(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	crash := "panic: boom\n\ngoroutine 1 [running]:\nmain.main()\n\t/src/main.go:3 +0x1d\n"
	tests := []struct {
		name  string
		input string
		times []time.Time
		want  string
	}{
		{"crash then exit", "starting\n" + crash, []time.Time{t0}, crash},
		{"fatal error", "fatal error: concurrent map writes\n", []time.Time{t0}, "fatal error: concurrent map writes\n"},
		{"no crash", "starting\nstopping\n", []time.Time{t0}, ""},
		{"header mid-line is ignored", "recovered from panic: boom\n", []time.Time{t0}, ""},
		{"kept running after the header", "panic: logged text\nmore output\n", []time.Time{t0, t0, t0.Add(5 * time.Second)}, ""},
		{"exited long after the header", "panic: logged text\n", []time.Time{t0, t0.Add(5 * time.Second)}, ""},
		{"partial line before the header", "no newline" + crash, []time.Time{t0}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got := scanForCrash(strings.NewReader(tt.input), &out, clock(tt.times...))
			if got != tt.want {
				t.Errorf("crash = %q, want %q", got, tt.want)
			}
			if out.String() != tt.input {
				t.Errorf("output %q differs from input", out.String())
			}
		})
	}
}

func TestScanForCrashPassesLongLinesThrough(t *testing.T) {
	long := strings.Repeat("x", 200<<10) + "\n" + "panic: after a long line\n"
	var out bytes.Buffer
	got := scanForCrash(strings.NewReader(long), &out, clock(time.Now()))
	if out.String() != long {
		t.Error("long line was not copied through unchanged")
	}
	if got != "panic: after a long line\n" {
		t.Errorf("crash = %q", got)
	}
}

func TestMonitorNeverStartsInTests(t *testing.T) {
	c := newClient()
	c.startPanicMonitor() // must return at once instead of re-running the tests
}
