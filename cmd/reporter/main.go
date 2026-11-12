// Command reporter runs the reporter service, which receives log and error
// events from applications and stores them in MongoDB.
//
//	reporter serve -config /etc/reporter/config.json
//	reporter check-config -config /etc/reporter/config.json
//	reporter keygen -project booking
//	reporter version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-fynx/reporter/internal/app"
	"github.com/go-fynx/reporter/internal/auth"
	"github.com/go-fynx/reporter/internal/config"
	"github.com/go-fynx/reporter/internal/event"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage:
  reporter serve -config <file>         start the service
  reporter check-config -config <file>  validate a configuration file
  reporter keygen -project <name>       create an API key for a project
  reporter version                      print the version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "serve":
		return serve(rest, stdout, stderr)
	case "check-config":
		return checkConfig(rest, stdout, stderr)
	case "keygen":
		return keygen(rest, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

func configFlag(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "config.json", "path to the JSON configuration file")
	return fs, path
}

func serve(args []string, stdout, stderr io.Writer) int {
	fs, configPath := configFlag("serve", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: cfg.LogLevel()})).
		With("app", "reporter", "version", version)
	warnIfReadableByOthers(logger, *configPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, logger)
	if err != nil {
		logger.Error("startup failed", "error", err)
		return 1
	}
	if err := a.Serve(ctx); err != nil {
		logger.Error("service stopped with an error", "error", err)
		return 1
	}
	return 0
}

// warnIfReadableByOthers flags a config file every user on the machine can
// read, since it may contain the MongoDB password. Group access is fine: the
// recommended setup is 0640 owned by root and the service's group.
func warnIfReadableByOthers(logger *slog.Logger, path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		logger.Warn("config file is readable by all users and may contain the MongoDB password",
			"path", path, "mode", perm.String(), "fix", "chmod o-rwx "+path)
	}
}

func checkConfig(args []string, stdout, stderr io.Writer) int {
	fs, configPath := configFlag("check-config", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	transport := "TLS"
	if cfg.Server.Insecure {
		transport = "plaintext (development only)"
	}
	fmt.Fprintf(stdout, "%s is valid: environment %s, listening on %s with %s, storage %s, %d API key(s)\n",
		*configPath, cfg.Environment, cfg.Server.ListenAddress, transport, cfg.Storage.Type, len(cfg.APIKeys))
	return 0
}

func keygen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	project := fs.String("project", "", "project the key belongs to (required)")
	id := fs.String("id", "", "name for the key in the config file (default <project>-<date>)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !event.ValidName(*project) {
		fmt.Fprintln(stderr, errors.New("-project is required: 1 to 100 letters, digits, '.', '_' or '-'"))
		return 2
	}
	if *id == "" {
		*id = *project + "-" + time.Now().UTC().Format("20060102")
	}

	apiKey, hash := auth.Generate()
	entry, err := json.MarshalIndent(config.APIKey{ID: *id, Project: *project, SHA256: hash}, "    ", "  ")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, `API key for project %q. It is shown only once; give it to the services that report for this project:

    %s

Add this entry to "api_keys" in the service's config file and restart the service:

    %s
`, *project, apiKey, entry)
	return 0
}
