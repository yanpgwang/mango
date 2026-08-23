package main

import (
	"flag"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/demo"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/endpoint"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/mango"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/ui"
)

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	savedURL, _ := endpoint.Load()
	defaultURL := os.Getenv("MANGO_URL")
	if defaultURL == "" {
		defaultURL = savedURL
	}
	if defaultURL == "" {
		defaultURL = endpoint.DefaultURL
	}
	flags := flag.NewFlagSet("mango-tui", flag.ExitOnError)
	baseURL := flags.String("url", defaultURL, "Mango API base URL")
	apiKey := flags.String("api-key", os.Getenv("MANGO_API_KEY"), "Mango API key")
	demoMode := flags.Bool("demo", false, "Explore the interface without a Mango server")
	notify := flags.String("notify", envOr("MANGO_NOTIFY", "disabled"), "Background notifications: disabled, bell, or osc777")
	noMotion := flags.Bool("no-motion", envBool("MANGO_NO_MOTION"), "Disable decorative animation")
	showVersion := flags.Bool("version", false, "Print the terminal UI version")
	_ = flags.Parse(os.Args[1:])

	attachID := ""
	arguments := flags.Args()
	if *showVersion || (len(arguments) == 1 && arguments[0] == "version") {
		fmt.Printf("mango-tui %s (%s, %s)\n", version, commit, buildDate)
		return
	}
	if len(arguments) > 0 {
		if arguments[0] != "attach" || len(arguments) != 2 {
			usage()
			os.Exit(2)
		}
		attachID = arguments[1]
	}

	selectedEndpoint, err := resolveEndpoint(*baseURL, *demoMode)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mango-tui:", err)
		os.Exit(2)
	}
	backendForEndpoint := func(target string) (mango.Backend, error) {
		if target == endpoint.DemoURL {
			return demo.New(), nil
		}
		return mango.New(mango.Config{BaseURL: target, APIKey: *apiKey})
	}
	backend, err := backendForEndpoint(selectedEndpoint)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mango-tui:", err)
		os.Exit(2)
	}
	discovered := endpoint.Discover(selectedEndpoint, os.Getenv("MANGO_URL"), savedURL, *demoMode)
	endpointOptions := make([]ui.EndpointOption, 0, len(discovered))
	for _, candidate := range discovered {
		endpointOptions = append(endpointOptions, ui.EndpointOption{
			URL: candidate.URL, Label: candidate.Label, Source: candidate.Source,
			Available: candidate.Available, SkipProbe: candidate.SkipProbe,
		})
	}

	notificationMode := ui.NotificationMode(*notify)
	switch notificationMode {
	case ui.NotificationDisabled, ui.NotificationBell, ui.NotificationOSC777:
	default:
		fmt.Fprintln(os.Stderr, "mango-tui: --notify must be disabled, bell, or osc777")
		os.Exit(2)
	}
	program := tea.NewProgram(ui.NewWithOptions(backend, attachID, ui.Options{
		Notifications:      notificationMode,
		ReducedMotion:      *noMotion,
		Endpoint:           selectedEndpoint,
		Endpoints:          endpointOptions,
		BackendForEndpoint: backendForEndpoint,
		ProbeEndpoint:      endpoint.Probe,
		SaveEndpoint:       endpoint.Save,
	}))
	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "mango-tui:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mango-tui [--demo] [--url URL] [--api-key KEY] [--notify MODE] [--no-motion] [attach SESSION_ID]")
}

func envBool(name string) bool {
	switch os.Getenv(name) {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	default:
		return false
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func resolveEndpoint(value string, demoMode bool) (string, error) {
	if demoMode {
		return endpoint.DemoURL, nil
	}
	return endpoint.Normalize(value)
}
