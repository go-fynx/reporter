package reporter

import (
	"strconv"
	"strings"

	reporterv1 "github.com/go-fynx/reporter/reporter-go/proto/reporter/v1"
)

// parsedPanic is what can be recovered from the text Go prints when a
// program crashes.
type parsedPanic struct {
	kind    string // "panic" or "fatal error"
	message string
	frames  []*reporterv1.StackFrame
}

// parsePanic reads a crash report such as:
//
//	panic: first [recovered]
//		panic: second
//	[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x49d23a]
//
//	goroutine 1 [running]:
//	main.main.func1()
//		/src/main.go:35 +0x5a
//	panic({0x55f018?, 0x4a79c0?})
//		/usr/local/go/src/runtime/panic.go:859 +0x125
//	main.main()
//		/src/main.go:37 +0x23a
//
// The message is the last panic in the chain; continuation lines of a
// multi-line message are indented with a tab. The frames are those of the
// first goroutine listed, which is the one that crashed, without runtime
// frames at the top or "panic" frames anywhere.
func parsePanic(text string) (parsedPanic, bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	i := 0
	for i < len(lines) && headerKind(lines[i]) == "" {
		i++
	}
	if i == len(lines) {
		return parsedPanic{}, false
	}

	kind := headerKind(lines[i])
	p := parsedPanic{kind: kind}
	message := []string{strings.TrimPrefix(lines[i], kind+": ")}
	for i++; i < len(lines) && strings.HasPrefix(lines[i], "\t"); i++ {
		if later, ok := strings.CutPrefix(lines[i], "\tpanic: "); ok {
			message = []string{later}
		} else {
			message = append(message, lines[i][1:])
		}
	}
	p.message = strings.TrimSpace(trimRecovered(strings.Join(message, "\n")))

	for i < len(lines) && !strings.HasPrefix(lines[i], "goroutine ") {
		i++
	}
	for i++; i+1 < len(lines); i += 2 {
		call, location := lines[i], lines[i+1]
		if call == "" || strings.HasPrefix(call, "created by ") || strings.HasPrefix(call, "goroutine ") {
			break
		}
		if strings.HasPrefix(call, "...") {
			// "...additional frames elided..." is a single line.
			i--
			continue
		}
		if !strings.HasPrefix(location, "\t") {
			break
		}
		f := parseFrame(call, location)
		if f.Function == "panic" || (len(p.frames) == 0 && isRuntime(f.Function)) {
			continue
		}
		p.frames = append(p.frames, f)
		if len(p.frames) == maxFrames {
			break
		}
	}
	return p, true
}

func headerKind(line string) string {
	switch {
	case strings.HasPrefix(line, "panic: "):
		return "panic"
	case strings.HasPrefix(line, "fatal error: "):
		return "fatal error"
	default:
		return ""
	}
}

// trimRecovered removes the markers Go adds to panics that were recovered and
// raised again.
func trimRecovered(message string) string {
	for _, suffix := range []string{" [recovered, repanicked]", " [recovered]"} {
		if trimmed, ok := strings.CutSuffix(message, suffix); ok {
			return trimmed
		}
	}
	return message
}

// parseFrame reads one frame: a call such as "main.(*T).Run(0x1, ...)" and a
// location such as "\t/src/main.go:12 +0x1d".
func parseFrame(call, location string) *reporterv1.StackFrame {
	function := call
	if i := strings.LastIndex(function, "("); i > 0 {
		function = function[:i]
	}
	loc := strings.TrimPrefix(location, "\t")
	if i := strings.Index(loc, " +0x"); i >= 0 {
		loc = loc[:i]
	}
	file, line := loc, 0
	if i := strings.LastIndex(loc, ":"); i >= 0 {
		if n, err := strconv.Atoi(loc[i+1:]); err == nil {
			file, line = loc[:i], n
		}
	}
	return &reporterv1.StackFrame{Function: validUTF8(function), File: validUTF8(file), Line: int32(line)}
}
