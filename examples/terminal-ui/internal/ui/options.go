package ui

import (
	"context"

	"github.com/yanpgwang/mango/examples/terminal-ui/internal/mango"
)

// NotificationMode controls out-of-focus terminal notifications. Escape
// sequence notifications are opt-in because unsupported terminals may print
// them literally.
type NotificationMode string

const (
	NotificationDisabled NotificationMode = "disabled"
	NotificationBell     NotificationMode = "bell"
	NotificationOSC777   NotificationMode = "osc777"
)

// Options contains terminal-specific behavior that should be explicit rather
// than guessed from environment variables inside the UI model.
type Options struct {
	Notifications NotificationMode
	ReducedMotion bool
	// Endpoint is the initially selected endpoint. Authentication still belongs
	// to the CLI/config layer until Mango exposes a real identity flow.
	Endpoint string
	// Endpoints contains configured and automatically discovered connection
	// targets. The UI always preserves Endpoint even when it is not in this list.
	Endpoints []EndpointOption
	// BackendForEndpoint lets the welcome flow switch clients only after the
	// user confirms an endpoint. A nil factory keeps the backend passed to New.
	BackendForEndpoint func(string) (mango.Backend, error)
	// ProbeEndpoint reports whether an endpoint is reachable. Probes run as
	// Bubble Tea commands and never block rendering.
	ProbeEndpoint func(context.Context, string) bool
	// SaveEndpoint persists a successful selection for the next launch.
	SaveEndpoint func(string) error
}

// EndpointOption is one item in the welcome screen's endpoint picker.
type EndpointOption struct {
	URL       string
	Label     string
	Source    string
	Available bool
	SkipProbe bool
}
