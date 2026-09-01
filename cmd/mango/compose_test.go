package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Check the deployment boundary offline. Actual command execution, mount
// visibility, and restart behavior are covered by OpenSandbox service checks.
func TestLocalComposeOpenSandboxBoundary(t *testing.T) {
	body, err := os.ReadFile("../../deployments/local/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Services map[string]struct {
			User        string            `yaml:"user"`
			Image       string            `yaml:"image"`
			Environment map[string]string `yaml:"environment"`
			Ports       []string          `yaml:"ports"`
			Volumes     []yaml.Node       `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"api", "worker"} {
		service := config.Services[name]
		for _, obsolete := range []string{"MANGO_SANDBOX", "DOCKER_HOST", "MANGO_ALLOW_UNSAFE_LOCAL_SANDBOX"} {
			if _, present := service.Environment[obsolete]; present {
				t.Fatalf("%s retains obsolete sandbox setting %s", name, obsolete)
			}
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
	if worker.User == "0:0" || worker.User == "root" {
		t.Fatal("worker must retain the non-root image user")
	}
	if worker.Environment[openSandboxDomainEnv] != "http://opensandbox:8090" {
		t.Fatal("worker must call the internal OpenSandbox control plane")
	}
	if worker.Environment[openSandboxUseProxyEnv] != "true" {
		t.Fatal("worker must use the OpenSandbox server proxy")
	}
	opensandbox := config.Services["opensandbox"]
	if !strings.Contains(opensandbox.Image, "opensandbox/server:v0.2.3@sha256:") {
		t.Fatalf("OpenSandbox image is not version-and-digest pinned: %q", opensandbox.Image)
	}
	if len(opensandbox.Ports) != 1 || opensandbox.Ports[0] != "127.0.0.1:8090:8090" {
		t.Fatalf("OpenSandbox host exposure = %v, want loopback only", opensandbox.Ports)
	}
	var socket, configMount, stateVolume bool
	for name, service := range config.Services {
		for _, volume := range service.Volumes {
			if volume.Kind != yaml.MappingNode {
				if strings.Contains(volume.Value, "opensandbox.toml:/etc/opensandbox/config.toml:ro") {
					configMount = name == "opensandbox"
				}
				if strings.Contains(volume.Value, "opensandbox-data:/var/lib/opensandbox") {
					stateVolume = name == "opensandbox"
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
				if name != "opensandbox" || mount.Type != "bind" ||
					mount.Bind.CreateHostPath == nil || *mount.Bind.CreateHostPath {
					t.Fatalf("%s socket mount must be OpenSandbox-only and reject a missing host socket", name)
				}
				socket = true
			}
		}
	}
	if !socket || !configMount || !stateVolume {
		t.Fatalf("incomplete OpenSandbox boundary: socket=%v config=%v state=%v", socket, configMount, stateVolume)
	}
}
