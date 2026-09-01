package sandbox

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

func TestExtractCanonicalSkillRejectsUnsafeArchiveEntries(t *testing.T) {
	tests := map[string][]skillArchiveEntry{
		"traversal": {
			{name: "Portable_Tool/SKILL.md", body: "instructions"},
			{name: "Portable_Tool/../escape", body: "escape"},
		},
		"symlink": {
			{name: "Portable_Tool/SKILL.md", body: "instructions"},
			{name: "Portable_Tool/link", body: "../../etc", mode: os.ModeSymlink | 0o777},
		},
		"duplicate": {
			{name: "Portable_Tool/SKILL.md", body: "instructions"},
			{name: "Portable_Tool/SKILL.md", body: "again"},
		},
	}
	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			archive, mount := skillArchiveFixture(t, entries)
			file := writeSkillArchiveFixture(t, archive)
			err := extractCanonicalSkill(
				context.Background(), file, int64(len(archive)), t.TempDir(), mount,
			)
			if err == nil {
				t.Fatal("unsafe Skill archive succeeded")
			}
		})
	}
}

type skillArchiveEntry struct {
	name string
	body string
	mode os.FileMode
}

func skillArchiveFixture(
	t *testing.T,
	entries []skillArchiveEntry,
) ([]byte, ReadOnlySkillMount) {
	t.Helper()
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	var expanded int64
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)
		part, err := w.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
		expanded += int64(len(entry.body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data := archive.Bytes()
	sum := sha256.Sum256(data)
	return data, ReadOnlySkillMount{
		Identity: "skill_portable@100", Name: "portable-tool",
		RuntimePath: SessionSkillsRoot + "/portable-tool", ArchiveRoot: "Portable_Tool",
		SizeBytes: int64(len(data)), UncompressedSizeBytes: expanded,
		ChecksumSHA256: hex.EncodeToString(sum[:]),
	}
}

func writeSkillArchiveFixture(t *testing.T, data []byte) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "skill-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	return file
}
