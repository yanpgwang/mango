package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/yanpgwang/mango/examples/terminal-ui/internal/demo"
)

func TestEndpointListGroupsBySource(t *testing.T) {
	model := NewWithOptions(demo.New(), "", Options{
		Endpoint: "http://127.0.0.1:8080",
		Endpoints: []EndpointOption{
			{URL: "http://127.0.0.1:8080", Source: "configured", Available: true},
			{URL: "https://team.example.com", Source: "manual"},
			{URL: "demo://local", Label: "Local demo", SkipProbe: true, Available: true},
		},
	})
	model.width, model.height = 100, 32
	model.resize()
	view := ansi.Strip(model.renderConnect())
	for _, want := range []string{"Configured", "Manual", "Demo", "Local demo"} {
		if !strings.Contains(view, want) {
			t.Fatalf("endpoint list missing group %q: %q", want, view)
		}
	}
}

func TestConnectScreenIsBorderlessFullscreen(t *testing.T) {
	model := NewWithOptions(demo.New(), "", Options{
		Endpoint:  "http://127.0.0.1:8080",
		Endpoints: []EndpointOption{{URL: "http://127.0.0.1:8080", Source: "configured"}},
	})
	model.width, model.height = 120, 40
	model.resize()
	view := model.renderConnect()
	if lipgloss.Width(view) != model.width || lipgloss.Height(view) != model.height {
		t.Fatalf("connect view = %dx%d, want fullscreen %dx%d",
			lipgloss.Width(view), lipgloss.Height(view), model.width, model.height)
	}
	if plain := ansi.Strip(view); strings.ContainsAny(plain, "┌┐└┘") {
		t.Fatalf("connect screen still draws a bordered frame: %q", plain)
	}
}

func TestConnectEnterOnManualRowOpensEditor(t *testing.T) {
	model := NewWithOptions(demo.New(), "", Options{
		Endpoint:  "http://127.0.0.1:8080",
		Endpoints: []EndpointOption{{URL: "http://127.0.0.1:8080", Source: "configured"}},
	})
	// One endpoint plus the trailing "enter another endpoint" row: moving down
	// past the endpoint lands on that row, where Enter opens the editor rather
	// than connecting.
	updated, _ := model.updateConnect(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	model = updated.(Model)
	updated, _ = model.updateConnect(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	model = updated.(Model)
	if !model.connect.editing || model.loading {
		t.Fatalf("enter on manual row editing=%v loading=%v", model.connect.editing, model.loading)
	}
}

func TestPixelCloudBlinksWhileDrifting(t *testing.T) {
	model := New(demo.New(), "")
	sawOpen, sawBlink := false, false
	for frame := 0; frame < 120; frame++ {
		model.motion = frame
		scene := ansi.Strip(model.cloudScene(58, false))
		if strings.Contains(scene, "●") {
			sawOpen = true
		}
		if strings.Contains(scene, "▬") {
			sawBlink = true
		}
	}
	if !sawOpen || !sawBlink {
		t.Fatalf("pixel cloud did not blink: open=%v blink=%v", sawOpen, sawBlink)
	}
}
