package dockertest

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestFixtureExecAndPermissions(t *testing.T) {
	Require(t)
	fixture := NewFixture(t, "")
	root := fixture.Root
	directory := filepath.Join(root, "restricted")
	if err := fixture.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, "data")
	if err := fixture.WriteFile(file, []byte("fixture"), 0o400); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, args := range [][]string{{"chmod", "0555", file}, {"chmod", "0000", directory}, {"chmod", "0700", directory}, {"cat", file}} {
		stdout, stderr, code, err := fixture.Exec(ctx, root, args, nil)
		if err != nil || code != 0 {
			t.Fatalf("%v: exit %d, error %v, stdout %q stderr %q", args, code, err, stdout, stderr)
		}
	}
}
