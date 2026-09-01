package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"path"
	"strings"
	"testing"
)

func TestGitRepositoryStagingTreeIsTargetSibling(t *testing.T) {
	const target = "/workspace/projects/repository"
	archivePath, stagingPath := gitRepositoryStagingPaths(target, "sesrsc_repository")
	if path.Dir(stagingPath) != path.Dir(target) {
		t.Fatalf("staging tree %q is not a target sibling", stagingPath)
	}
	if !strings.HasPrefix(archivePath, gitRepositoryStagingRoot+"/") {
		t.Fatalf("archive path %q escaped the private staging root", archivePath)
	}
	if strings.HasPrefix(stagingPath, gitRepositoryStagingRoot+"/") {
		t.Fatalf("staging tree %q remained under the private archive root", stagingPath)
	}
}

func TestValidateGitRepositoryArchiveRejectsUnsafeEntries(t *testing.T) {
	tests := map[string][]*tar.Header{
		"escaping symlink": {
			{Name: ".git/HEAD", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5},
			{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "../../etc", Mode: 0o777},
		},
		"path traversal": {
			{Name: ".git/HEAD", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5},
			{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644},
		},
		"missing head": {
			{Name: "README.md", Typeflag: tar.TypeReg, Mode: 0o644},
		},
	}
	for name, headers := range tests {
		t.Run(name, func(t *testing.T) {
			archive := gitRepositoryArchiveForTest(t, headers)
			err := validateGitRepositoryArchive(context.Background(), bytes.NewReader(archive))
			if err == nil || !IsPermanent(err) {
				t.Fatalf("validation error = %v, want permanent", err)
			}
		})
	}
}

func gitRepositoryArchiveForTest(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	for _, header := range headers {
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := w.Write(bytes.Repeat([]byte("x"), int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}
