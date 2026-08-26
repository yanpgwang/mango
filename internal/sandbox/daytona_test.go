package sandbox

import "testing"

func TestDaytonaAutoPauseInterval(t *testing.T) {
	if interval := daytonaAutoPauseInterval(0); interval != nil {
		t.Fatalf("zero auto-pause interval = %d, want nil", *interval)
	}
	if interval := daytonaAutoPauseInterval(-1); interval != nil {
		t.Fatalf("negative auto-pause interval = %d, want nil", *interval)
	}
	interval := daytonaAutoPauseInterval(15)
	if interval == nil || *interval != 15 {
		t.Fatalf("positive auto-pause interval = %v, want 15", interval)
	}
}
