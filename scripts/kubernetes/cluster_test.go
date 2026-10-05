package kubernetes_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
)

const nodeImage = "kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"

type clusterFixture struct {
	t                         *testing.T
	ctx                       context.Context
	root, temp, name, node    string
	kind, kubectl, helm       string
	image, workerImage, model string
	workerBinary, revision    string
	chart                     string
	kubeconfig, keyring       string
	ports                     [2]int
	builtImages               []string
}

type stateFixture struct {
	cluster              *clusterFixture
	project, namespace   string
	baseURL, internalURL string
	addresses            map[string]string
	s3                   *s3.Client
}

// Capture stdout privately; failures report only stderr, never a generated key,
// SQL dump or submitted Secret document. All command arguments are literal
// fixture paths/IDs rather than credentials.
func (c *clusterFixture) command(input []byte, environment []string, binary string, args ...string) []byte {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = c.root
	cmd.Env = append(os.Environ(), environment...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		text := stderr.String()
		if len(text) > 4000 {
			text = text[len(text)-4000:]
		}
		c.t.Fatalf("%s failed: %v\n%s", filepath.Base(binary), err, text)
	}
	return stdout.Bytes()
}

func (c *clusterFixture) cleanup(binary string, args ...string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = c.root
	// The model image is required for Compose to parse even during teardown.
	cmd.Env = append(os.Environ(), "MANGO_KUBERNETES_MODEL_IMAGE="+c.model)
	if err := cmd.Run(); err != nil {
		c.t.Errorf("owned fixture cleanup %s: %v", filepath.Base(binary), err)
	}
}

func (c *clusterFixture) removeWorkspace(name string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "volume", "inspect", "--format", "{{.Name}}", name)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if !strings.Contains(strings.ToLower(string(output)), "no such volume") {
			c.t.Errorf("inspect owned workspace volume: %v", err)
		}
		return
	}
	c.cleanup("docker", "volume", "rm", name)
}

func executable(name, variable string) string {
	if value := os.Getenv(variable); value != "" {
		return value
	}
	return name
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
}

// Keep HTTP assertions in the test goroutine so a failed contract immediately
// runs cleanup, rather than exiting an Eventually callback goroutine.
func (c *clusterFixture) await(timeout time.Duration, condition func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("fixture condition did not complete within %s", timeout)
		}
		select {
		case <-c.ctx.Done():
			c.t.Fatalf("fixture context ended: %v", c.ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func newCluster(t *testing.T) *clusterFixture {
	t.Helper()
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	var random [6]byte
	_, err = rand.Read(random[:])
	require.NoError(t, err)
	name := "mango-alpha-" + hex.EncodeToString(random[:])
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	t.Cleanup(cancel)
	c := &clusterFixture{t: t, ctx: ctx, root: root, temp: t.TempDir(), name: name,
		kind: executable("kind", "MANGO_KIND_PATH"), kubectl: executable("kubectl", "MANGO_KUBECTL_PATH"),
		helm: executable("helm", "MANGO_HELM_PATH"), ports: [2]int{freePort(t), freePort(t)}}
	c.node, c.kubeconfig = name+"-control-plane", filepath.Join(c.temp, "kubeconfig")
	for c.ports[0] == c.ports[1] {
		c.ports[1] = freePort(t)
	}
	c.image, c.workerImage, c.model = "mango:"+name, "mango-self-hosted-worker:"+name, "mango-alpha-model:"+name
	c.workerBinary = filepath.Join(c.temp, "mango-worker")
	c.chart = "charts/mango"
	c.revision = strings.TrimSpace(string(c.command(nil, nil, "git", "rev-parse", "HEAD")))
	keyBytes := make([]byte, 32)
	_, err = rand.Read(keyBytes)
	require.NoError(t, err)
	c.keyring = fmt.Sprintf("{\"active_key_id\":\"alpha\",\"keys\":{\"alpha\":%q}}", base64.StdEncoding.EncodeToString(keyBytes))
	for _, tool := range []string{c.kind, c.kubectl, c.helm, "docker"} {
		_, err := exec.LookPath(tool)
		require.NoError(t, err, "required Kubernetes test tool missing")
	}
	c.command(nil, nil, "docker", "info", "--format", "{{.ServerVersion}}")
	// Register before any import/build, and remove unique aliases only once.
	t.Cleanup(func() {
		if len(c.builtImages) != 0 {
			c.cleanup("docker", append([]string{"image", "rm"}, c.builtImages...)...)
		}
	})
	inputs := []struct{ file, image string }{{"scripts/kubernetes/model/Dockerfile", c.model}}
	if candidateFolder := os.Getenv("MANGO_RELEASE_CANDIDATE"); candidateFolder != "" {
		if !filepath.IsAbs(candidateFolder) {
			candidateFolder = filepath.Join(c.root, candidateFolder)
		}
		candidate, err := readCandidate(candidateFolder, c.revision)
		require.NoError(t, err)
		t.Log("Using actual release archives, packaged chart and OCI images:", candidate.Revision)
		c.installCandidateInputs(candidateFolder, candidate)
	} else {
		inputs = append([]struct{ file, image string }{{"Dockerfile", c.image}, {"deployments/self-hosted/docker/Dockerfile", c.workerImage}}, inputs...)
		// Same-host source-test execution retains the compiling toolchain even
		// when sudo's secure_path selects an older Go. Never distributed.
		//nolint:staticcheck // SA1019: intentional same-host test toolchain selection.
		c.command(nil, nil, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", c.workerBinary, "./cmd/mango-worker")
	}
	t.Log("Building explicit source fixture inputs")
	builder := []string{"buildx", "build", "--load"}
	if name := os.Getenv("MANGO_KUBERNETES_BUILDER"); name != "" {
		builder = append(builder, "--builder", name)
	}
	for _, item := range inputs {
		args := append(append([]string{}, builder...), "-f", item.file, "-t", item.image,
			"--build-arg", "VERSION=0.1.0-alpha.2", "--build-arg", "REVISION="+c.revision, ".")
		c.command(nil, nil, "docker", args...)
		c.builtImages = append(c.builtImages, item.image)
	}
	config := fmt.Sprintf("kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n- role: control-plane\n  extraPortMappings:\n  - containerPort: 30080\n    hostPort: %d\n    listenAddress: 127.0.0.1\n  - containerPort: 30081\n    hostPort: %d\n    listenAddress: 127.0.0.1\n", c.ports[0], c.ports[1])
	configPath := filepath.Join(c.temp, "kind.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0600))
	t.Cleanup(func() { c.cleanup(c.kind, "delete", "cluster", "--name", c.name, "--kubeconfig", c.kubeconfig) })
	t.Log("Creating isolated Kubernetes 1.37.0 cluster with explicit kubeconfig")
	c.command(nil, nil, c.kind, "create", "cluster", "--name", c.name, "--kubeconfig", c.kubeconfig, "--image", nodeImage, "--config", configPath, "--wait", "3m")
	c.command(nil, nil, c.kind, "load", "docker-image", c.image, "--name", c.name)
	return c
}

func (c *clusterFixture) kube(input []byte, args ...string) []byte {
	c.t.Helper()
	return c.command(input, nil, c.kubectl, append([]string{"--kubeconfig", c.kubeconfig}, args...)...)
}

func (c *clusterFixture) apply(objects ...map[string]any) {
	c.t.Helper()
	data, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": objects})
	require.NoError(c.t, err)
	c.kube(data, "apply", "-f", "-")
}

func (s *stateFixture) compose(input []byte, args ...string) []byte {
	s.cluster.t.Helper()
	base := []string{"compose", "--project-name", s.project, "-f", "scripts/kubernetes/fixtures/compose.yaml"}
	return s.cluster.command(input, []string{"MANGO_KUBERNETES_MODEL_IMAGE=" + s.cluster.model}, "docker", append(base, args...)...)
}

func (c *clusterFixture) startState(suffix string) *stateFixture {
	c.t.Helper()
	s := &stateFixture{cluster: c, project: c.name + "-" + suffix, addresses: map[string]string{}}
	c.t.Cleanup(func() {
		c.cleanup("docker", "compose", "--project-name", s.project, "-f", "scripts/kubernetes/fixtures/compose.yaml", "down", "--volumes", "--remove-orphans")
	})
	s.compose(nil, "up", "-d", "--wait", "--wait-timeout", "240", "postgres", "nats", "seaweedfs", "model")
	network := s.project + "_default"
	c.command(nil, nil, "docker", "network", "connect", network, c.node)
	c.t.Cleanup(func() { c.cleanup("docker", "network", "disconnect", network, c.node) })
	for _, service := range []string{"postgres", "nats", "seaweedfs", "model"} {
		s.addresses[service] = s.containerIP(service, network)
	}
	address := strings.TrimSpace(string(s.compose(nil, "port", "seaweedfs", "8333")))
	s.s3 = s3.New(s3.Options{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("fixture-s3-only", "fixture-s3-secret-only", ""),
		BaseEndpoint: aws.String("http://" + address), UsePathStyle: true, HTTPClient: &http.Client{Timeout: 15 * time.Second}})
	_, err := s.s3.CreateBucket(c.ctx, &s3.CreateBucketInput{Bucket: aws.String("mango-alpha")})
	require.NoError(c.t, err)
	return s
}

func (s *stateFixture) containerIP(service, network string) string {
	s.cluster.t.Helper()
	id := strings.TrimSpace(string(s.compose(nil, "ps", "-q", service)))
	require.NotEmpty(s.cluster.t, id)
	raw := s.cluster.command(nil, nil, "docker", "inspect", "--format", "{{json .NetworkSettings.Networks}}", id)
	var networks map[string]struct{ IPAddress string }
	require.NoError(s.cluster.t, json.Unmarshal(raw, &networks))
	require.NotEmpty(s.cluster.t, networks[network].IPAddress)
	return networks[network].IPAddress
}

func (c *clusterFixture) install(s *stateFixture, namespace string, restore bool) {
	c.t.Helper()
	s.namespace = namespace
	s.compose(nil, "up", "-d", "--wait", "--wait-timeout", "240", "temporal")
	s.addresses["temporal"] = s.containerIP("temporal", s.project+"_default")
	c.apply(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace}})
	for name, data := range map[string]map[string]string{
		"fixture-db":    {"database-url": "postgres://postgres:fixture-postgres-only@" + s.addresses["postgres"] + ":5432/mango?sslmode=disable"},
		"fixture-auth":  {"api-key": "fixture-workspace-only"},
		"fixture-nats":  {"nats-url": "nats://" + s.addresses["nats"] + ":4222"},
		"fixture-s3":    {"access-key": "fixture-s3-only", "secret-key": "fixture-s3-secret-only"},
		"fixture-model": {"api-key": "fixture-model-only"}, "fixture-keyring": {"keyring.json": c.keyring},
	} {
		c.apply(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": name, "namespace": namespace}, "stringData": data})
	}
	values := fmt.Sprintf("image:\n  repository: mango\n  tag: %s\n  pullPolicy: Never\ndatabase:\n  existingSecret: {name: fixture-db, key: database-url}\nauth:\n  existingSecret: {name: fixture-auth, key: api-key}\nnats:\n  existingSecret: {name: fixture-nats, key: nats-url}\ntemporal:\n  address: %s:7233\nfiles:\n  endpoint: http://%s:8333\n  bucket: mango-alpha\n  existingSecret: {name: fixture-s3}\nmodel:\n  baseURL: http://%s:8081\n  id: alpha-fixture\n  existingSecret: {name: fixture-model, key: api-key}\nvault:\n  enabled: true\n  existingSecret: {name: fixture-keyring}\nmigration:\n  enabled: %t\n", c.name, s.addresses["temporal"], s.addresses["seaweedfs"], s.addresses["model"], !restore)
	valuesPath := filepath.Join(c.temp, namespace+"-values.yaml")
	require.NoError(c.t, os.WriteFile(valuesPath, []byte(values), 0600))
	c.t.Log("Installing real chart with existing Secrets and external state:", namespace)
	c.command(nil, nil, c.helm, "install", namespace, c.chart, "--namespace", namespace, "--kubeconfig", c.kubeconfig, "--values", valuesPath, "--wait", "--timeout", "5m")
	index := 0
	if restore {
		index = 1
	}
	nodePort := 30080 + index
	c.apply(map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "fixture-exposure", "namespace": namespace},
		"spec": map[string]any{"type": "NodePort", "selector": map[string]string{"app.kubernetes.io/name": "mango", "app.kubernetes.io/instance": namespace, "app.kubernetes.io/component": "api"},
			"ports": []any{map[string]any{"port": 8080, "targetPort": "http", "nodePort": nodePort}}}})
	s.baseURL = "http://127.0.0.1:" + strconv.Itoa(c.ports[index])
	s.internalURL = "http://" + c.node + ":" + strconv.Itoa(nodePort)
	c.t.Cleanup(func() {
		if !c.t.Failed() {
			return
		}
		for _, probe := range []struct {
			binary string
			args   []string
		}{
			{c.kubectl, []string{"--kubeconfig", c.kubeconfig, "-n", namespace, "get", "pods,services,endpointslices", "-o", "wide"}},
			{c.kubectl, []string{"--kubeconfig", c.kubeconfig, "-n", namespace, "logs", "deployment/" + namespace + "-orchestrator", "--tail=30"}},
			{"docker", []string{"exec", c.node, "ip", "-4", "route"}},
			{"docker", []string{"exec", c.node, "curl", "--max-time", "3", "-v", "http://127.0.0.1:" + strconv.Itoa(nodePort) + "/healthz"}},
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			cmd := exec.CommandContext(ctx, probe.binary, probe.args...)
			output, _ := cmd.CombinedOutput()
			cancel()
			c.t.Logf("isolated network diagnostic:\n%s", output)
		}
	})
	// NodePort and EndpointSlice reconciliation happen after API acceptance.
	// Helm waited for the chart's Pods, not this separate test-only exposure.
	c.await(30*time.Second, func() bool {
		request, err := http.NewRequestWithContext(c.ctx, "GET", s.baseURL+"/healthz", nil)
		if err != nil {
			return false
		}
		response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == http.StatusOK
	})
	var version struct{ Version, Revision string }
	raw := c.kube(nil, "-n", namespace, "exec", "deployment/"+namespace+"-api", "--", "mango", "version")
	require.NoError(c.t, json.Unmarshal(raw, &version))
	require.Equal(c.t, "0.1.0-alpha.2", version.Version)
	require.Equal(c.t, c.revision, version.Revision)
}
