package kubernetes_test

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/stretchr/testify/require"
)

type candidateArtifact struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Platform  string   `json:"platform"`
	Image     string   `json:"image"`
	Digest    string   `json:"digest"`
	Platforms []string `json:"platforms"`
	SHA256    string   `json:"sha256"`
	Size      int64    `json:"size_bytes"`
}

type releaseCandidate struct {
	SchemaVersion int                 `json:"schema_version"`
	Version       string              `json:"version"`
	Revision      string              `json:"revision"`
	SDKVersions   map[string]string   `json:"sdk_versions"`
	Artifacts     []candidateArtifact `json:"artifacts"`
}

func readCandidate(folder, revision string) (releaseCandidate, error) {
	var result releaseCandidate
	data, err := os.ReadFile(filepath.Join(folder, "manifest.json"))
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, err
	}
	if result.SchemaVersion != 1 || result.Version != "0.1.0-alpha.2" || result.Revision != revision ||
		len(result.SDKVersions) != 3 || result.SDKVersions["go"] != result.Version || result.SDKVersions["typescript"] != result.Version || result.SDKVersions["python"] != "0.1.0a2" {
		return result, errors.New("candidate identity must match this reviewed alpha source and SDKs")
	}
	sha := regexp.MustCompile(`^[0-9a-f]{64}$`)
	names, roles := map[string]bool{}, map[string]bool{}
	for _, item := range result.Artifacts {
		if item.Name == "" || item.Name == "." || item.Name == ".." || strings.ContainsAny(item.Name, `/\`) || names[item.Name] || !sha.MatchString(item.SHA256) || item.Size <= 0 {
			return result, errors.New("invalid or duplicate candidate artifact")
		}
		names[item.Name] = true
		path := filepath.Join(folder, item.Name)
		info, err := os.Lstat(path)
		if err != nil {
			return result, err
		}
		if !info.Mode().IsRegular() || info.Size() != item.Size {
			return result, errors.New("candidate artifact must be an ordinary file with its declared size")
		}
		file, err := os.Open(path)
		if err != nil {
			return result, err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		if err := errors.Join(copyErr, file.Close()); err != nil {
			return result, err
		}
		if hex.EncodeToString(hash.Sum(nil)) != item.SHA256 {
			return result, errors.New("candidate artifact bytes differ from the reviewed manifest")
		}
		role := item.Kind
		switch item.Kind {
		case "commands":
			if !slices.Contains([]string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"}, item.Platform) {
				return result, errors.New("unsupported command archive platform")
			}
			role += ":" + item.Platform
		case "python-sdk":
			if strings.HasSuffix(item.Name, ".whl") {
				role += ":wheel"
			} else if strings.HasSuffix(item.Name, ".tar.gz") {
				role += ":sdist"
			} else {
				return result, errors.New("unexpected Python package type")
			}
		case "oci-image":
			if !slices.Contains([]string{"mango", "mango-self-hosted-worker"}, item.Image) || !strings.HasPrefix(item.Digest, "sha256:") || !sha.MatchString(strings.TrimPrefix(item.Digest, "sha256:")) ||
				!slices.Equal(item.Platforms, []string{"linux/amd64", "linux/arm64"}) {
				return result, errors.New("invalid release OCI identity or platforms")
			}
			role += ":" + item.Image
		case "helm-chart", "go-sdk", "typescript-sdk":
		default:
			return result, errors.New("unexpected candidate artifact kind")
		}
		if roles[role] {
			return result, errors.New("duplicate candidate artifact role")
		}
		roles[role] = true
	}
	for _, role := range []string{"commands:" + runtime.GOOS + "/" + runtime.GOARCH, "helm-chart", "go-sdk", "typescript-sdk", "python-sdk:wheel", "python-sdk:sdist", "oci-image:mango", "oci-image:mango-self-hosted-worker"} {
		if !roles[role] {
			return result, fmt.Errorf("candidate lacks required artifact role %s", role)
		}
	}
	return result, nil
}

func extractCandidateWorker(archive, target string) (err error) {
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, compressed.Close()) }()
	created := false
	defer func() {
		if err != nil && created {
			err = errors.Join(err, os.Remove(target))
		}
	}()
	prefix := strings.TrimSuffix(filepath.Base(archive), ".tar.gz") + "/"
	allowed := map[string]bool{prefix + "mango": true, prefix + "mango-worker": true, prefix + "LICENSE": true}
	seen := map[string]bool{}
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if !allowed[header.Name] || seen[header.Name] || !header.FileInfo().Mode().IsRegular() {
			return errors.New("unexpected command archive member")
		}
		seen[header.Name] = true
		if header.Name == prefix+"mango-worker" {
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
			if err != nil {
				return err
			}
			created = true
			_, copyErr := io.Copy(output, reader)
			if err := errors.Join(copyErr, output.Close()); err != nil {
				return err
			}
		}
	}
	if len(seen) != 3 {
		return errors.New("command archive lacks the paired commands and license")
	}
	_, err = io.Copy(io.Discard, compressed) // Verify the complete gzip checksum.
	return err
}

func (c *clusterFixture) installCandidateInputs(folder string, candidate releaseCandidate) {
	c.t.Helper()
	store := c.command(nil, nil, "docker", "info", "--format", "{{json .DriverStatus}}")
	require.Contains(c.t, string(store), "io.containerd.snapshotter", "OCI acceptance needs a containerd Docker image store")
	for _, item := range candidate.Artifacts {
		path := filepath.Join(folder, item.Name)
		switch item.Kind {
		case "helm-chart":
			c.chart = path
		case "commands":
			if item.Platform == runtime.GOOS+"/"+runtime.GOARCH {
				require.NoError(c.t, extractCandidateWorker(path, c.workerBinary))
				var identity struct{ Version, Revision string }
				require.NoError(c.t, json.Unmarshal(c.command(nil, nil, c.workerBinary, "version"), &identity))
				require.Equal(c.t, candidate.Version, identity.Version)
				require.Equal(c.t, candidate.Revision, identity.Revision)
			}
		case "oci-image":
			c.command(nil, nil, "docker", "image", "load", "--input", path)
			alias := c.image
			if item.Image == "mango-self-hosted-worker" {
				alias = c.workerImage
			}
			c.command(nil, nil, "docker", "tag", item.Digest, alias)
			c.builtImages = append(c.builtImages, alias)
			metadata := c.command(nil, nil, "docker", "image", "inspect", alias, "--format", `{{.Os}}/{{.Architecture}} {{.Config.User}} {{index .Config.Labels "org.opencontainers.image.version"}} {{index .Config.Labels "org.opencontainers.image.revision"}}`)
			require.Equal(c.t, "linux/"+runtime.GOARCH+" 65532:65532 "+candidate.Version+" "+candidate.Revision, strings.TrimSpace(string(metadata)))
			c.t.Log("Imported release image:", item.Image, item.Digest)
		}
	}
}
