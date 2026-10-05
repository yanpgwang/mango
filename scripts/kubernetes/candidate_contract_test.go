package kubernetes_test

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCandidateRejectsUnmatchedOrChangedInputs(t *testing.T) {
	for _, scenario := range []string{"valid", "old-source", "changed-sdk", "escape", "duplicate-role", "missing-chart", "invalid-image-digest"} {
		t.Run(scenario, func(t *testing.T) {
			folder := t.TempDir()
			var records []map[string]any
			for _, item := range []struct{ name, kind, platform, image string }{
				{"commands.tar.gz", "commands", runtime.GOOS + "/" + runtime.GOARCH, ""},
				{"go.zip", "go-sdk", "", ""}, {"python.whl", "python-sdk", "", ""}, {"python.tar.gz", "python-sdk", "", ""},
				{"typescript.tgz", "typescript-sdk", "", ""}, {"chart.tgz", "helm-chart", "", ""},
				{"runtime.tar", "oci-image", "", "mango"}, {"worker.tar", "oci-image", "", "mango-self-hosted-worker"},
			} {
				body := []byte(item.name)
				require.NoError(t, os.WriteFile(filepath.Join(folder, item.name), body, 0600))
				digest := sha256.Sum256(body)
				records = append(records, map[string]any{"name": item.name, "kind": item.kind, "platform": item.platform, "image": item.image,
					"sha256": fmt.Sprintf("%x", digest), "size_bytes": len(body), "digest": "sha256:" + fmt.Sprintf("%x", digest), "platforms": []string{"linux/amd64", "linux/arm64"}})
			}
			revision := "0123456789abcdef0123456789abcdef01234567"
			manifestRevision := revision
			switch scenario {
			case "old-source":
				manifestRevision = "abcdef0123456789abcdef0123456789abcdef01"
			case "changed-sdk":
				// Keep the length identical so this exercises the checksum,
				// rather than passing because a size check happens to reject it.
				require.NoError(t, os.WriteFile(filepath.Join(folder, "go.zip"), []byte("BADZIP"), 0600))
			case "escape":
				records[0]["name"] = "../outside.tar.gz"
			case "duplicate-role":
				records[7]["image"] = "mango"
			case "missing-chart":
				records = append(records[:5], records[6:]...)
			case "invalid-image-digest":
				records[6]["digest"] = "sha256:invalid"
			}
			metadata := map[string]any{"schema_version": 1, "version": "0.1.0-alpha.2", "revision": manifestRevision,
				"sdk_versions": map[string]string{"go": "0.1.0-alpha.2", "python": "0.1.0a2", "typescript": "0.1.0-alpha.2"}, "artifacts": records}
			data, err := json.Marshal(metadata)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(folder, "manifest.json"), data, 0600))
			_, err = readCandidate(folder, revision)
			if scenario == "valid" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestCandidateWorkerArchiveRejectsEscapesAndPreservesExistingOutput(t *testing.T) {
	for _, scenario := range []string{"valid", "traversal", "symlink", "existing"} {
		t.Run(scenario, func(t *testing.T) {
			folder := t.TempDir()
			archive, target := filepath.Join(folder, "commands.tar.gz"), filepath.Join(folder, "mango-worker")
			file, err := os.Create(archive)
			require.NoError(t, err)
			compressed := gzip.NewWriter(file)
			writer := tar.NewWriter(compressed)
			for _, name := range []string{"mango", "mango-worker", "LICENSE"} {
				header := &tar.Header{Name: "commands/" + name, Mode: 0755, Size: 5, Typeflag: tar.TypeReg}
				if name == "mango-worker" && scenario == "traversal" {
					header.Name = "commands/../../escaped"
				}
				if name == "mango-worker" && scenario == "symlink" {
					header.Typeflag, header.Linkname, header.Size = tar.TypeSymlink, "../../escaped", 0
				}
				require.NoError(t, writer.WriteHeader(header))
				if header.Size != 0 {
					_, err = writer.Write([]byte("bytes"))
					require.NoError(t, err)
				}
			}
			require.NoError(t, writer.Close())
			require.NoError(t, compressed.Close())
			require.NoError(t, file.Close())
			if scenario == "existing" {
				require.NoError(t, os.WriteFile(target, []byte("keep me"), 0600))
			}
			err = extractCandidateWorker(archive, target)
			if scenario == "valid" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if scenario == "existing" {
				data, readErr := os.ReadFile(target)
				require.NoError(t, readErr)
				require.Equal(t, "keep me", string(data))
			}
		})
	}
}
