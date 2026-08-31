package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Check the deployment boundary offline. Actual daemon access, mount visibility,
// and restart behavior are covered separately by Docker service checks.
func TestLocalComposeDockerBoundary(t *testing.T) {
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
		if service.Environment[sandboxProviderEnv] != "docker" {
			t.Fatalf("%s does not select Docker", name)
		}
		if _, present := service.Environment["MANGO_ALLOW_UNSAFE_LOCAL_SANDBOX"]; present {
			t.Fatalf("%s retains unsafe host execution override", name)
		}
	}
	api := config.Services["api"]
	if api.User == "0:0" || api.User == "root" {
		t.Fatal("API must retain the non-root image user")
	}
	if _, present := api.Environment["MANGO_MODEL_API_KEY"]; present {
		t.Fatal("API must not receive model credentials")
	}
	worker := config.Services["worker"]
	if worker.User != "0:0" {
		t.Fatal("worker socket access must have an explicit permission strategy")
	}
	if worker.Environment["DOCKER_HOST"] != "unix:///var/run/docker.sock" {
		t.Fatal("worker must use its mounted Unix socket")
	}
	var socket, resources bool
	for name, service := range config.Services {
		for _, volume := range service.Volumes {
			if volume.Kind != yaml.MappingNode {
				if strings.Contains(volume.Value, "docker.sock") {
					t.Fatal("socket mounts must use the explicit worker-only bind declaration")
				}
				continue
			}
			var mount struct {
				Type   string `yaml:"type"`
				Source string `yaml:"source"`
				Target string `yaml:"target"`
				Bind   struct {
					CreateHostPath *bool `yaml:"create_host_path"`
				} `yaml:"bind"`
			}
			if err := volume.Decode(&mount); err != nil {
				t.Fatal(err)
			}
			if mount.Target == "/var/run/docker.sock" {
				if name != "worker" || mount.Type != "bind" ||
					mount.Bind.CreateHostPath == nil || *mount.Bind.CreateHostPath {
					t.Fatalf("%s socket mount must be worker-only and reject a missing host socket", name)
				}
				socket = true
			}
			if name == "worker" && mount.Target == worker.Environment[sandboxResourceDirEnv] {
				if mount.Type != "bind" || mount.Source != mount.Target || mount.Source == "" {
					t.Fatal("worker and daemon must see the same resource bind path")
				}
				resources = true
			}
		}
	}
	if !socket || !resources {
		t.Fatalf("missing Docker mounts: socket=%v resources=%v", socket, resources)
	}
}
