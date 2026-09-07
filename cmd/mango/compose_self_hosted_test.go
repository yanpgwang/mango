package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The control plane must not receive a Docker socket or select a sandbox
// backend. Operator-launched Environment workers own those credentials.
func TestLocalComposeSelfHostedBoundary(t *testing.T) {
	body, err := os.ReadFile("../../deployments/local/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Services map[string]struct {
			User        string            `yaml:"user"`
			Environment map[string]string `yaml:"environment"`
			Volumes     []yaml.Node       `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"api", "worker"} {
		service := config.Services[name]
		for key := range service.Environment {
			if strings.HasPrefix(key, "MANGO_SANDBOX") || key == "DOCKER_HOST" {
				t.Fatalf("%s retains control-plane sandbox setting %s", name, key)
			}
		}
		for _, volume := range service.Volumes {
			if strings.Contains(volume.Value, "docker.sock") {
				t.Fatalf("%s receives the Docker socket", name)
			}
			var mount struct {
				Source string `yaml:"source"`
				Target string `yaml:"target"`
			}
			if volume.Kind == yaml.MappingNode {
				if err := volume.Decode(&mount); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(mount.Source, "docker.sock") || strings.Contains(mount.Target, "docker.sock") {
					t.Fatalf("%s receives the Docker socket", name)
				}
			}
		}
	}
	if config.Services["api"].User == "0:0" || config.Services["api"].User == "root" {
		t.Fatal("API must retain the non-root image user")
	}
	if _, present := config.Services["api"].Environment["MANGO_MODEL_API_KEY"]; present {
		t.Fatal("API must not receive model credentials")
	}
}
