package main

import (
	"strings"
	"testing"
)

func TestBatchSandboxRuntimeClass(t *testing.T) {
	data := []byte(`{"spec":{"template":{"spec":{"runtimeClassName":"kata-qemu"}}}}`)
	got, err := batchSandboxRuntimeClass(data)
	if err != nil {
		t.Fatal(err)
	}
	if got != "kata-qemu" {
		t.Fatalf("runtimeClassName = %q, want kata-qemu", got)
	}
	if _, err := batchSandboxRuntimeClass([]byte(`{"spec":{}}`)); err == nil {
		t.Fatal("missing runtimeClassName succeeded")
	}
	if _, err := batchSandboxRuntimeClass([]byte(`{`)); err == nil {
		t.Fatal("malformed BatchSandbox succeeded")
	}
}

func TestFindOpenSandboxPod(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	data := []byte(`{
  "items": [
    {
      "metadata": {"name": "unrelated"},
      "spec": {"runtimeClassName": "runc", "containers": []}
    },
    {
      "metadata": {"name": "sandbox-pod"},
      "spec": {
        "runtimeClassName": "kata-qemu",
        "automountServiceAccountToken": false,
        "securityContext": {"seccompProfile": {"type": "RuntimeDefault"}},
        "initContainers": [
          {"name": "execd-installer", "image": "registry.test/execd@` + digest + `",
           "securityContext": {"privileged": true}}
        ],
        "containers": [
          {"name": "sandbox", "image": "registry.test/agent@` + digest + `",
           "env": [{"name": "OPENSANDBOX_ID", "value": "sbx_123"}],
           "resources": {"limits": {"cpu": "1", "memory": "512Mi"},
                         "requests": {"cpu": "1", "memory": "512Mi"}},
           "securityContext": {"allowPrivilegeEscalation": false, "privileged": false}},
          {"name": "egress", "image": "registry.test/egress@` + digest + `",
           "securityContext": {"allowPrivilegeEscalation": false, "privileged": false,
                               "capabilities": {"add": ["NET_ADMIN"]}}}
        ]
      }
    }
  ]
}`)
	pod, found, err := findOpenSandboxPod(data, "sbx_123")
	if err != nil {
		t.Fatal(err)
	}
	if !found || pod.Name != "sandbox-pod" || pod.RuntimeClassName != "kata-qemu" {
		t.Fatalf("pod = (%+v, %t), want sandbox-pod/kata-qemu", pod, found)
	}
	image := "registry.test/agent@" + digest
	if err := validateKataPod(pod, "kata-qemu", image, "1", "512Mi"); err != nil {
		t.Fatalf("valid Kata Pod rejected: %v", err)
	}
	if _, found, err := findOpenSandboxPod(data, "sbx_missing"); err != nil || found {
		t.Fatalf("missing pod = (found=%t, err=%v), want false, nil", found, err)
	}
	if _, _, err := findOpenSandboxPod([]byte(`{`), "sbx_123"); err == nil {
		t.Fatal("malformed pod list succeeded")
	}
}

func TestValidateKataPodRejectsUnsafeOrUnboundedSandbox(t *testing.T) {
	pod := validKataPodForTest()
	pod.Containers[0].SecurityContext.AllowPrivilegeEscalation = nil
	if err := validateKataPod(
		pod, "kata-qemu", pod.Containers[0].Image, "1", "512Mi",
	); err == nil {
		t.Fatal("missing allowPrivilegeEscalation boundary succeeded")
	}

	pod = validKataPodForTest()
	pod.HostPID = true
	if err := validateKataPod(
		pod, "kata-qemu", pod.Containers[0].Image, "1", "512Mi",
	); err == nil {
		t.Fatal("host PID namespace succeeded")
	}

	pod = validKataPodForTest()
	delete(pod.Containers[0].Resources.Limits, "memory")
	if err := validateKataPod(
		pod, "kata-qemu", pod.Containers[0].Image, "1", "512Mi",
	); err == nil {
		t.Fatal("missing memory limit succeeded")
	}

	pod = validKataPodForTest()
	pod.Containers[0].SecurityContext.Capabilities.Add = []string{"SYS_ADMIN"}
	if err := validateKataPod(
		pod, "kata-qemu", pod.Containers[0].Image, "1", "512Mi",
	); err == nil {
		t.Fatal("sandbox SYS_ADMIN capability succeeded")
	}

	pod = validKataPodForTest()
	pod.Containers = pod.Containers[:1]
	if err := validateKataPod(
		pod, "kata-qemu", pod.Containers[0].Image, "1", "512Mi",
	); err == nil {
		t.Fatal("missing egress container succeeded")
	}

	pod = validKataPodForTest()
	pod.InitContainers = nil
	if err := validateKataPod(
		pod, "kata-qemu", pod.Containers[0].Image, "1", "512Mi",
	); err == nil {
		t.Fatal("missing execd installer succeeded")
	}

	pod = validKataPodForTest()
	pod.Containers = append(pod.Containers, podContainer{
		Name: "injected-helper", Image: "registry.test/helper:latest",
		SecurityContext: podContainerSecurityContext{
			AllowPrivilegeEscalation: boolPointer(false),
		},
	})
	if err := validateKataPod(
		pod, "kata-qemu", pod.Containers[0].Image, "1", "512Mi",
	); err == nil {
		t.Fatal("unknown unpinned regular sidecar succeeded")
	}
}

func boolPointer(value bool) *bool { return &value }

func validKataPodForTest() kataPod {
	falseValue := false
	digest := "sha256:" + strings.Repeat("a", 64)
	return kataPod{
		Name: "sandbox-pod", RuntimeClassName: "kata-qemu",
		AutomountServiceAccountToken: &falseValue,
		SeccompProfile:               &podSeccompProfile{Type: "RuntimeDefault"},
		InitContainers: []podContainer{{
			Name: "execd-installer", Image: "registry.test/execd@" + digest,
		}},
		Containers: []podContainer{
			{
				Name: "sandbox", Image: "registry.test/agent@" + digest,
				Resources: podResources{
					Limits:   map[string]string{"cpu": "1", "memory": "512Mi"},
					Requests: map[string]string{"cpu": "1", "memory": "512Mi"},
				},
				SecurityContext: podContainerSecurityContext{
					AllowPrivilegeEscalation: &falseValue,
				},
			},
			{
				Name: "egress", Image: "registry.test/egress@" + digest,
				SecurityContext: podContainerSecurityContext{
					AllowPrivilegeEscalation: &falseValue,
					Capabilities:             podCapabilities{Add: []string{"NET_ADMIN"}},
				},
			},
		},
	}
}

func TestImageHasSHA256Digest(t *testing.T) {
	valid := "registry.example.test/agent@sha256:" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if !imageHasSHA256Digest(valid) {
		t.Fatalf("valid digest rejected: %s", valid)
	}
	for _, value := range []string{
		"python:3.12-slim",
		"registry.example.test/agent@sha256:short",
		"registry.example.test/agent@sha256:" +
			"zz23456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		if imageHasSHA256Digest(value) {
			t.Fatalf("invalid digest accepted: %s", value)
		}
	}
}
