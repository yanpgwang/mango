package domain

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestParseTools_BuiltinCustomMCP(t *testing.T) {
	raw := []any{
		map[string]any{
			"type":           "agent_toolset_20260401",
			"default_config": map[string]any{"enabled": true, "permission_policy": map[string]any{"type": "always_allow"}},
			"configs": []any{
				map[string]any{"name": "bash", "permission_policy": map[string]any{"type": "always_ask"}},
				map[string]any{"name": "web_fetch", "enabled": false},
			},
		},
		map[string]any{"type": "custom", "name": "get_weather", "description": "d",
			"input_schema": map[string]any{"type": "object"}},
		map[string]any{
			"type":            "mcp_toolset",
			"mcp_server_name": "github",
			"configs": []any{map[string]any{
				"name": "delete_issue",
				"permission_policy": map[string]any{
					"type": "always_allow",
				},
			}},
		},
	}
	ts, err := ParseTools(raw)
	if err != nil {
		t.Fatal(err)
	}
	if ts.Builtin == nil || len(ts.Custom) != 1 || len(ts.MCP) != 1 {
		t.Fatalf("parsed = %+v", ts)
	}
	if ts.Custom[0].Name != "get_weather" {
		t.Fatalf("custom = %+v", ts.Custom)
	}
	// bash overridden to always_ask, still enabled by default
	if en, pol := ts.BuiltinEnabled("bash"); !en || pol.Type != "always_ask" {
		t.Fatalf("bash en=%v pol=%v", en, pol.Type)
	}
	// web_fetch disabled
	if en, _ := ts.BuiltinEnabled("web_fetch"); en {
		t.Fatal("web_fetch should be disabled")
	}
	// read: default enabled + default always_allow
	if en, pol := ts.BuiltinEnabled("read"); !en || pol.Type != "always_allow" {
		t.Fatalf("read en=%v pol=%v", en, pol.Type)
	}
	if en, pol := ts.MCP[0].ToolEnabled("list_issues"); !en || pol.Type != "always_ask" {
		t.Fatalf("MCP default en=%v pol=%v", en, pol.Type)
	}
	if en, pol := ts.MCP[0].ToolEnabled("delete_issue"); !en || pol.Type != "always_allow" {
		t.Fatalf("MCP override en=%v pol=%v", en, pol.Type)
	}
}

func TestParseTools_RejectsUnknownType(t *testing.T) {
	_, err := ParseTools([]any{map[string]any{"type": "bogus"}})
	if err == nil {
		t.Fatal("expected error for unknown tool type")
	}
}

func TestValidateToolConfiguration_RejectsUnsupportedToolFields(t *testing.T) {
	cases := map[string][]any{
		"built-in toolset": {map[string]any{
			"type": BuiltinToolsetType, "authorization_token": "secret",
		}},
		"built-in default_config": {map[string]any{
			"type":           BuiltinToolsetType,
			"default_config": map[string]any{"enabled": true, "sudo": true},
		}},
		"built-in config": {map[string]any{
			"type":    BuiltinToolsetType,
			"configs": []any{map[string]any{"name": "bash", "sudo": true}},
		}},
		"permission policy": {map[string]any{
			"type": BuiltinToolsetType,
			"configs": []any{map[string]any{
				"name":              "bash",
				"permission_policy": map[string]any{"type": "always_allow", "scope": "all"},
			}},
		}},
		"custom tool": {map[string]any{
			"type": "custom", "name": "weather", "handler_url": "https://example.com",
		}},
		"mcp toolset": {map[string]any{
			"type": "mcp_toolset", "mcp_server_name": "github", "headers": map[string]any{},
		}},
		"mcp default_config": {map[string]any{
			"type": "mcp_toolset", "mcp_server_name": "github",
			"default_config": map[string]any{"enabled": true, "headers": map[string]any{}},
		}},
		"mcp config": {map[string]any{
			"type": "mcp_toolset", "mcp_server_name": "github",
			"configs": []any{map[string]any{"name": "list_issues", "api_key": "secret"}},
		}},
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateToolWireShape(raw, nil); err == nil {
				t.Fatalf("unsupported tool field was accepted: %#v", raw)
			}
		})
	}
}

func TestValidateToolConfiguration_RejectsMalformedOptionalValues(t *testing.T) {
	cases := map[string][]any{
		"default_config not object": {map[string]any{
			"type": BuiltinToolsetType, "default_config": "enabled",
		}},
		"configs not array": {map[string]any{
			"type": BuiltinToolsetType, "configs": map[string]any{"name": "bash"},
		}},
		"default enabled not boolean": {map[string]any{
			"type":           BuiltinToolsetType,
			"default_config": map[string]any{"enabled": "yes"},
		}},
		"config enabled not boolean": {map[string]any{
			"type":    BuiltinToolsetType,
			"configs": []any{map[string]any{"name": "bash", "enabled": "yes"}},
		}},
		"custom description not string": {map[string]any{
			"type": "custom", "name": "weather", "description": 7,
			"input_schema": map[string]any{"type": "object"},
		}},
		"custom schema not object": {map[string]any{
			"type": "custom", "name": "weather", "description": "d", "input_schema": "object",
		}},
		"custom schema wrong type": {map[string]any{
			"type": "custom", "name": "weather", "description": "d",
			"input_schema": map[string]any{"type": "string"},
		}},
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateToolConfiguration(raw, nil); err == nil {
				t.Fatalf("malformed tool value was accepted: %#v", raw)
			}
		})
	}
}

func TestValidateToolConfiguration_EnforcesCustomToolContract(t *testing.T) {
	valid := map[string]any{
		"type": "custom", "name": "weather-lookup_v2", "description": "Look up weather.",
		"input_schema": map[string]any{"type": "object"},
	}
	if err := ValidateToolConfiguration([]any{valid}, nil); err != nil {
		t.Fatalf("valid custom tool was rejected: %v", err)
	}

	boundary := map[string]any{
		"type": "custom", "name": strings.Repeat("a", 128),
		"description": strings.Repeat("界", 4096),
		"input_schema": map[string]any{
			"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
		},
	}
	if err := ValidateToolConfiguration([]any{boundary}, nil); err != nil {
		t.Fatalf("boundary custom tool was rejected: %v", err)
	}

	cases := map[string]map[string]any{
		"missing name": {
			"type": "custom", "description": "d", "input_schema": map[string]any{"type": "object"},
		},
		"invalid name character": {
			"type": "custom", "name": "weather.lookup", "description": "d",
			"input_schema": map[string]any{"type": "object"},
		},
		"name too long": {
			"type": "custom", "name": strings.Repeat("a", 129), "description": "d",
			"input_schema": map[string]any{"type": "object"},
		},
		"missing description": {
			"type": "custom", "name": "weather", "input_schema": map[string]any{"type": "object"},
		},
		"empty description": {
			"type": "custom", "name": "weather", "description": "",
			"input_schema": map[string]any{"type": "object"},
		},
		"description too long": {
			"type": "custom", "name": "weather", "description": strings.Repeat("d", 4097),
			"input_schema": map[string]any{"type": "object"},
		},
		"missing input schema": {
			"type": "custom", "name": "weather", "description": "d",
		},
		"missing schema type": {
			"type": "custom", "name": "weather", "description": "d",
			"input_schema": map[string]any{"properties": map[string]any{}},
		},
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateToolConfiguration([]any{raw}, nil); err == nil {
				t.Fatalf("invalid custom tool was accepted: %#v", raw)
			}
		})
	}
}

func TestValidateSkillToolConfigurationRequiresReadAndReservesDispatcher(t *testing.T) {
	if err := ValidateSkillToolConfiguration(nil, false); err != nil {
		t.Fatalf("tool-free agent without Skills was rejected: %v", err)
	}
	if err := ValidateSkillToolConfiguration(nil, true); err == nil ||
		!strings.Contains(err.Error(), "require the read tool") {
		t.Fatalf("Skills without read error = %v", err)
	}
	readOnly := []any{map[string]any{
		"type":           BuiltinToolsetType,
		"default_config": map[string]any{"enabled": false},
		"configs":        []any{map[string]any{"name": "read", "enabled": true}},
	}}
	if err := ValidateSkillToolConfiguration(readOnly, true); err != nil {
		t.Fatalf("Skills with read were rejected: %v", err)
	}
	withCollision := append(readOnly, map[string]any{
		"type": "custom", "name": "Skill", "description": "collides",
		"input_schema": map[string]any{"type": "object"},
	})
	if err := ValidateSkillToolConfiguration(withCollision, true); err == nil ||
		!strings.Contains(err.Error(), "runtime Skill dispatcher") {
		t.Fatalf("Skill dispatcher collision error = %v", err)
	}
}

func TestStoredToolConfiguration_ToleratesHistoricalIncompleteCustomTool(t *testing.T) {
	tool := map[string]any{"type": "custom", "name": "legacy_tool"}
	if err := ValidateStoredToolConfiguration([]any{tool}, nil); err != nil {
		t.Fatalf("historical custom tool was rejected: %v", err)
	}
	if err := ValidateToolConfiguration([]any{tool}, nil); err == nil {
		t.Fatal("new admission accepted an incomplete custom tool")
	}
}

func TestValidateToolConfiguration_EnforcesToolCollectionBounds(t *testing.T) {
	tools := make([]any, 128)
	for index := range tools {
		tools[index] = map[string]any{
			"type": "custom", "name": fmt.Sprintf("tool_%d", index), "description": "d",
			"input_schema": map[string]any{"type": "object"},
		}
	}
	if err := ValidateToolConfiguration(tools, nil); err != nil {
		t.Fatalf("128 tool entries were rejected: %v", err)
	}

	overLimit := append(slices.Clone(tools), map[string]any{
		"type": "custom", "name": "tool_128", "description": "d",
		"input_schema": map[string]any{"type": "object"},
	})
	if err := ValidateToolConfiguration(overLimit, nil); err == nil {
		t.Fatal("129 tool entries were accepted")
	}
	if err := ValidateStoredToolConfiguration(overLimit, nil); err != nil {
		t.Fatalf("historical over-limit snapshot was rejected: %v", err)
	}
}

func TestValidateToolConfiguration_EnforcesMCPToolConfigNameBounds(t *testing.T) {
	servers := []any{map[string]any{
		"type": "url", "name": "server", "url": "https://mcp.example.com",
	}}
	toolWithName := func(name string) []any {
		return []any{map[string]any{
			"type": "mcp_toolset", "mcp_server_name": "server",
			"configs": []any{map[string]any{"name": name}},
		}}
	}

	if err := ValidateToolConfiguration(toolWithName(strings.Repeat("界", 128)), servers); err != nil {
		t.Fatalf("128-character MCP tool name was rejected: %v", err)
	}
	longName := strings.Repeat("界", 129)
	if err := ValidateToolConfiguration(toolWithName(longName), servers); err == nil {
		t.Fatal("129-character MCP tool name was accepted")
	}
	if err := ValidateStoredToolConfiguration(toolWithName(longName), servers); err != nil {
		t.Fatalf("historical long MCP tool name was rejected: %v", err)
	}
}

func TestStoredToolConfiguration_ToleratesHistoricalFields(t *testing.T) {
	baseTools := []any{map[string]any{
		"type": "mcp_toolset", "mcp_server_name": "github",
	}}
	baseServers := []any{map[string]any{
		"type": "url", "name": "github", "url": "https://example.com/mcp",
	}}
	cases := map[string]struct {
		tools   []any
		servers []any
	}{
		"tool field": {
			tools: []any{map[string]any{
				"type": "mcp_toolset", "mcp_server_name": "github",
				"permission": map[string]any{"type": "always_ask"},
			}},
			servers: baseServers,
		},
		"server field": {
			tools: baseTools,
			servers: []any{map[string]any{
				"type": "url", "name": "github", "url": "https://example.com/mcp",
				"authorization_token": "historical-value",
			}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateStoredToolConfiguration(tc.tools, tc.servers); err != nil {
				t.Fatalf("historical snapshot was rejected: %v", err)
			}
			if err := ValidateToolConfiguration(tc.tools, tc.servers); err == nil {
				t.Fatal("new admission accepted a historical field")
			}
		})
	}
}

func TestParseTools_Empty(t *testing.T) {
	ts, err := ParseTools(nil)
	if err != nil || ts.Builtin != nil || len(ts.Custom) != 0 {
		t.Fatalf("empty parse = %+v err=%v", ts, err)
	}
}

func TestParseMCPServers_ValidatesIdentityAndURL(t *testing.T) {
	servers, err := ParseMCPServers([]any{map[string]any{
		"type": "url", "name": "github", "url": "https://example.com/mcp",
	}})
	if err != nil || servers["github"].URL != "https://example.com/mcp" {
		t.Fatalf("servers=%#v err=%v", servers, err)
	}
	for _, raw := range [][]any{
		{map[string]any{"type": "url", "name": "x", "url": "file:///tmp/mcp"}},
		{map[string]any{"type": "url", "name": "x", "url": "https://u:p@example.com/mcp"}},
		{map[string]any{"type": "url", "name": "x", "url": "http://127.0.0.1/mcp"}},
		{map[string]any{"type": "url", "name": "x", "url": "http://169.254.169.254/mcp"}},
		{map[string]any{"type": "url", "name": "x", "url": "http://[::1]/mcp"}},
		{
			map[string]any{"type": "url", "name": "x", "url": "https://one.example"},
			map[string]any{"type": "url", "name": "x", "url": "https://two.example"},
		},
	} {
		if _, err := ParseMCPServers(raw); err == nil {
			t.Fatalf("expected invalid MCP servers to fail: %#v", raw)
		}
	}
}

func TestValidateToolConfiguration_RejectsUnsupportedMCPServerFields(t *testing.T) {
	for _, field := range []string{"authorization_token", "headers", "tool_configuration"} {
		t.Run(field, func(t *testing.T) {
			err := validateToolWireShape(nil, []any{map[string]any{
				"type": "url", "name": "github", "url": "https://example.com/mcp",
				field: "secret",
			}})
			if err == nil {
				t.Fatalf("unsupported field %q was accepted", field)
			}
			if !strings.Contains(err.Error(), field) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error should name only the field: %v", err)
			}
		})
	}
}

func TestMCPAddressAllowed(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1",
		"10.0.0.1",
		"100.64.0.1",
		"169.254.169.254",
		"192.168.1.1",
		"::1",
		"fc00::1",
		"fe80::1",
		"2001:db8::1",
	} {
		address := netip.MustParseAddr(raw)
		if MCPAddressAllowed(address) {
			t.Errorf("non-public address %s was allowed", raw)
		}
	}
	for _, raw := range []string{
		"8.8.8.8",
		"1.1.1.1",
		"2606:4700:4700::1111",
	} {
		address := netip.MustParseAddr(raw)
		if !MCPAddressAllowed(address) {
			t.Errorf("public address %s was rejected", raw)
		}
	}
}

func TestValidateToolConfiguration_RequiresMatchingMCPPairs(t *testing.T) {
	server := []any{map[string]any{
		"type": "url", "name": "github", "url": "https://mcp.example.com",
	}}
	toolset := []any{map[string]any{
		"type": "mcp_toolset", "mcp_server_name": "github",
	}}
	if err := ValidateToolConfiguration(toolset, server); err != nil {
		t.Fatalf("valid MCP pair: %v", err)
	}
	if err := ValidateToolConfiguration(nil, server); err == nil {
		t.Fatal("expected unreferenced MCP server to fail")
	}
	if err := ValidateToolConfiguration(toolset, nil); err == nil {
		t.Fatal("expected dangling MCP toolset to fail")
	}
}

func TestValidateToolConfiguration_RejectsApprovalForNativeWeb(t *testing.T) {
	err := ValidateToolConfiguration([]any{map[string]any{
		"type": BuiltinToolsetType,
		"configs": []any{map[string]any{
			"name": "web_search",
			"permission_policy": map[string]any{
				"type": "always_ask",
			},
		}},
	}}, nil)
	if err == nil {
		t.Fatal("expected provider-native web approval policy to fail")
	}
}

func TestParseTools_RejectsDuplicateAndUnknownConfiguration(t *testing.T) {
	cases := [][]any{
		{
			map[string]any{"type": BuiltinToolsetType},
			map[string]any{"type": BuiltinToolsetType},
		},
		{map[string]any{
			"type": BuiltinToolsetType,
			"configs": []any{
				map[string]any{"name": "not_a_builtin"},
			},
		}},
		{map[string]any{
			"type": "mcp_toolset", "mcp_server_name": "github",
			"default_config": map[string]any{
				"permission_policy": map[string]any{"type": "sometimes"},
			},
		}},
		{map[string]any{
			"type": "custom", "name": "bash",
		}},
	}
	for _, raw := range cases {
		if _, err := ParseTools(raw); err == nil {
			t.Fatalf("expected invalid tool configuration to fail: %#v", raw)
		}
	}
}

func TestAutoPermissionConfigurationUsesLocalAndMCPAdmission(t *testing.T) {
	local := map[string]any{"type": "agent_toolset_20260401", "default_config": map[string]any{"permission_policy": map[string]any{"type": "auto"}}, "configs": []any{map[string]any{"name": "web_fetch", "enabled": false}, map[string]any{"name": "web_search", "enabled": false}}}
	if err := ValidateStoredToolConfiguration([]any{local}, nil); err != nil {
		t.Fatalf("local auto: %v", err)
	}
	mcp := map[string]any{"type": "mcp_toolset", "mcp_server_name": "issues", "default_config": map[string]any{"permission_policy": map[string]any{"type": "auto"}}}
	if err := ValidateStoredToolConfiguration([]any{mcp}, []any{map[string]any{"name": "issues", "type": "url", "url": "https://mcp.example.test"}}); err != nil {
		t.Fatalf("MCP auto: %v", err)
	}
	delete(local, "configs")
	if err := ValidateStoredToolConfiguration([]any{local}, nil); err == nil {
		t.Fatal("provider-native Web auto accepted")
	}
}
