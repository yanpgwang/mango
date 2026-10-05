package buildinfo

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestWriteReportsBuildIdentity(t *testing.T) {
	var output bytes.Buffer
	if err := Write(&output); err != nil {
		t.Fatal(err)
	}
	var info map[string]string
	if err := json.Unmarshal(output.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if len(info) != 2 || info["version"] != "dev" || info["revision"] != "unknown" {
		t.Fatalf("development identity = %#v", info)
	}
}

type failedWriter struct{ err error }

func (w failedWriter) Write([]byte) (int, error) { return 0, w.err }

func TestWritePropagatesOutputFailure(t *testing.T) {
	want := errors.New("output unavailable")
	if err := Write(failedWriter{want}); !errors.Is(err, want) {
		t.Fatalf("write error = %v", err)
	}
}
