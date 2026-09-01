// Command opensandbox-kata-audit creates one OpenSandbox resource and verifies
// that its Kubernetes workload actually uses the requested Kata RuntimeClass.
// It is an explicit operator qualification tool, not part of Mango's runtime.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/yanpgwang/mango/internal/sandbox"
)

const (
	auditCPULimit    = "1"
	auditMemoryLimit = "512Mi"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "OpenSandbox/Kata audit failed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) (returnErr error) {
	baseURL, err := requiredEnv("OPEN_SANDBOX_DOMAIN")
	if err != nil {
		return err
	}
	apiKey, err := requiredEnv("OPEN_SANDBOX_API_KEY")
	if err != nil {
		return err
	}
	if strings.HasPrefix(apiKey, "REPLACE_WITH_") {
		return errors.New("OPEN_SANDBOX_API_KEY still contains a qualification placeholder")
	}
	image, err := requiredEnv("OPEN_SANDBOX_IMAGE")
	if err != nil {
		return err
	}
	if !imageHasSHA256Digest(image) {
		return fmt.Errorf("OPEN_SANDBOX_IMAGE must be pinned by sha256 digest, got %q", image)
	}
	useProxy, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("OPEN_SANDBOX_USE_SERVER_PROXY")))
	if err != nil || !useProxy {
		return errors.New("OPEN_SANDBOX_USE_SERVER_PROXY must be true for qualification")
	}
	namespace, err := requiredEnv("OPEN_SANDBOX_KATA_NAMESPACE")
	if err != nil {
		return err
	}
	runtimeClass, err := requiredEnv("OPEN_SANDBOX_KATA_RUNTIME_CLASS")
	if err != nil {
		return err
	}
	if strings.HasPrefix(runtimeClass, "REPLACE_WITH_") {
		return errors.New("OPEN_SANDBOX_KATA_RUNTIME_CLASS still contains a qualification placeholder")
	}
	kubeContext := strings.TrimSpace(os.Getenv("OPEN_SANDBOX_KATA_KUBECTL_CONTEXT"))

	provider, err := sandbox.NewOpenSandboxProvider(sandbox.OpenSandboxConfig{
		BaseURL:  baseURL,
		APIKey:   apiKey,
		Image:    image,
		UseProxy: true,
	})
	if err != nil {
		return err
	}
	ref, box, err := provider.Create(
		ctx,
		fmt.Sprintf("mango-opensandbox-kata-audit-%d", time.Now().UnixNano()),
		sandbox.Spec{
			Timeout: 2 * time.Minute, Network: "none",
			CPUs: auditCPULimit, Memory: auditMemoryLimit,
		},
	)
	if err != nil {
		return fmt.Errorf("create audit sandbox: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := box.Destroy(cleanupCtx); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("destroy audit sandbox: %w", err)
		}
	}()

	result, err := box.Exec(ctx, sandbox.Command{
		Path: "/bin/sh",
		Args: []string{"-c", "test -n \"$OPENSANDBOX_ID\" && test \"$OPENSANDBOX_ID\" = \"$1\"", "audit", ref.ID},
	})
	if err != nil {
		return fmt.Errorf("execute audit probe: %w", err)
	}
	if result.TimedOut || result.ExitCode != 0 {
		return fmt.Errorf(
			"audit probe failed (exit=%d timeout=%t stderr=%q)",
			result.ExitCode, result.TimedOut, strings.TrimSpace(string(result.Stderr)),
		)
	}
	if err := verifyKataRuntime(
		ctx, kubeContext, namespace, runtimeClass, ref.ID, image,
		auditCPULimit, auditMemoryLimit,
	); err != nil {
		return err
	}
	fmt.Printf(
		"OpenSandbox sandbox %s uses RuntimeClass %s in namespace %s\n",
		ref.ID, runtimeClass, namespace,
	)
	return nil
}

func verifyKataRuntime(
	ctx context.Context,
	kubeContext string,
	namespace string,
	runtimeClass string,
	sandboxID string,
	image string,
	cpuLimit string,
	memoryLimit string,
) error {
	if _, err := runKubectl(
		ctx, kubeContext, "get", "runtimeclass", runtimeClass, "-o", "name",
	); err != nil {
		return fmt.Errorf("verify RuntimeClass %q: %w", runtimeClass, err)
	}
	data, err := runKubectl(
		ctx, kubeContext, "-n", namespace, "get", "batchsandbox", sandboxID, "-o", "json",
	)
	if err != nil {
		return fmt.Errorf("read BatchSandbox %q: %w", sandboxID, err)
	}
	gotRuntimeClass, err := batchSandboxRuntimeClass(data)
	if err != nil {
		return fmt.Errorf("decode BatchSandbox %q: %w", sandboxID, err)
	}
	if gotRuntimeClass != runtimeClass {
		return fmt.Errorf(
			"BatchSandbox %q runtimeClassName = %q, want %q",
			sandboxID, gotRuntimeClass, runtimeClass,
		)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		data, listErr := runKubectl(
			ctx, kubeContext, "-n", namespace, "get", "pods", "-o", "json",
		)
		if listErr == nil {
			pod, found, parseErr := findOpenSandboxPod(data, sandboxID)
			if parseErr != nil {
				return fmt.Errorf("decode sandbox pods: %w", parseErr)
			}
			if found {
				if err := validateKataPod(
					pod, runtimeClass, image, cpuLimit, memoryLimit,
				); err != nil {
					return fmt.Errorf("OpenSandbox pod %q: %w", pod.Name, err)
				}
				return nil
			}
		}
		if time.Now().After(deadline) {
			if listErr != nil {
				return fmt.Errorf("list sandbox pods: %w", listErr)
			}
			return fmt.Errorf(
				"no pod in namespace %q exposes OPENSANDBOX_ID=%q",
				namespace, sandboxID,
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func batchSandboxRuntimeClass(data []byte) (string, error) {
	var workload struct {
		Spec struct {
			Template struct {
				Spec struct {
					RuntimeClassName string `json:"runtimeClassName"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &workload); err != nil {
		return "", err
	}
	value := strings.TrimSpace(workload.Spec.Template.Spec.RuntimeClassName)
	if value == "" {
		return "", errors.New("spec.template.spec.runtimeClassName is empty")
	}
	return value, nil
}

type podEnvironment struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type podContainer struct {
	Name            string                      `json:"name"`
	Image           string                      `json:"image"`
	Env             []podEnvironment            `json:"env"`
	Resources       podResources                `json:"resources"`
	SecurityContext podContainerSecurityContext `json:"securityContext"`
}

type podResources struct {
	Limits   map[string]string `json:"limits"`
	Requests map[string]string `json:"requests"`
}

type podSeccompProfile struct {
	Type string `json:"type"`
}

type podCapabilities struct {
	Add  []string `json:"add"`
	Drop []string `json:"drop"`
}

type podContainerSecurityContext struct {
	AllowPrivilegeEscalation *bool              `json:"allowPrivilegeEscalation"`
	Privileged               *bool              `json:"privileged"`
	Capabilities             podCapabilities    `json:"capabilities"`
	SeccompProfile           *podSeccompProfile `json:"seccompProfile"`
}

type kataPod struct {
	Name                         string
	RuntimeClassName             string
	AutomountServiceAccountToken *bool
	HostNetwork                  bool
	HostPID                      bool
	HostIPC                      bool
	SeccompProfile               *podSeccompProfile
	InitContainers               []podContainer
	Containers                   []podContainer
}

func findOpenSandboxPod(data []byte, sandboxID string) (kataPod, bool, error) {
	var pods struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				RuntimeClassName             string `json:"runtimeClassName"`
				AutomountServiceAccountToken *bool  `json:"automountServiceAccountToken"`
				HostNetwork                  bool   `json:"hostNetwork"`
				HostPID                      bool   `json:"hostPID"`
				HostIPC                      bool   `json:"hostIPC"`
				SecurityContext              struct {
					SeccompProfile *podSeccompProfile `json:"seccompProfile"`
				} `json:"securityContext"`
				InitContainers []podContainer `json:"initContainers"`
				Containers     []podContainer `json:"containers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &pods); err != nil {
		return kataPod{}, false, err
	}
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			for _, variable := range container.Env {
				if variable.Name == "OPENSANDBOX_ID" && variable.Value == sandboxID {
					return kataPod{
						Name: pod.Metadata.Name, RuntimeClassName: pod.Spec.RuntimeClassName,
						AutomountServiceAccountToken: pod.Spec.AutomountServiceAccountToken,
						HostNetwork:                  pod.Spec.HostNetwork, HostPID: pod.Spec.HostPID,
						HostIPC:        pod.Spec.HostIPC,
						SeccompProfile: pod.Spec.SecurityContext.SeccompProfile,
						InitContainers: pod.Spec.InitContainers,
						Containers:     pod.Spec.Containers,
					}, true, nil
				}
			}
		}
	}
	return kataPod{}, false, nil
}

func validateKataPod(
	pod kataPod,
	runtimeClass string,
	image string,
	cpuLimit string,
	memoryLimit string,
) error {
	if pod.RuntimeClassName != runtimeClass {
		return fmt.Errorf(
			"runtimeClassName = %q, want %q", pod.RuntimeClassName, runtimeClass,
		)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		return errors.New("automountServiceAccountToken must be explicitly false")
	}
	if pod.HostNetwork || pod.HostPID || pod.HostIPC {
		return errors.New("hostNetwork, hostPID, and hostIPC must all be false")
	}
	if !safeSeccompProfile(pod.SeccompProfile) {
		return errors.New("pod seccompProfile must be RuntimeDefault or Localhost")
	}
	var sandboxContainer *podContainer
	var egressContainer *podContainer
	for index := range pod.Containers {
		container := &pod.Containers[index]
		if container.SecurityContext.AllowPrivilegeEscalation == nil ||
			*container.SecurityContext.AllowPrivilegeEscalation {
			return fmt.Errorf(
				"container %q allowPrivilegeEscalation must be explicitly false",
				container.Name,
			)
		}
		if container.SecurityContext.Privileged != nil &&
			*container.SecurityContext.Privileged {
			return fmt.Errorf("container %q must not be privileged", container.Name)
		}
		if container.SecurityContext.SeccompProfile != nil &&
			!safeSeccompProfile(container.SecurityContext.SeccompProfile) {
			return fmt.Errorf("container %q uses an unsafe seccomp profile", container.Name)
		}
		switch container.Name {
		case "sandbox":
			if len(container.SecurityContext.Capabilities.Add) != 0 {
				return fmt.Errorf(
					"sandbox container adds capabilities %v",
					container.SecurityContext.Capabilities.Add,
				)
			}
			sandboxContainer = container
		case "egress":
			if !stringSetEqual(
				container.SecurityContext.Capabilities.Add, []string{"NET_ADMIN"},
			) {
				return fmt.Errorf(
					"egress container capabilities.add = %v, want [NET_ADMIN]",
					container.SecurityContext.Capabilities.Add,
				)
			}
			egressContainer = container
		default:
			return fmt.Errorf("unexpected regular container %q", container.Name)
		}
	}
	if sandboxContainer == nil {
		return errors.New("sandbox container is missing")
	}
	if egressContainer == nil {
		return errors.New("egress container is missing")
	}
	if sandboxContainer.Image != image {
		return fmt.Errorf(
			"sandbox image = %q, want %q", sandboxContainer.Image, image,
		)
	}
	for resource, want := range map[string]string{
		"cpu": cpuLimit, "memory": memoryLimit,
	} {
		if got := sandboxContainer.Resources.Limits[resource]; got != want {
			return fmt.Errorf("sandbox limit %s = %q, want %q", resource, got, want)
		}
		if got := sandboxContainer.Resources.Requests[resource]; got != want {
			return fmt.Errorf("sandbox request %s = %q, want %q", resource, got, want)
		}
	}
	execdInstallerFound := false
	for _, container := range pod.InitContainers {
		if container.Name == "execd-installer" {
			execdInstallerFound = true
		}
		if container.Name != "execd-installer" &&
			container.SecurityContext.Privileged != nil &&
			*container.SecurityContext.Privileged {
			return fmt.Errorf("unexpected privileged init container %q", container.Name)
		}
		if !imageHasSHA256Digest(container.Image) {
			return fmt.Errorf("init container %q image is not digest-pinned", container.Name)
		}
	}
	if !execdInstallerFound {
		return errors.New("execd-installer init container is missing")
	}
	if !imageHasSHA256Digest(egressContainer.Image) {
		return errors.New("egress container image is not digest-pinned")
	}
	return nil
}

func safeSeccompProfile(profile *podSeccompProfile) bool {
	if profile == nil {
		return false
	}
	return profile.Type == "RuntimeDefault" || profile.Type == "Localhost"
}

func stringSetEqual(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		counts[value]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func runKubectl(ctx context.Context, kubeContext string, args ...string) ([]byte, error) {
	if kubeContext != "" {
		args = append([]string{"--context", kubeContext}, args...)
	}
	command := exec.CommandContext(ctx, "kubectl", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf(
			"kubectl %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(output)),
		)
	}
	return output, nil
}

func requiredEnv(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func imageHasSHA256Digest(value string) bool {
	marker := strings.LastIndex(value, "@sha256:")
	if marker <= 0 {
		return false
	}
	digest := value[marker+len("@sha256:"):]
	if len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}
