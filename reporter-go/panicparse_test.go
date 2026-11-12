package reporter

import (
	"strings"
	"testing"
)

// These inputs are real Go 1.27 crash output, with paths shortened.
func TestParsePanic(t *testing.T) {
	tests := []struct {
		name, input string
		kind, msg   string
		functions   []string
	}{
		{
			name: "string",
			input: "panic: something went wrong\n\ngoroutine 1 [running]:\nmain.crash(...)\n\t/src/main.go:17 +0x254\n" +
				"main.main()\n\t/src/main.go:9 +0x25\n",
			kind: "panic", msg: "something went wrong",
			functions: []string{"main.crash", "main.main"},
		},
		{
			name: "nil pointer with signal line",
			input: "panic: runtime error: invalid memory address or nil pointer dereference\n" +
				"[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x49d23a]\n\n" +
				"goroutine 1 [running]:\nmain.main()\n\t/src/main.go:27 +0x1ba\n",
			kind: "panic", msg: "runtime error: invalid memory address or nil pointer dereference",
			functions: []string{"main.main"},
		},
		{
			name:  "custom value",
			input: "panic: (main.custom) 0x4a7860\n\ngoroutine 1 [running]:\nmain.main()\n\t/src/main.go:29 +0x267\n",
			kind:  "panic", msg: "(main.custom) 0x4a7860",
			functions: []string{"main.main"},
		},
		{
			name:  "multi-line message",
			input: "panic: line one\n\tline two\n\ngoroutine 1 [running]:\nmain.main()\n\t/src/main.go:42 +0x351\n",
			kind:  "panic", msg: "line one\nline two",
			functions: []string{"main.main"},
		},
		{
			name: "re-panic with a new value",
			input: "panic: first [recovered]\n\tpanic: again: first\n\ngoroutine 1 [running]:\n" +
				"main.main.func2.1()\n\t/src/main.go:35 +0x5a\n" +
				"panic({0x55f018?, 0x4a79c0?})\n\t/usr/local/go/src/runtime/panic.go:859 +0x125\n" +
				"main.main.func2()\n\t/src/main.go:36 +0x3e\nmain.main()\n\t/src/main.go:37 +0x23a\n",
			kind: "panic", msg: "again: first",
			functions: []string{"main.main.func2.1", "main.main.func2", "main.main"},
		},
		{
			name: "re-panic with the same value",
			input: "panic: same [recovered, repanicked]\n\ngoroutine 1 [running]:\nmain.main.func3()\n\t/src/main.go:39 +0x18\n" +
				"panic({0x55f018?, 0x4a7990?})\n\t/usr/local/go/src/runtime/panic.go:859 +0x125\nmain.main()\n\t/src/main.go:40 +0x33e\n",
			kind: "panic", msg: "same",
			functions: []string{"main.main.func3", "main.main"},
		},
		{
			name: "goroutine with created-by",
			input: "panic: in goroutine\n\ngoroutine 7 [running]:\nmain.main.func1()\n\t/src/main.go:31 +0x25\n" +
				"created by main.main in goroutine 1\n\t/src/main.go:31 +0xa7\n",
			kind: "panic", msg: "in goroutine",
			functions: []string{"main.main.func1"},
		},
		{
			name: "fatal error with runtime frame on top",
			input: "fatal error: concurrent map writes\n\ngoroutine 8 [running]:\n" +
				"internal/runtime/maps.fatal({0x4a101f?, 0x0?})\n\t/usr/local/go/src/runtime/panic.go:1195 +0x18\n" +
				"main.main.func4()\n\t/src/main.go:48 +0x2d\ncreated by main.main in goroutine 1\n\t/src/main.go:46 +0x35a\n" +
				"\ngoroutine 1 [sleep]:\ntime.Sleep(0x12a05f200)\n\t/usr/local/go/src/runtime/time.go:363 +0x165\n",
			kind: "fatal error", msg: "concurrent map writes",
			functions: []string{"main.main.func4"},
		},
		{
			name: "method with receiver, generics and arguments",
			input: "panic: x\n\ngoroutine 1 [running]:\ngithub.com/acme/app/db.(*Store[...]).Get(0xc000012345, {0x4b2e10, 0x3})\n" +
				"\t/src/db/db.go:88 +0x1d\nmain.main()\n\t/src/main.go:5 +0x2\n",
			kind: "panic", msg: "x",
			functions: []string{"github.com/acme/app/db.(*Store[...]).Get", "main.main"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := parsePanic(tt.input)
			if !ok {
				t.Fatal("not recognised as a crash")
			}
			if p.kind != tt.kind || p.message != tt.msg {
				t.Errorf("kind %q message %q", p.kind, p.message)
			}
			var functions []string
			for _, f := range p.frames {
				functions = append(functions, f.GetFunction())
			}
			if strings.Join(functions, " ") != strings.Join(tt.functions, " ") {
				t.Errorf("frames = %v, want %v", functions, tt.functions)
			}
		})
	}
}

func TestParsePanicFrameLocation(t *testing.T) {
	p, _ := parsePanic("panic: x\n\ngoroutine 1 [running]:\nmain.main()\n\t/src/app/main.go:27 +0x1ba\n")
	f := p.frames[0]
	if f.GetFile() != "/src/app/main.go" || f.GetLine() != 27 {
		t.Errorf("frame = %v", f)
	}
}

func TestParsePanicRejectsOtherText(t *testing.T) {
	for _, input := range []string{"", "just some output\n", "error: something\n"} {
		if _, ok := parsePanic(input); ok {
			t.Errorf("parsePanic(%q) recognised a crash", input)
		}
	}
}
