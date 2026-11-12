package reporter

import (
	"path"
	"reflect"
	"runtime"
	"strings"

	reporterv1 "github.com/go-fynx/reporter/reporter-go/proto/reporter/v1"
)

// sdkPrefix starts the function name of every function in this package.
var sdkPrefix = reflect.TypeFor[Notifier]().PkgPath() + "."

// stackSource is a stack waiting to be resolved: program counters, or frames
// already parsed from a crash.
type stackSource struct {
	pcs    []uintptr
	frames []*reporterv1.StackFrame
	// fromCallSite means pcs were recorded inside the SDK, whose leading
	// frames must be removed.
	fromCallSite bool
	// panicking means pcs were recorded while recovering a panic, so leading
	// runtime frames are removed too and the stack starts where it panicked.
	panicking bool
}

func (s stackSource) resolve(cfg Configuration) []*reporterv1.StackFrame {
	if s.frames != nil {
		for _, f := range s.frames {
			pkg := funcPackage(f.Function)
			f.Function = displayName(f.Function)
			f.File = trimPath(f.File, pkg, cfg.SourceRoot)
			f.InProject = inProject(pkg, cfg.ProjectPackages)
		}
		return s.frames
	}
	if len(s.pcs) == 0 {
		return nil
	}

	var out []*reporterv1.StackFrame
	skipping := s.fromCallSite
	frames := runtime.CallersFrames(s.pcs)
	for {
		f, more := frames.Next()
		if skipping && (strings.HasPrefix(f.Function, sdkPrefix) || (s.panicking && isRuntime(f.Function))) {
			if !more {
				break
			}
			continue
		}
		skipping = false
		if f.Function != "" || f.File != "" {
			pkg := funcPackage(f.Function)
			out = append(out, &reporterv1.StackFrame{
				Function:  validUTF8(displayName(f.Function)),
				File:      validUTF8(trimPath(f.File, pkg, cfg.SourceRoot)),
				Line:      int32(f.Line),
				InProject: inProject(pkg, cfg.ProjectPackages),
			})
		}
		if !more || len(out) == maxFrames {
			break
		}
	}
	return out
}

func isRuntime(function string) bool {
	return strings.HasPrefix(function, "runtime.") || strings.HasPrefix(function, "internal/runtime/") || function == "panic"
}

// funcPackage returns the import path of a function's package:
// "github.com/acme/app/db.(*Store).Get" belongs to "github.com/acme/app/db".
// Go writes dots in the last element of a package path as "%2e"
// ("gopkg.in/yaml%2ev3.Unmarshal"), so the first dot after the last slash
// always ends the package.
func funcPackage(function string) string {
	slash := strings.LastIndex(function, "/")
	dot := strings.Index(function[slash+1:], ".")
	if dot < 0 {
		return displayName(function)
	}
	return displayName(function[:slash+1+dot])
}

// displayName undoes Go's "%2e" escaping of dots in package paths.
func displayName(function string) string {
	return strings.ReplaceAll(function, "%2e", ".")
}

// inProject reports whether pkg matches one of the ProjectPackages patterns.
// A pattern ending in "/**" matches that package and everything below it;
// other patterns use path.Match, so "main*" matches main.
func inProject(pkg string, patterns []string) bool {
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "/**"); ok {
			if pkg == prefix || strings.HasPrefix(pkg, prefix+"/") {
				return true
			}
			continue
		}
		if ok, _ := path.Match(p, pkg); ok {
			return true
		}
	}
	return false
}

// trimPath shortens a frame's file path the way bugsnag-go does: it removes
// sourceRoot, or else cuts everything before the package's import path, so
// "/home/u/go/pkg/mod/github.com/x/y@v1/z.go" becomes "github.com/x/y@v1/z.go".
func trimPath(file, pkg, sourceRoot string) string {
	if sourceRoot != "" && strings.HasPrefix(file, sourceRoot) {
		return strings.TrimPrefix(file[len(sourceRoot):], "/")
	}
	if pkg != "main" && pkg != "" {
		if i := strings.Index(file, pkg); i > 0 {
			return file[i:]
		}
	}
	return file
}
