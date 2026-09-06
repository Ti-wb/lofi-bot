package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/obsevidence"
)

const usageText = `Usage:
  obs-stale-monitor capture --password-stdin --input NAME --a-basename FILE --b-basename FILE --output ABSOLUTE_PATH [options]
  obs-stale-monitor verify --trace ABSOLUTE_PATH
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stderr, usageText)
		return 2
	}
	switch args[0] {
	case "capture":
		return runCapture(ctx, args[1:], stdin, stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		_, _ = io.WriteString(stdout, usageText)
		return 0
	default:
		_, _ = io.WriteString(stderr, "E_USAGE\n")
		return 2
	}
}

func runCapture(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) int {
	flags := flag.NewFlagSet("capture", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var (
		passwordStdin bool
		host          string
		port          int
		input         string
		aBasename     string
		bBasename     string
		output        string
		maxBytes      int64
		maxDuration   time.Duration
	)
	flags.BoolVar(&passwordStdin, "password-stdin", false, "")
	flags.StringVar(&host, "host", "127.0.0.1", "")
	flags.IntVar(&port, "port", 4455, "")
	flags.StringVar(&input, "input", "", "")
	flags.StringVar(&aBasename, "a-basename", "", "")
	flags.StringVar(&bBasename, "b-basename", "", "")
	flags.StringVar(&output, "output", "", "")
	flags.Int64Var(&maxBytes, "max-bytes", obsevidence.DefaultMaxTraceBytes, "")
	flags.DurationVar(&maxDuration, "max-duration", obsevidence.DefaultMaxDuration, "")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(stdout, usageText)
			return 0
		}
		_, _ = io.WriteString(stderr, "E_USAGE\n")
		return 2
	}
	if !passwordStdin || flags.NArg() != 0 {
		_, _ = io.WriteString(stderr, "E_USAGE\n")
		return 2
	}

	password, err := readPassword(stdin)
	if err != nil {
		_, _ = io.WriteString(stderr, "E_PASSWORD\n")
		return 2
	}
	defer clearBytes(password)

	revision, modified := buildRevision()
	cfg := obsevidence.CaptureConfig{
		Host:        host,
		Port:        port,
		Password:    password,
		Input:       input,
		ABasename:   aBasename,
		BBasename:   bBasename,
		OutputPath:  output,
		MaxBytes:    maxBytes,
		MaxDuration: maxDuration,
		Revision:    revision,
		Modified:    modified,
		Ready: func() {
			_, _ = io.WriteString(stdout, "READY\n")
		},
	}
	if err := obsevidence.Capture(ctx, cfg); err != nil {
		switch {
		case errors.Is(err, obsevidence.ErrInvalidConfig):
			_, _ = io.WriteString(stderr, "E_CONFIG\n")
			return 2
		case errors.Is(err, obsevidence.ErrOBSVersion):
			_, _ = io.WriteString(stderr, "E_OBS_VERSION\n")
		default:
			_, _ = io.WriteString(stderr, "E_CAPTURE\n")
		}
		return 1
	}
	return 0
}

func runVerify(args []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var tracePath string
	flags.StringVar(&tracePath, "trace", "", "")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(stdout, usageText)
			return 0
		}
		_, _ = io.WriteString(stderr, "E_USAGE\n")
		return 2
	}
	if flags.NArg() != 0 || tracePath == "" {
		_, _ = io.WriteString(stderr, "E_USAGE\n")
		return 2
	}
	verdict, err := obsevidence.VerifyTrace(tracePath)
	if err != nil {
		if errors.Is(err, obsevidence.ErrNoCorrelation) {
			_, _ = io.WriteString(stderr, "E_NO_CORRELATION\n")
			return 1
		}
		_, _ = io.WriteString(stderr, "E_INVALID_TRACE\n")
		return 1
	}
	encoded, err := obsevidence.SanitizedVerdictJSON(verdict)
	if err != nil {
		_, _ = io.WriteString(stderr, "E_INVALID_TRACE\n")
		return 1
	}
	encoded = append(encoded, '\n')
	if _, err := stdout.Write(encoded); err != nil {
		_, _ = io.WriteString(stderr, "E_OUTPUT\n")
		return 1
	}
	return 0
}

func readPassword(reader io.Reader) ([]byte, error) {
	limited := io.LimitReader(reader, obsevidence.MaxPasswordBytes+1)
	value, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.New("read password")
	}
	if len(value) > obsevidence.MaxPasswordBytes {
		clearBytes(value)
		return nil, errors.New("password is too long")
	}
	return value, nil
}

func buildRevision() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown", false
	}
	revision := "unknown"
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	return revision, modified
}

func clearBytes(value []byte) {
	for idx := range value {
		value[idx] = 0
	}
}
