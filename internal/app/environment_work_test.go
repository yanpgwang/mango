package app

import (
	"context"
	"errors"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestEnvironmentWorkHeartbeatBoundsLeaseTTL(t *testing.T) {
	environments := newMemoryEnvironmentRepository()
	if err := environments.Put(context.Background(), domain.Environment{
		ID: "env_self_hosted", ConfigType: "self_hosted",
	}); err != nil {
		t.Fatal(err)
	}
	service := NewEnvironmentWorkService(nil, environments)
	for _, ttl := range []int64{0, MaxEnvironmentWorkTTLSeconds + 1, 1<<63 - 1} {
		_, err := service.Heartbeat(
			context.Background(), "env_self_hosted", "work_one", nil, &ttl,
		)
		var domainErr *domain.DomainError
		if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindValidation {
			t.Errorf("TTL %d error = %T %v, want validation", ttl, err, err)
		}
	}
}
