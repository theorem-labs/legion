package ownerharness

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// The stand-ins' answers to a version request: they say what they are, so that a report of a run against them never
// passes for a run against the Legion daemon or omp.
const (
	LegionVersion = "legion harness: the owner message test's test-only stand-in for the supervisor, not the Legion daemon"
	OmpVersion    = "omp harness: the owner message test's test-only stand-in for an owner session, not omp"
)

// Main runs a stand-in under the name the test binary was linked as, writing its own output to stdout:
//
//	legion start --config FILE [--check-config] | legion version    the supervisor stand-in
//	omp --system-prompt-file FILE | omp --version                   a scripted owner session (it logs to ./session.log)
func Main(name string, args []string, stdout io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch name {
	case "legion":
		err = mainLegion(ctx, args, stdout)
	case "omp":
		err = mainOMP(ctx, args, stdout)
	default:
		err = fmt.Errorf("unknown stand-in %q: link the test binary as legion or omp", name)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return 1
	}
	return 0
}

func mainLegion(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 1 && args[0] == "version" {
		_, err := fmt.Fprintln(stdout, LegionVersion)
		return err
	}
	if len(args) == 0 || args[0] != "start" {
		return fmt.Errorf("usage: legion start --config FILE [--check-config] | legion version")
	}
	path, check := "", false
	for rest := args[1:]; len(rest) > 0; rest = rest[1:] {
		switch {
		case rest[0] == "--check-config":
			check = true
		case rest[0] == "--config" && len(rest) > 1:
			path, rest = rest[1], rest[1:]
		default:
			return fmt.Errorf("usage: legion start --config FILE [--check-config]")
		}
	}
	config, err := LoadConfig(path, os.Getenv)
	if err != nil {
		return err
	}
	if check {
		_, err := fmt.Fprintf(stdout, "Config OK: project=%s\n", config.Project)
		return err
	}
	return Supervise(ctx, config, os.Stderr)
}

func mainOMP(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 1 && args[0] == "--version" {
		_, err := fmt.Fprintln(stdout, OmpVersion)
		return err
	}
	if len(args) != 2 || args[0] != "--system-prompt-file" {
		return fmt.Errorf("usage: omp --system-prompt-file FILE | omp --version")
	}
	log, err := os.OpenFile("session.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	session, err := NewSession(args[1], os.Getenv, io.MultiWriter(log, os.Stderr))
	if err != nil {
		return err
	}
	return session.Run(ctx)
}
