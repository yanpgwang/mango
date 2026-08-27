package main

import (
	"testing"

	"github.com/yanpgwang/mango/examples/terminal-ui/internal/endpoint"
)

func TestResolveEndpointNormalizesCLIAndEnvironmentValues(t *testing.T) {
	for input, expected := range map[string]string{
		"localhost:8080":         "http://localhost:8080",
		"https://mango.example/": "https://mango.example",
	} {
		got, err := resolveEndpoint(input, false)
		if err != nil || got != expected {
			t.Fatalf("resolveEndpoint(%q)=%q err=%v", input, got, err)
		}
	}
	if _, err := resolveEndpoint("ftp://mango.example", false); err == nil {
		t.Fatal("ftp endpoint was accepted")
	}
	if got, err := resolveEndpoint("ignored", true); err != nil || got != endpoint.DemoURL {
		t.Fatalf("demo endpoint=%q err=%v", got, err)
	}
}
