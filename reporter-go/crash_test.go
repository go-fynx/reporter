//go:build linux || darwin

package reporter_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-fynx/reporter/reporter-go/internal/testserver"
	reporterv1 "github.com/go-fynx/reporter/reporter-go/proto/reporter/v1"
)

var (
	buildOnce   sync.Once
	crasherPath string
	buildErr    error
)

// crasher builds testdata/crasher once per test run.
func crasher(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "reporter-crasher")
		if err != nil {
			buildErr = err
			return
		}
		crasherPath = filepath.Join(dir, "crasher")
		out, err := exec.Command("go", "build", "-o", crasherPath, "./testdata/crasher").CombinedOutput()
		if err != nil {
			buildErr = errors.New(string(out))
		}
	})
	if buildErr != nil {
		t.Fatalf("build crasher: %v", buildErr)
	}
	return crasherPath
}

type crashRun struct {
	exitCode int
	stderr   string
	events   []*reporterv1.Event
}

// runCrasher runs the helper in mode with the SDK pointed at a fake service,
// and returns once both the program and its crash monitor have exited.
func runCrasher(t *testing.T, mode string, extraEnv ...string) crashRun {
	t.Helper()
	srv := testserver.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, crasher(t), mode)
	cmd.Env = append(os.Environ(),
		"REPORTER_API_KEY=test-key",
		"REPORTER_ENDPOINT="+srv.URL,
		"REPORTER_SERVICE=crasher",
		"REPORTER_ENVIRONMENT=test",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// The monitor shares the program's standard error, so Wait returns only
	// after the monitor has reported the crash and exited as well.
	err := cmd.Run()
	run := crashRun{stderr: stderr.String(), events: srv.Events()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		run.exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run crasher %s: %v\n%s", mode, err, run.stderr)
	}
	return run
}

func TestCrashIsReported(t *testing.T) {
	run := runCrasher(t, "panic")
	if run.exitCode != 2 {
		t.Errorf("exit code %d, want Go's 2 for a panic", run.exitCode)
	}
	for _, want := range []string{"crasher: started", "panic: something went wrong", "goroutine 1 [running]:"} {
		if !strings.Contains(run.stderr, want) {
			t.Errorf("standard error lacks %q:\n%s", want, run.stderr)
		}
	}
	if len(run.events) != 1 {
		t.Fatalf("%d events, want 1\n%s", len(run.events), run.stderr)
	}
	e := run.events[0]
	if e.GetErrorType() != "panic" || e.GetMessage() != "something went wrong" || !e.GetUnhandled() ||
		e.GetSeverity() != reporterv1.Severity_SEVERITY_ERROR {
		t.Errorf("event = %v", e)
	}
	stack := e.GetStack()
	if len(stack) < 2 || stack[0].GetFunction() != "main.crash" || stack[1].GetFunction() != "main.main" ||
		!stack[0].GetInProject() || !strings.HasSuffix(stack[0].GetFile(), "main.go") || stack[0].GetLine() == 0 {
		t.Errorf("stack = %v", stack)
	}
}

func TestCrashKinds(t *testing.T) {
	tests := []struct {
		mode, kind, message, top string
	}{
		// Closures are named main.main.funcN in source order; only the prefix
		// matters here.
		{"error", "panic", "loading config: file not found", "main.main"},
		{"nil-map", "panic", "assignment to entry in nil map", "main.main"},
		{"nil-pointer", "panic", "runtime error: invalid memory address or nil pointer dereference", "main.main"},
		{"goroutine", "panic", "in goroutine", "main.main.func"},
		{"repanic", "panic", "again: first", "main.main.func"},
		{"fatal", "fatal error", "concurrent map writes", "main.main.func"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			run := runCrasher(t, tt.mode)
			if run.exitCode != 2 || len(run.events) != 1 {
				t.Fatalf("exit %d, %d events\n%s", run.exitCode, len(run.events), run.stderr)
			}
			e := run.events[0]
			if e.GetErrorType() != tt.kind || e.GetMessage() != tt.message {
				t.Errorf("type %q message %q", e.GetErrorType(), e.GetMessage())
			}
			if top := e.GetStack()[0].GetFunction(); !strings.HasPrefix(top, tt.top) {
				t.Errorf("top frame %q, want %q", top, tt.top)
			}
		})
	}
}

func TestNoEventWithoutACrash(t *testing.T) {
	for mode, wantExit := range map[string]int{"ok": 0, "exit": 3} {
		t.Run(mode, func(t *testing.T) {
			run := runCrasher(t, mode)
			if run.exitCode != wantExit || len(run.events) != 0 {
				t.Errorf("exit %d with %d events, want exit %d and none\n%s", run.exitCode, len(run.events), wantExit, run.stderr)
			}
		})
	}
}

func TestCrashMonitorCanBeTurnedOff(t *testing.T) {
	run := runCrasher(t, "panic", "REPORTER_DISABLE_PANIC_HANDLER=1")
	if run.exitCode != 2 || len(run.events) != 0 || !strings.Contains(run.stderr, "panic: something went wrong") {
		t.Errorf("exit %d with %d events\n%s", run.exitCode, len(run.events), run.stderr)
	}
}

// TestShutdownSignalDoesNotKillTheProgram covers Ctrl-C and systemd, which
// signal the program and its monitor together. If the monitor died first,
// the program would be killed by SIGPIPE on its next write to standard error.
func TestShutdownSignalDoesNotKillTheProgram(t *testing.T) {
	srv := testserver.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, crasher(t), "signal")
	cmd.Env = append(os.Environ(), "REPORTER_API_KEY=k", "REPORTER_ENDPOINT="+srv.URL, "REPORTER_SERVICE=crasher")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("waiting for ready: %q %v", line, err)
	}

	// Signal the whole process group, as a terminal or systemd would.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	if err != nil {
		t.Fatalf("program did not exit cleanly: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "crasher: stopped") {
		t.Errorf("shutdown output incomplete:\n%s", stderr.String())
	}
	if len(srv.Events()) != 0 {
		t.Errorf("a clean shutdown produced %d events", len(srv.Events()))
	}
}

// TestProgramSurvivesTheMonitorDying kills the monitor while the program is
// running. The program must get its standard error back and lose nothing.
func TestProgramSurvivesTheMonitorDying(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc to find the monitor process")
	}
	srv := testserver.New(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, crasher(t), "signal")
	cmd.Env = append(os.Environ(), "REPORTER_API_KEY=k", "REPORTER_ENDPOINT="+srv.URL, "REPORTER_SERVICE=crasher")
	var stderr syncBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("waiting for ready: %q %v", line, err)
	}

	monitor := childOf(t, cmd.Process.Pid)
	if err := syscall.Kill(monitor, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the program to log that its monitor stopped", func() bool {
		return strings.Contains(stderr.String(), "crash monitor stopped unexpectedly")
	})
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("program did not exit cleanly: %v\n%s", err, stderr.String())
	}
	for _, want := range []string{"crasher: started", "crasher: shutting down 4", "crasher: stopped"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("standard error lacks %q:\n%s", want, stderr.String())
		}
	}
}

// childOf waits for pid's child process to appear and returns it.
func childOf(t *testing.T, pid int) int {
	t.Helper()
	var child int
	waitFor(t, "the monitor process to start", func() bool {
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			id, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
			if err != nil {
				continue
			}
			// Fields after the command name, which is in parentheses: state, ppid, ...
			fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
			if len(fields) > 1 && fields[1] == strconv.Itoa(pid) {
				child = id
				return true
			}
		}
		return false
	})
	return child
}

// syncBuffer is a bytes.Buffer that is safe to read while a process writes
// to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
