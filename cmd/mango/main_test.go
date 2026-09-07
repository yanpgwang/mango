package main

import (
	"net/http"
	"testing"
	"time"
)

// TestDefaultAddr_BindsLoopback asserts the serve default listen address binds
// to loopback so a fresh serve never exposes the unauthenticated API on all
// interfaces.
func TestDefaultAddr_BindsLoopback(t *testing.T) {
	if defaultAddr != "127.0.0.1:8080" {
		t.Fatalf("defaultAddr = %q; want 127.0.0.1:8080", defaultAddr)
	}
}

// TestNewHTTPServer_Timeouts asserts the serving server sets slow-header and
// idle bounds and a header-size cap, and deliberately leaves WriteTimeout unset
// so long-lived SSE streams are not aborted mid-response.
func TestNewHTTPServer_Timeouts(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", http.NewServeMux())
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v; want 10s", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v; want 120s", srv.IdleTimeout)
	}
	if srv.MaxHeaderBytes != 1<<20 {
		t.Errorf("MaxHeaderBytes = %d; want %d", srv.MaxHeaderBytes, 1<<20)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v; want 0 (unset, so SSE streams are not aborted)", srv.WriteTimeout)
	}
}

// TestResolveModelClient_ReportsRealModelWithEnv proves model selection reports
// realModel=true when both the model base URL and API key are configured. It
// performs no network call: construction does not contact the endpoint.
func TestResolveModelClient_ReportsRealModelWithEnv(t *testing.T) {
	t.Setenv("MANGO_MODEL_BASE_URL", "https://model.invalid")
	t.Setenv("MANGO_MODEL_API_KEY", "sk-test")
	client, realModel, err := resolveModelClient()
	if err != nil {
		t.Fatal(err)
	}
	if !realModel {
		t.Fatalf("resolveModelClient realModel=false with model env configured; want true")
	}
	if client == nil {
		t.Fatal("resolveModelClient returned a nil client")
	}
}

func TestResolveModelClient_UsesFakeWithoutEnv(t *testing.T) {
	t.Setenv("MANGO_MODEL_BASE_URL", "")
	t.Setenv("MANGO_MODEL_API_KEY", "")
	client, realModel, err := resolveModelClient()
	if err != nil {
		t.Fatal(err)
	}
	if realModel {
		t.Fatal("resolveModelClient realModel=true without model configuration")
	}
	if client == nil {
		t.Fatal("resolveModelClient returned a nil fake client")
	}
}
