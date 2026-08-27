package endpoint

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverOrdersAndDeduplicatesCandidates(t *testing.T) {
	candidates := Discover("https://saved.example.com/", "https://env.example.com", "https://saved.example.com", true)
	if len(candidates) != 5 {
		t.Fatalf("candidates=%#v", candidates)
	}
	if candidates[0].URL != DemoURL || !candidates[0].Available ||
		candidates[1].URL != "https://saved.example.com" ||
		candidates[2].URL != "https://env.example.com" ||
		candidates[3].URL != DefaultURL {
		t.Fatalf("candidate order=%#v", candidates)
	}
}

func TestSaveAndLoadEndpoint(t *testing.T) {
	configRoot := t.TempDir()
	previousConfigDir := userConfigDir
	userConfigDir = func() (string, error) { return configRoot, nil }
	t.Cleanup(func() { userConfigDir = previousConfigDir })
	const target = "https://mango.example.com"
	if err := Save(target); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil || loaded != target {
		t.Fatalf("loaded=%q err=%v", loaded, err)
	}
	path, err := configPath()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode=%o", info.Mode().Perm())
	}
}

func TestSaveRejectsCredentialsInEndpointURL(t *testing.T) {
	if err := Save("https://secret@example.com"); err == nil {
		t.Fatal("saved endpoint URL with embedded credentials")
	}
	if err := Save("https://example.com?api_key=secret"); err == nil {
		t.Fatal("saved endpoint URL with query parameters")
	}
}

func TestLoadRejectsMalformedSavedEndpoint(t *testing.T) {
	configRoot := t.TempDir()
	previousConfigDir := userConfigDir
	userConfigDir = func() (string, error) { return configRoot, nil }
	t.Cleanup(func() { userConfigDir = previousConfigDir })
	path := filepath.Join(configRoot, "mango", "connection.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"endpoint\":\"https://secret@example.com\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if loaded, err := Load(); err == nil || loaded != "" {
		t.Fatalf("loaded malformed endpoint=%q err=%v", loaded, err)
	}
}

func TestProbeFindsListeningEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if !Probe(context.Background(), "http://"+listener.Addr().String()) {
		t.Fatal("listening endpoint was not detected")
	}
	if Probe(context.Background(), "not a URL") {
		t.Fatal("invalid endpoint was detected")
	}
}
