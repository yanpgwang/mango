package helm_test

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type document = map[string]any

func helm(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	if os.Getenv("MANGO_TEST_HELM") != "1" {
		t.Skip("set MANGO_TEST_HELM=1 to require Helm chart contracts")
	}
	binary := os.Getenv("MANGO_HELM_PATH")
	if binary == "" {
		binary = "helm"
	}
	return exec.Command(binary, args...).CombinedOutput()
}

func render(t *testing.T, release string, extra ...string) map[string]document {
	t.Helper()
	args := append([]string{"template", release, "../../charts/mango", "--namespace", "alpha", "-f", "testdata/values.yaml"}, extra...)
	output, err := helm(t, args...)
	if err != nil {
		t.Fatalf("render: %v\n%s", err, output)
	}
	result := map[string]document{}
	decoder := yaml.NewDecoder(strings.NewReader(string(output)))
	for {
		var object document
		if err := decoder.Decode(&object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if len(object) == 0 {
			continue
		}
		metadata := field(t, object, "metadata")
		if metadata["namespace"] != "alpha" {
			t.Fatalf("resource has wrong namespace: %v", metadata)
		}
		key := fmt.Sprintf("%s/%s", object["kind"], metadata["name"])
		if _, exists := result[key]; exists {
			t.Fatalf("duplicate resource %s", key)
		}
		result[key] = object
	}
	return result
}

func field(t *testing.T, parent document, path ...string) document {
	t.Helper()
	for _, key := range path {
		value, ok := parent[key].(map[string]any)
		if !ok {
			t.Fatalf("missing object %s: %v", key, parent)
		}
		parent = value
	}
	return parent
}

func container(t *testing.T, pod document) document {
	t.Helper()
	items, ok := pod["containers"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("expected exactly one role container: %v", pod)
	}
	return items[0].(map[string]any)
}

func env(t *testing.T, item document) map[string]document {
	t.Helper()
	if _, exists := item["envFrom"]; exists {
		t.Fatal("credential selection must not use envFrom")
	}
	result := map[string]document{}
	for _, raw := range item["env"].([]any) {
		value := raw.(map[string]any)
		name := value["name"].(string)
		if _, duplicate := result[name]; duplicate {
			t.Fatalf("duplicate env %s", name)
		}
		result[name] = value
	}
	return result
}

func secret(t *testing.T, value document, name, key string) {
	t.Helper()
	got := field(t, value, "valueFrom", "secretKeyRef")
	if got["name"] != name || got["key"] != key {
		t.Fatalf("secret reference = %v; want %s/%s", got, name, key)
	}
}

func TestChartControlPlaneRoles(t *testing.T) {
	objects := render(t, "alpha")
	if len(objects) != 5 {
		t.Fatalf("resources = %d; want two Deployments, Service, ConfigMap and Job", len(objects))
	}
	for _, role := range []string{"api", "orchestrator", "migrate"} {
		kind := "Deployment"
		if role == "migrate" {
			kind = "Job"
		}
		pod := field(t, objects[kind+"/alpha-"+role], "spec", "template", "spec")
		item := container(t, pod)
		if item["image"] != "ghcr.io/yanpgwang/mango:0.1.0-alpha.2" || pod["automountServiceAccountToken"] != false {
			t.Fatalf("%s image/token policy: %v", role, pod)
		}
		security := field(t, pod, "securityContext")
		for _, key := range []string{"runAsUser", "runAsGroup", "fsGroup"} {
			if security[key] != 65532 {
				t.Fatalf("%s %s = %v", role, key, security[key])
			}
		}
		if security["runAsNonRoot"] != true || field(t, security, "seccompProfile")["type"] != "RuntimeDefault" {
			t.Fatalf("%s pod security: %v", role, security)
		}
		cs := field(t, item, "securityContext")
		if cs["allowPrivilegeEscalation"] != false || cs["readOnlyRootFilesystem"] != true || !reflect.DeepEqual(field(t, cs, "capabilities")["drop"], []any{"ALL"}) {
			t.Fatalf("%s container security: %v", role, cs)
		}
		resources := field(t, item, "resources")
		for _, level := range []string{"requests", "limits"} {
			for _, key := range []string{"cpu", "memory", "ephemeral-storage"} {
				if field(t, resources, level)[key] == nil {
					t.Fatalf("%s omitted %s.%s", role, level, key)
				}
			}
		}
		volumes := pod["volumes"].([]any)
		if len(volumes) != 1 || field(t, volumes[0].(map[string]any), "emptyDir")["sizeLimit"] == nil {
			t.Fatalf("%s must have a bounded writable temporary volume only: %v", role, volumes)
		}
		mount := item["volumeMounts"].([]any)[0].(map[string]any)
		if mount["mountPath"] != "/tmp" || mount["readOnly"] == true {
			t.Fatalf("%s temporary mount: %v", role, mount)
		}
		environment := env(t, item)
		secret(t, environment["MANGO_DATABASE_URL"], "fixture-database", "url")
		if role == "migrate" {
			if len(environment) != 1 || !reflect.DeepEqual(item["args"], []any{"migrate"}) {
				t.Fatalf("migration must receive only DB configuration: %v", item)
			}
			continue
		}
		secret(t, environment["MANGO_NATS_URL"], "fixture-nats", "url")
		secret(t, environment["MANGO_FILE_S3_ACCESS_KEY"], "fixture-s3", "access")
		secret(t, environment["MANGO_FILE_S3_SECRET_KEY"], "fixture-s3", "secret")
		for _, name := range []string{"MANGO_TEMPORAL_HOSTPORT", "MANGO_TEMPORAL_NAMESPACE", "MANGO_FILE_S3_ENDPOINT", "MANGO_FILE_S3_REGION", "MANGO_FILE_S3_BUCKET", "MANGO_FILE_S3_PATH_STYLE", "MANGO_FILE_S3_CREATE_BUCKET", "MANGO_FILE_UPLOAD_TEMP_DIR"} {
			ref := field(t, environment[name], "valueFrom", "configMapKeyRef")
			if ref["name"] != "alpha-config" || ref["key"] != name {
				t.Fatalf("%s configuration reference: %v", role, ref)
			}
		}
		if role == "api" {
			secret(t, environment["MANGO_API_KEY"], "fixture-auth", "token")
			if !reflect.DeepEqual(item["args"], []any{"serve", "-addr", ":8080"}) || environment["MANGO_MODEL_API_KEY"] != nil {
				t.Fatalf("API leaked model configuration or has wrong role: %v", item)
			}
			for _, probe := range []string{"startupProbe", "livenessProbe"} {
				if field(t, item, probe, "httpGet")["path"] != "/healthz" {
					t.Fatalf("incorrect %s", probe)
				}
			}
			if field(t, item, "readinessProbe", "httpGet")["path"] != "/readyz" || field(t, item, "readinessProbe")["timeoutSeconds"] != 3 {
				t.Fatal("readiness must exceed the two-second database bound")
			}
			if pod["terminationGracePeriodSeconds"] != 15 {
				t.Fatal("API termination grace")
			}
		} else {
			secret(t, environment["MANGO_MODEL_API_KEY"], "fixture-model", "credential")
			if environment["MANGO_API_KEY"] != nil || !reflect.DeepEqual(item["args"], []any{"orchestrate"}) || pod["terminationGracePeriodSeconds"] != 60 {
				t.Fatalf("orchestration role configuration: %v", item)
			}
			if _, exists := item["readinessProbe"]; exists {
				t.Fatal("orchestration must not fabricate provider readiness")
			}
			if field(t, objects["Deployment/alpha-orchestrator"], "spec", "strategy")["type"] != "Recreate" {
				t.Fatal("orchestration replacement must use Recreate")
			}
		}
	}
	job := objects["Job/alpha-migrate"]
	annotations := field(t, job, "metadata", "annotations")
	if annotations["helm.sh/hook"] != "pre-install" || annotations["helm.sh/hook-delete-policy"] != "before-hook-creation,hook-succeeded" {
		t.Fatalf("migration hook lifecycle: %v", annotations)
	}
	js := field(t, job, "spec")
	if js["activeDeadlineSeconds"] != 300 || js["backoffLimit"] != 3 || js["ttlSecondsAfterFinished"] != 3600 {
		t.Fatal("unbounded migration hook")
	}
	service := field(t, objects["Service/alpha-api"], "spec")
	if service["type"] != "ClusterIP" || !reflect.DeepEqual(service["selector"], field(t, objects["Deployment/alpha-api"], "spec", "selector", "matchLabels")) {
		t.Fatal("Service selector/type does not map to API Pods")
	}
	for _, role := range []string{"api", "orchestrator"} {
		d := objects["Deployment/alpha-"+role]
		selector := field(t, d, "spec", "selector", "matchLabels")
		labels := field(t, d, "spec", "template", "metadata", "labels")
		for key, value := range selector {
			if labels[key] != value {
				t.Fatalf("%s selector mismatch", role)
			}
		}
	}
	data := field(t, objects["ConfigMap/alpha-config"], "data")
	for key, expected := range map[string]string{"MANGO_TEMPORAL_HOSTPORT": "temporal.operator.svc:7233", "MANGO_TEMPORAL_NAMESPACE": "default", "MANGO_FILE_S3_ENDPOINT": "http://s3.operator.svc:8333", "MANGO_FILE_S3_BUCKET": "mango-fixture", "MANGO_FILE_S3_REGION": "us-east-1", "MANGO_FILE_S3_PATH_STYLE": "true", "MANGO_FILE_S3_CREATE_BUCKET": "false", "MANGO_FILE_UPLOAD_TEMP_DIR": "/tmp", "MANGO_MODEL_BASE_URL": "https://model.example.test", "MANGO_MODEL_ID": "fixture-model", "MANGO_MODEL_AUTH": "bearer"} {
		if data[key] != expected {
			t.Fatalf("configuration %s = %v; want %s", key, data[key], expected)
		}
	}
	if len(data) != 11 {
		t.Fatal("unexpected nonsecret configuration fields")
	}
}

func TestChartRejectsMissingAndInvalidConfiguration(t *testing.T) {
	if output, err := helm(t, "template", "alpha", "../../charts/mango"); err == nil {
		t.Fatalf("empty configuration accepted: %s", output)
	}
	for _, override := range []string{"model.baseURL=", "model.baseUrl=typo", "vault.enabled=true", "image.digest=not-a-digest", "image.tag=latest", "database.existingSecret.key="} {
		if output, err := helm(t, "template", "alpha", "../../charts/mango", "-f", "testdata/values.yaml", "--set", override); err == nil {
			t.Fatalf("invalid %s accepted: %s", override, output)
		}
	}
}

func TestChartOptionalKeyringMigrationRoleAndRestore(t *testing.T) {
	objects := render(t, "alpha", "--set", "vault.enabled=true,vault.existingSecret.name=fixture-vault,vault.existingSecret.key=keyring.json,database.migrationSecret.name=fixture-ddl,database.migrationSecret.key=owner-url")
	secret(t, env(t, container(t, field(t, objects["Job/alpha-migrate"], "spec", "template", "spec")))["MANGO_DATABASE_URL"], "fixture-ddl", "owner-url")
	for _, role := range []string{"api", "orchestrator"} {
		pod := field(t, objects["Deployment/alpha-"+role], "spec", "template", "spec")
		volumes := pod["volumes"].([]any)
		if len(volumes) != 2 {
			t.Fatal("keyring volume missing")
		}
		keyring := field(t, volumes[1].(map[string]any), "secret")
		if keyring["secretName"] != "fixture-vault" || keyring["defaultMode"] != 288 || len(keyring["items"].([]any)) != 1 {
			t.Fatalf("keyring not selected/readable by fsGroup: %v", keyring)
		}
		item := keyring["items"].([]any)[0].(map[string]any)
		if item["key"] != "keyring.json" || item["path"] != "keyring.json" {
			t.Fatal("wrong keyring file selection")
		}
		if env(t, container(t, pod))["MANGO_VAULT_KEYRING_FILE"]["value"] != "/run/mango/keyring/keyring.json" {
			t.Fatal("keyring env does not match mounted file")
		}
		mounts := container(t, pod)["volumeMounts"].([]any)
		if len(mounts) != 2 || mounts[1].(map[string]any)["mountPath"] != "/run/mango/keyring" || mounts[1].(map[string]any)["readOnly"] != true {
			t.Fatal("keyring must be mounted read-only at the selected directory")
		}
	}
	if restored := render(t, "alpha", "--set", "migration.enabled=false"); len(restored) != 4 {
		t.Fatal("same-release restore must allow the initialization hook to be disabled")
	}
}

func TestChartDigestAndLongResourceNames(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	first := render(t, strings.Repeat("a", 52)+"x", "--set", "image.digest="+digest)
	second := render(t, strings.Repeat("a", 52)+"y", "--set", "image.digest="+digest)
	for key, object := range first {
		if len(field(t, object, "metadata")["name"].(string)) > 63 || second[key] != nil {
			t.Fatalf("long resource name invalid or colliding: %s", key)
		}
		if object["kind"] == "Job" || object["kind"] == "Deployment" {
			pod := field(t, object, "spec", "template", "spec")
			if container(t, pod)["image"] != "ghcr.io/yanpgwang/mango@"+digest {
				t.Fatal("image digest did not override the tag")
			}
		}
	}
}

func TestChartLintAndPackageExcludesLocalFiles(t *testing.T) {
	if output, err := helm(t, "lint", "--strict", "../../charts/mango", "-f", "testdata/values.yaml"); err != nil {
		t.Fatalf("lint: %v\n%s", err, output)
	}
	folder := t.TempDir()
	chart := filepath.Join(folder, "mango")
	err := filepath.WalkDir("../../charts/mango", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel("../../charts/mango", path)
		if err != nil {
			return err
		}
		target := filepath.Join(chart, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".env", "dev.env", "local.key", "scratch.log"} {
		if err := os.WriteFile(filepath.Join(chart, name), []byte("synthetic private packaging marker"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := helm(t, "package", chart, "--destination", folder); err != nil {
		t.Fatalf("package: %v\n%s", err, output)
	}
	file, err := os.Open(filepath.Join(folder, "mango-0.1.0-alpha.2.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = compressed.Close() }()
	archive := tar.NewReader(compressed)
	required := map[string]bool{"mango/Chart.yaml": false, "mango/values.yaml": false, "mango/values.schema.json": false, "mango/LICENSE": false}
	allowed := map[string]bool{"mango/README.md": true, "mango/.helmignore": true}
	for _, name := range []string{"_helpers.tpl", "configmap.yaml", "api-deployment.yaml", "orchestrator-deployment.yaml", "service.yaml", "migration-job.yaml", "NOTES.txt"} {
		allowed["mango/templates/"+name] = true
	}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := header.Name
		if header.Typeflag != tar.TypeReg || (!allowed[name] && !hasKey(required, name)) {
			t.Fatalf("unexpected chart distribution input: %s", name)
		}
		if hasKey(required, name) {
			required[name] = true
		}
	}
	for name, present := range required {
		if !present {
			t.Fatalf("chart package missing %s", name)
		}
	}
}

func hasKey(values map[string]bool, key string) bool {
	_, exists := values[key]
	return exists
}
