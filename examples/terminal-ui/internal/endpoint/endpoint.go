// Package endpoint discovers, probes, and remembers Mango control-plane URLs.
package endpoint

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultURL = "http://127.0.0.1:8080"
	DemoURL    = "demo://local"
)

type Candidate struct {
	URL       string
	Label     string
	Source    string
	Available bool
	SkipProbe bool
}

type savedConfig struct {
	Endpoint string `json:"endpoint"`
}

var userConfigDir = os.UserConfigDir

// Discover assembles known endpoints without scanning the network. Reachability
// is checked separately so startup rendering is immediate.
func Discover(selected, environment, saved string, includeDemo bool) []Candidate {
	candidates := make([]Candidate, 0, 5)
	seen := map[string]bool{}
	add := func(candidate Candidate) {
		candidate.URL = strings.TrimSpace(candidate.URL)
		if candidate.URL == "" {
			return
		}
		if candidate.URL != DemoURL {
			normalized, err := Normalize(candidate.URL)
			if err != nil {
				return
			}
			candidate.URL = normalized
		}
		if seen[candidate.URL] {
			return
		}
		seen[candidate.URL] = true
		if candidate.Label == "" {
			candidate.Label = candidate.URL
		}
		candidates = append(candidates, candidate)
	}
	if includeDemo {
		add(Candidate{URL: DemoURL, Label: "Local demo", Source: "built in · no server required", Available: true, SkipProbe: true})
	}
	add(Candidate{URL: selected, Source: "selected"})
	add(Candidate{URL: environment, Source: "MANGO_URL"})
	add(Candidate{URL: saved, Source: "saved"})
	add(Candidate{URL: DefaultURL, Source: "local default"})
	add(Candidate{URL: "http://localhost:8080", Source: "local candidate"})
	return candidates
}

func Load() (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var config savedConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return "", err
	}
	if strings.TrimSpace(config.Endpoint) == "" {
		return "", nil
	}
	return Normalize(config.Endpoint)
}

func Save(value string) error {
	normalized, err := Normalize(value)
	if err != nil {
		return err
	}
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(savedConfig{Endpoint: normalized}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".connection-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

// Normalize accepts a convenient host:port form but rejects URL components
// that would either leak credentials or corrupt the client's path joining.
func Normalize(value string) (string, error) {
	target := strings.TrimSpace(value)
	if target == "" {
		return "", errors.New("enter an endpoint")
	}
	if !strings.Contains(target, "://") {
		target = "http://" + target
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("use an http:// or https:// endpoint")
	}
	if parsed.User != nil {
		return "", errors.New("credentials do not belong in the endpoint URL")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("query parameters and fragments do not belong in the endpoint URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return strings.TrimRight(parsed.String(), "/"), nil
}

// Probe reports basic transport reachability. The actual API request remains
// the source of truth because a listening server may still reject credentials.
func Probe(parent context.Context, value string) bool {
	if value == DemoURL {
		return true
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	host := parsed.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		port := "80"
		if parsed.Scheme == "https" {
			port = "443"
		}
		host = net.JoinHostPort(parsed.Hostname(), port)
	}
	ctx, cancel := context.WithTimeout(parent, 700*time.Millisecond)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", host)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func configPath() (string, error) {
	directory, err := userConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "mango", "connection.json"), nil
}
