package domain

import (
	"strings"
	"testing"
)

func TestNormalizeSessionMemoryStoreMountPath(t *testing.T) {
	got, err := NormalizeSessionMemoryStoreMountPath(" Project  Knowledge! ")
	if err != nil || got != "/mnt/memory/project-knowledge" {
		t.Fatalf("normalized mount = %q, %v", got, err)
	}
	for _, name := range []string{"---", string([]byte{0xff}), strings.Repeat("a", 256)} {
		if got, err := NormalizeSessionMemoryStoreMountPath(name); err == nil {
			t.Fatalf("NormalizeSessionMemoryStoreMountPath(%q) = %q, want error", name, got)
		}
	}
}
