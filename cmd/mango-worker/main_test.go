package main

import (
	"context"
	"strings"
	"testing"
)

func TestDockerSupervisorRequiresExplicitEnvironmentCredential(t *testing.T) {
	t.Setenv("MANGO_ENVIRONMENT_KEY", "")
	t.Setenv("MANGO_API_KEY", "workspace-administration-key")
	err := run(context.Background(), []string{"docker"})
	if err == nil || !strings.Contains(err.Error(), "MANGO_ENVIRONMENT_KEY is required") {
		t.Fatalf("missing Environment credential error=%v", err)
	}
}
