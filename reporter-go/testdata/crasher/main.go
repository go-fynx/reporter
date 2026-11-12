// Command crasher is a helper for the SDK's crash-reporting tests. Unless
// CRASHER_NO_SDK is set it configures the SDK from REPORTER_* environment
// variables, which starts the crash monitor, and then does what its argument
// says.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	reporter "github.com/go-fynx/reporter/reporter-go"
)

type custom struct{ Code int }

func main() {
	if os.Getenv("CRASHER_NO_SDK") == "" {
		reporter.Configure(reporter.Configuration{})
	}
	fmt.Fprintln(os.Stderr, "crasher: started")

	switch os.Args[1] {
	case "panic":
		crash()
	case "error":
		panic(fmt.Errorf("loading config: %w", errors.New("file not found")))
	case "nil-map":
		var m map[string]int
		m["x"] = 1
	case "index":
		s := []int{1}
		_ = s[len(os.Args)+5]
	case "nil-pointer":
		fmt.Println(nothing().Code)
	case "custom":
		panic(custom{Code: 7})
	case "goroutine":
		go func() { panic("in goroutine") }()
		time.Sleep(time.Minute)
	case "repanic":
		func() {
			defer func() { panic(fmt.Sprint("again: ", recover())) }()
			panic("first")
		}()
	case "repanic-same":
		defer func() { panic(recover()) }()
		panic("same")
	case "multiline":
		panic("line one\nline two")
	case "fatal":
		m := map[int]int{}
		for range 4 {
			go func() {
				for j := 0; ; j++ {
					m[j] = j
				}
			}()
		}
		time.Sleep(time.Minute)
	case "exit":
		os.Exit(3)
	case "ok":
		fmt.Fprintln(os.Stderr, "crasher: finished")
	case "signal":
		// Behaves like a service: wait for SIGTERM or Ctrl-C, then keep
		// writing to standard error while shutting down.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		for i := range 5 {
			fmt.Fprintln(os.Stderr, "crasher: shutting down", i)
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Fprintln(os.Stderr, "crasher: stopped")
	default:
		fmt.Fprintln(os.Stderr, "crasher: unknown mode", os.Args[1])
		os.Exit(64)
	}
}

//go:noinline
func crash() {
	panic("something went wrong")
}

//go:noinline
func nothing() *custom {
	return nil
}
