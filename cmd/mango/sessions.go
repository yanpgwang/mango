package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unicode"

	"github.com/yanpgwang/mango/internal/sessionconnect"
	mango "github.com/yanpgwang/mango/sdk/go"
)

func runSessionsCommand() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := sessionsCommand(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		// HTTP diagnostics can contain remote text; keep terminal controls out of it.
		message := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) && r != '\n' && r != '\t' {
				return -1
			}
			return r
		}, err.Error())
		_, _ = fmt.Fprintln(os.Stderr, "sessions:", message)
		os.Exit(1)
	}
}

func sessionsCommand(ctx context.Context, args []string, input io.ReadCloser, output, diagnostics io.Writer) error {
	const usage = "usage: mango sessions connect SESSION_ID [flags]"
	if len(args) == 0 || args[0] != "connect" {
		return errors.New(usage)
	}
	baseURL := strings.TrimSpace(os.Getenv("MANGO_BASE_URL"))
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	fs := flag.NewFlagSet("sessions connect", flag.ContinueOnError)
	fs.SetOutput(diagnostics)
	fs.StringVar(&baseURL, "base-url", baseURL, "Mango HTTP endpoint (MANGO_BASE_URL)")
	readOnly := fs.Bool("read-only", false, "follow history and live events without reading input")
	verbose := fs.Bool("verbose", false, "show complete event JSON")
	fs.Usage = func() { _, _ = fmt.Fprintln(diagnostics, usage); fs.PrintDefaults() }
	if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
		fs.Usage()
		return nil
	}
	if len(args) < 2 || !strings.HasPrefix(args[1], "sesn_") {
		return errors.New(usage)
	}
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(usage)
	}
	key := strings.TrimSpace(os.Getenv("MANGO_API_KEY"))
	if key == "" {
		return errors.New("MANGO_API_KEY is required (Workspace API key)")
	}
	client, err := mango.New(mango.Config{BaseURL: baseURL, APIKey: key})
	if err != nil {
		return err
	}
	return sessionconnect.Run(ctx, sessionconnect.Options{Client: client, SessionID: args[1], Input: input, Output: output, ReadOnly: *readOnly, Verbose: *verbose})
}
