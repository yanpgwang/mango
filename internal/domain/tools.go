package domain

import (
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

const BuiltinToolsetType = "agent_toolset_20260401"

const maxAgentToolEntries = 128

var BuiltinToolNames = []string{"bash", "read", "write", "edit", "glob", "grep", "web_fetch", "web_search"}

type PermissionPolicy struct{ Type string }

type BuiltinConfig struct {
	Name    string
	Enabled *bool
	Policy  *PermissionPolicy
}

type BuiltinToolset struct {
	DefaultEnabled bool
	DefaultPolicy  PermissionPolicy
	Configs        []BuiltinConfig
}

type CustomTool struct {
	Name        string
	Description string
	InputSchema map[string]any
}

type MCPToolset struct {
	ServerName     string
	DefaultEnabled bool
	DefaultPolicy  PermissionPolicy
	Configs        []BuiltinConfig
}

type MCPServer struct {
	Name string
	URL  string
}

type ToolSet struct {
	Builtin *BuiltinToolset
	Custom  []CustomTool
	MCP     []MCPToolset
}

var blockedMCPAddressPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // shared carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
}

// MCPAddressAllowed reports whether a resolved MCP endpoint is safe for the
// default public-internet connector. Private MCP services require a separate
// tunnel/egress capability; the worker must never reach them directly from a
// tenant-controlled URL.
func MCPAddressAllowed(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() ||
		!address.IsGlobalUnicast() ||
		address.IsPrivate() ||
		address.IsLoopback() ||
		address.IsLinkLocalUnicast() ||
		address.IsLinkLocalMulticast() ||
		address.IsUnspecified() {
		return false
	}
	for _, prefix := range blockedMCPAddressPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func validPermissionPolicy(policy PermissionPolicy) bool {
	return policy.Type == "always_allow" || policy.Type == "always_ask" || policy.Type == "auto"
}

var (
	builtinToolsetFields = []string{"configs", "default_config", "type"}
	mcpToolsetFields     = []string{"configs", "default_config", "mcp_server_name", "type"}
	customToolFields     = []string{"description", "input_schema", "name", "type"}
	toolConfigFields     = []string{"enabled", "name", "permission_policy"}
	defaultConfigFields  = []string{"enabled", "permission_policy"}
	mcpServerFields      = []string{"name", "type", "url"}
	policyFields         = []string{"type"}
)

// rejectUnknownFields keeps unsupported nested data out of immutable Agent
// versions and Session snapshots. It names the unsupported key, never its
// value, and sorts keys so the error is deterministic.
func rejectUnknownFields(value map[string]any, context string, allowed []string) error {
	var unknown []string
	for key := range value {
		if !slices.Contains(allowed, key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	slices.Sort(unknown)
	return fmt.Errorf("%s does not support field %q", context, unknown[0])
}

func validateOptionalBool(value map[string]any, key, context string) error {
	raw, present := value[key]
	if !present || raw == nil {
		return nil
	}
	if _, ok := raw.(bool); !ok {
		return fmt.Errorf("%s %s must be a boolean", context, key)
	}
	return nil
}

// validateToolWireShape is the strict admission boundary for new public
// requests. Runtime parsing remains tolerant of unknown historical fields so
// persisted snapshots and Temporal replays survive upgrades.
func validateToolWireShape(rawTools, rawServers []any) error {
	if len(rawTools) > maxAgentToolEntries {
		return fmt.Errorf("an agent may configure at most 128 tool entries")
	}
	for _, item := range rawTools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var context string
		switch tool["type"] {
		case BuiltinToolsetType:
			context = "built-in toolset"
			if err := rejectUnknownFields(tool, context, builtinToolsetFields); err != nil {
				return err
			}
		case "mcp_toolset":
			context = "mcp_toolset"
			if err := rejectUnknownFields(tool, context, mcpToolsetFields); err != nil {
				return err
			}
		case "custom":
			if err := rejectUnknownFields(tool, "custom tool", customToolFields); err != nil {
				return err
			}
			name, ok := tool["name"].(string)
			if !ok || !validCustomToolName(name) {
				return fmt.Errorf("custom tool name must match ^[a-zA-Z0-9_-]{1,128}$")
			}
			description, ok := tool["description"].(string)
			if !ok {
				return fmt.Errorf("custom tool description must be a string")
			}
			descriptionLength := utf8.RuneCountInString(description)
			if descriptionLength < 1 || descriptionLength > 4096 {
				return fmt.Errorf("custom tool description must contain 1 to 4096 characters")
			}
			schema, ok := tool["input_schema"].(map[string]any)
			if !ok {
				return fmt.Errorf("custom tool input_schema must be an object")
			}
			if schemaType, present := schema["type"]; !present || schemaType != "object" {
				return fmt.Errorf("custom tool input_schema type must be object")
			}
			continue
		default:
			continue
		}
		if rawConfig, present := tool["default_config"]; present && rawConfig != nil {
			config, ok := rawConfig.(map[string]any)
			if !ok {
				return fmt.Errorf("%s default_config must be an object", context)
			}
			if err := rejectUnknownFields(config, context+" default_config", defaultConfigFields); err != nil {
				return err
			}
			if err := validateOptionalBool(config, "enabled", context+" default_config"); err != nil {
				return err
			}
			if policy, ok := config["permission_policy"].(map[string]any); ok {
				if err := rejectUnknownFields(policy, context+" default permission_policy", policyFields); err != nil {
					return err
				}
			}
		}
		if rawConfigs, present := tool["configs"]; present && rawConfigs != nil {
			configs, ok := rawConfigs.([]any)
			if !ok {
				return fmt.Errorf("%s configs must be an array", context)
			}
			for _, item := range configs {
				config, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if err := rejectUnknownFields(config, context+" config", toolConfigFields); err != nil {
					return err
				}
				if context == "mcp_toolset" {
					name, ok := config["name"].(string)
					if !ok || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 128 {
						return fmt.Errorf("mcp tool config name must contain 1 to 128 characters")
					}
				}
				if err := validateOptionalBool(config, "enabled", context+" config"); err != nil {
					return err
				}
				if policy, ok := config["permission_policy"].(map[string]any); ok {
					if err := rejectUnknownFields(policy, context+" permission_policy", policyFields); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, item := range rawServers {
		server, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if err := rejectUnknownFields(server, "mcp server", mcpServerFields); err != nil {
			return err
		}
	}
	return nil
}

func validCustomToolName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for _, char := range name {
		if (char >= 'a' && char <= 'z') ||
			(char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') ||
			char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func parsePolicy(raw any) *PermissionPolicy {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	t, _ := m["type"].(string)
	if t == "" {
		return nil
	}
	return &PermissionPolicy{Type: t}
}

func ParseTools(raw []any) (ToolSet, error) {
	var ts ToolSet
	customNames := make(map[string]struct{})
	mcpServers := make(map[string]struct{})
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return ToolSet{}, fmt.Errorf("tool entry must be an object")
		}
		switch m["type"] {
		case BuiltinToolsetType:
			if ts.Builtin != nil {
				return ToolSet{}, fmt.Errorf("only one built-in toolset may be configured")
			}
			bt := &BuiltinToolset{DefaultEnabled: true, DefaultPolicy: PermissionPolicy{Type: "always_allow"}}
			if dc, ok := m["default_config"].(map[string]any); ok {
				if en, ok := dc["enabled"].(bool); ok {
					bt.DefaultEnabled = en
				}
				if rawPolicy, present := dc["permission_policy"]; present {
					p := parsePolicy(rawPolicy)
					if p == nil || !validPermissionPolicy(*p) {
						return ToolSet{}, fmt.Errorf("built-in default permission_policy must be always_allow, always_ask, or auto")
					}
					bt.DefaultPolicy = *p
				}
			}
			configuredNames := make(map[string]struct{})
			if cfgs, ok := m["configs"].([]any); ok {
				for _, c := range cfgs {
					cm, ok := c.(map[string]any)
					if !ok {
						return ToolSet{}, fmt.Errorf("toolset config must be an object")
					}
					name, _ := cm["name"].(string)
					if name == "" {
						return ToolSet{}, fmt.Errorf("toolset config requires name")
					}
					if !containsBuiltinTool(name) {
						return ToolSet{}, fmt.Errorf("unknown built-in tool %q", name)
					}
					if _, duplicate := configuredNames[name]; duplicate {
						return ToolSet{}, fmt.Errorf("duplicate built-in tool config %q", name)
					}
					configuredNames[name] = struct{}{}
					bc := BuiltinConfig{Name: name}
					if rawPolicy, present := cm["permission_policy"]; present {
						p := parsePolicy(rawPolicy)
						if p == nil || !validPermissionPolicy(*p) {
							return ToolSet{}, fmt.Errorf("built-in tool %q permission_policy must be always_allow, always_ask, or auto", name)
						}
						bc.Policy = p
					}
					if en, ok := cm["enabled"].(bool); ok {
						bc.Enabled = &en
					}
					bt.Configs = append(bt.Configs, bc)
				}
			}
			ts.Builtin = bt
		case "custom":
			name, _ := m["name"].(string)
			if name == "" {
				return ToolSet{}, fmt.Errorf("custom tool requires name")
			}
			if _, duplicate := customNames[name]; duplicate {
				return ToolSet{}, fmt.Errorf("duplicate custom tool %q", name)
			}
			if containsBuiltinTool(name) {
				return ToolSet{}, fmt.Errorf(
					"custom tool %q conflicts with a built-in tool",
					name,
				)
			}
			customNames[name] = struct{}{}
			ct := CustomTool{Name: name}
			ct.Description, _ = m["description"].(string)
			ct.InputSchema, _ = m["input_schema"].(map[string]any)
			ts.Custom = append(ts.Custom, ct)
		case "mcp_toolset":
			sn, _ := m["mcp_server_name"].(string)
			if sn == "" {
				return ToolSet{}, fmt.Errorf("mcp_toolset requires mcp_server_name")
			}
			if _, duplicate := mcpServers[sn]; duplicate {
				return ToolSet{}, fmt.Errorf("duplicate mcp_toolset for server %q", sn)
			}
			mcpServers[sn] = struct{}{}
			mt := MCPToolset{
				ServerName:     sn,
				DefaultEnabled: true,
				DefaultPolicy: PermissionPolicy{
					Type: "always_ask",
				},
			}
			if dc, ok := m["default_config"].(map[string]any); ok {
				if en, ok := dc["enabled"].(bool); ok {
					mt.DefaultEnabled = en
				}
				if rawPolicy, present := dc["permission_policy"]; present {
					p := parsePolicy(rawPolicy)
					if p == nil || !validPermissionPolicy(*p) {
						return ToolSet{}, fmt.Errorf("mcp server %q default permission_policy must be always_allow, always_ask, or auto", sn)
					}
					mt.DefaultPolicy = *p
				}
			}
			configuredNames := make(map[string]struct{})
			if cfgs, ok := m["configs"].([]any); ok {
				for _, c := range cfgs {
					cm, ok := c.(map[string]any)
					if !ok {
						return ToolSet{}, fmt.Errorf("mcp toolset config must be an object")
					}
					name, _ := cm["name"].(string)
					if name == "" {
						return ToolSet{}, fmt.Errorf("mcp toolset config requires name")
					}
					if _, duplicate := configuredNames[name]; duplicate {
						return ToolSet{}, fmt.Errorf("duplicate mcp tool config %q for server %q", name, sn)
					}
					configuredNames[name] = struct{}{}
					cfg := BuiltinConfig{Name: name}
					if rawPolicy, present := cm["permission_policy"]; present {
						p := parsePolicy(rawPolicy)
						if p == nil || !validPermissionPolicy(*p) {
							return ToolSet{}, fmt.Errorf("mcp tool %s/%s permission_policy must be always_allow, always_ask, or auto", sn, name)
						}
						cfg.Policy = p
					}
					if en, ok := cm["enabled"].(bool); ok {
						cfg.Enabled = &en
					}
					mt.Configs = append(mt.Configs, cfg)
				}
			}
			ts.MCP = append(ts.MCP, mt)
		default:
			return ToolSet{}, fmt.Errorf("unknown tool type %v", m["type"])
		}
	}
	return ts, nil
}

func containsBuiltinTool(name string) bool {
	for _, candidate := range BuiltinToolNames {
		if candidate == name {
			return true
		}
	}
	return false
}

func (ts MCPToolset) ToolEnabled(name string) (bool, PermissionPolicy) {
	enabled := ts.DefaultEnabled
	policy := ts.DefaultPolicy
	for _, config := range ts.Configs {
		if config.Name != name {
			continue
		}
		if config.Enabled != nil {
			enabled = *config.Enabled
		}
		if config.Policy != nil {
			policy = *config.Policy
		}
	}
	return enabled, policy
}

func ParseMCPServers(raw []any) (map[string]MCPServer, error) {
	if len(raw) > 20 {
		return nil, fmt.Errorf("an agent may configure at most 20 MCP servers")
	}
	servers := make(map[string]MCPServer, len(raw))
	for _, item := range raw {
		value, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mcp server entry must be an object")
		}
		serverType, _ := value["type"].(string)
		name, _ := value["name"].(string)
		rawURL, _ := value["url"].(string)
		if serverType != "url" ||
			strings.TrimSpace(name) == "" ||
			strings.TrimSpace(rawURL) == "" {
			return nil, fmt.Errorf("mcp server requires type=url, name, and url")
		}
		if utf8.RuneCountInString(name) > 255 {
			return nil, fmt.Errorf("mcp server name exceeds 255 characters")
		}
		if utf8.RuneCountInString(rawURL) > 2048 {
			return nil, fmt.Errorf("mcp server %q url exceeds 2048 characters", name)
		}
		if _, duplicate := servers[name]; duplicate {
			return nil, fmt.Errorf("duplicate mcp server name %q", name)
		}
		parsed, err := url.Parse(rawURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
			parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			return nil, fmt.Errorf("mcp server %q has an invalid http(s) url", name)
		}
		if address, err := netip.ParseAddr(parsed.Hostname()); err == nil &&
			!MCPAddressAllowed(address) {
			return nil, fmt.Errorf(
				"mcp server %q url targets a non-public network",
				name,
			)
		}
		servers[name] = MCPServer{Name: name, URL: parsed.String()}
	}
	return servers, nil
}

// ValidateToolConfiguration performs control-plane validation that does not
// require contacting a provider or MCP server. It keeps malformed or
// internally inconsistent capabilities out of immutable Agent versions and
// Session snapshots.
func ValidateToolConfiguration(rawTools, rawServers []any) error {
	if err := validateToolWireShape(rawTools, rawServers); err != nil {
		return err
	}
	return ValidateStoredToolConfiguration(rawTools, rawServers)
}

// ValidateSkillToolConfiguration checks the agent-runtime capabilities Skills
// depend on. Skill is a private dispatcher, but its instructions may reference
// supporting files and scripts, so the public read tool must remain enabled.
// A user-defined tool named Skill would collide with the runtime dispatcher in
// the provider request and is therefore reserved only while Skills are active.
func ValidateSkillToolConfiguration(rawTools []any, hasSkills bool) error {
	if !hasSkills {
		return nil
	}
	toolSet, err := ParseTools(rawTools)
	if err != nil {
		return err
	}
	if enabled, _ := toolSet.BuiltinEnabled("read"); !enabled {
		return fmt.Errorf("skills require the read tool")
	}
	for _, custom := range toolSet.Custom {
		if custom.Name == "Skill" {
			return fmt.Errorf(
				"custom tool %q conflicts with the runtime Skill dispatcher",
				custom.Name,
			)
		}
	}
	return nil
}

// ValidateStoredToolConfiguration checks the executable semantics of an
// already-persisted Agent snapshot while tolerating unknown historical fields.
// Admission must use ValidateToolConfiguration instead.
func ValidateStoredToolConfiguration(rawTools, rawServers []any) error {
	toolSet, err := ParseTools(rawTools)
	if err != nil {
		return err
	}
	servers, err := ParseMCPServers(rawServers)
	if err != nil {
		return err
	}
	referenced := make(map[string]struct{}, len(toolSet.MCP))
	for _, configured := range toolSet.MCP {
		if _, ok := servers[configured.ServerName]; !ok {
			return fmt.Errorf(
				"mcp_toolset references unknown server %q",
				configured.ServerName,
			)
		}
		referenced[configured.ServerName] = struct{}{}
	}
	for name := range servers {
		if _, ok := referenced[name]; !ok {
			return fmt.Errorf("MCP server %q has no matching mcp_toolset", name)
		}
	}
	for _, name := range []string{"web_search", "web_fetch"} {
		enabled, policy := toolSet.BuiltinEnabled(name)
		if enabled && policy.Type != "always_allow" {
			return fmt.Errorf(
				"%s requires always_allow while it is provider-native",
				name,
			)
		}
	}
	return nil
}

func (ts ToolSet) BuiltinEnabled(name string) (bool, PermissionPolicy) {
	if ts.Builtin == nil {
		return false, PermissionPolicy{}
	}
	enabled := ts.Builtin.DefaultEnabled
	policy := ts.Builtin.DefaultPolicy
	for _, c := range ts.Builtin.Configs {
		if c.Name != name {
			continue
		}
		if c.Enabled != nil {
			enabled = *c.Enabled
		}
		if c.Policy != nil {
			policy = *c.Policy
		}
	}
	return enabled, policy
}
