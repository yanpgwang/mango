package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRepositoryContractIsCurrent(t *testing.T) {
	if err := run(filepath.Join("..", ".."), true); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryOperationsAreComplete(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "sdk", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	ops, err := operations(document)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]operation, len(ops))
	for _, op := range ops {
		seen[op.ID] = op
		if op.ID != "health" && op.ID != "readiness" && op.ID != "openAPI" && op.Public {
			t.Fatalf("protected operation %s became public", op.ID)
		}
	}
	if len(ops) != 99 {
		t.Fatalf("operations = %d, expected 99; review SDK coverage when adding routes", len(ops))
	}
	for _, id := range []string{"health", "readiness", "openAPI"} {
		if !seen[id].Public {
			t.Errorf("%s should be public", id)
		}
	}
	if op := seen["stopEnvironmentWork"]; op.ResponseStatus != 204 || op.ResponseContentType != "" {
		t.Errorf("empty response metadata = %+v", op)
	}
	if op := seen["failEnvironmentWork"]; op.ResponseStatus != 204 || op.ResponseContentType != "" {
		t.Errorf("failure response metadata = %+v", op)
	}
	if op := seen["streamSessionEvents"]; op.ResponseContentType != "text/event-stream" {
		t.Errorf("stream media type = %q", op.ResponseContentType)
	}
	if op := seen["uploadFile"]; op.RequestContentType != "multipart/form-data" {
		t.Errorf("upload media type = %q", op.RequestContentType)
	}
	if op := seen["getMemory"]; len(op.Parameters) < 2 {
		t.Errorf("nested resource path parameters lost: %+v", op)
	}
}

func TestExporterRejectsAmbiguousSuccessMedia(t *testing.T) {
	_, _, err := content(map[string]any{"application/json": map[string]any{}, "text/plain": map[string]any{}})
	if err == nil {
		t.Fatal("multiple media types silently selected")
	}
}

func TestResolveRejectsUnknownAndCyclicRefs(t *testing.T) {
	for _, ref := range []string{"#/missing", "https://example.invalid/spec", "#/cycle"} {
		doc := map[string]any{"cycle": map[string]any{"$ref": "#/cycle"}}
		if _, err := resolve(doc, map[string]any{"$ref": ref}); err == nil {
			t.Errorf("accepted %q", ref)
		}
	}
}
