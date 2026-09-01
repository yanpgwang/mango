package sandbox

import (
	"reflect"
	"testing"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

func TestOpenSandboxNetworkPolicy(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want *opensandbox.NetworkPolicy
	}{
		{
			name: "limited",
			spec: Spec{Network: "limited", NetworkAllowedHosts: []string{
				"api.example.com", "*.assets.example.com",
			}},
			want: &opensandbox.NetworkPolicy{
				DefaultAction: "deny",
				Egress: []opensandbox.NetworkRule{
					{Action: "allow", Target: "api.example.com"},
					{Action: "allow", Target: "*.assets.example.com"},
				},
			},
		},
		{name: "none", spec: Spec{Network: "none"}, want: &opensandbox.NetworkPolicy{DefaultAction: "deny"}},
		{name: "empty", spec: Spec{}, want: &opensandbox.NetworkPolicy{DefaultAction: "deny"}},
		{name: "unrestricted", spec: Spec{Network: "bridge"}},
		{name: "unknown fails closed", spec: Spec{Network: "brigde"}, want: &opensandbox.NetworkPolicy{DefaultAction: "deny"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := openSandboxNetworkPolicy(test.spec); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("network policy = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestValidateSandboxNetworkSpec(t *testing.T) {
	for _, mode := range []string{"", "none", "bridge", "limited"} {
		if err := validateSandboxNetworkSpec(Spec{Network: mode}); err != nil {
			t.Errorf("mode %q rejected: %v", mode, err)
		}
	}
	if err := validateSandboxNetworkSpec(Spec{Network: "brigde"}); err == nil {
		t.Fatal("unknown network mode succeeded")
	}
	if err := validateSandboxNetworkSpec(Spec{
		Network: "bridge", NetworkAllowedHosts: []string{"example.com"},
	}); err == nil {
		t.Fatal("unrestricted network accepted an allowlist")
	}
}

func TestBindingPackageProofSurvivesNetworkPolicyChange(t *testing.T) {
	original := Spec{
		Network: "limited", NetworkAllowedHosts: []string{"old.example.com"},
		Packages: PackageSet{NPM: []string{"typescript@5.9.2"}},
	}
	updated := original
	updated.NetworkAllowedHosts = []string{"new.example.com"}
	if !bindingProvesPackageSetup(bindingSpecHash(original), updated) {
		t.Fatal("network-only change invalidated package setup proof")
	}
	updated.Packages.NPM = []string{"typescript@6.0.0"}
	if bindingProvesPackageSetup(bindingSpecHash(original), updated) {
		t.Fatal("changed package plan retained setup proof")
	}
}
