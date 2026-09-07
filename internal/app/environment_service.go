package app

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/yanpgwang/mango/internal/domain"
)

type EnvironmentService struct {
	env          EnvironmentRepository
	ids          domain.IDGenerator
	clock        domain.Clock
	capabilities EnvironmentCapabilities
}

// EnvironmentCapabilities describe policies the configured execution runtime
// can enforce. The zero value fails closed for optional behavior.
type EnvironmentCapabilities struct {
	PackageSetup   bool
	LimitedNetwork bool
}

func NewEnvironmentService(
	env EnvironmentRepository,
	ids domain.IDGenerator,
	clock domain.Clock,
	capabilities ...EnvironmentCapabilities,
) *EnvironmentService {
	service := &EnvironmentService{env: env, ids: ids, clock: clock}
	if len(capabilities) > 0 {
		service.capabilities = capabilities[0]
	}
	return service
}

func (s *EnvironmentService) Create(ctx context.Context, e domain.Environment) (domain.Environment, error) {
	if e.ConfigType == "" {
		if configType, ok := e.Config["type"].(string); ok {
			e.ConfigType = configType
		} else {
			e.ConfigType = "self_hosted"
		}
	}
	if err := validateEnvironment(e); err != nil {
		return domain.Environment{}, err
	}
	if err := validateEnvironmentRuntimeConfig(e.Config, e.ConfigType, s.capabilities); err != nil {
		return domain.Environment{}, err
	}
	// Keep the validated configuration so Sessions can snapshot and enforce it.
	// Unsupported policies are still rejected by validation before this point.
	e.Config = normalizeEnvironmentConfig(e.Config, e.ConfigType)
	if e.Metadata == nil {
		e.Metadata = map[string]any{}
	}
	now := s.clock.Now().UTC()
	e.ID = s.ids.NewID(domain.PrefixEnv)
	e.CreatedAt = now
	e.UpdatedAt = now
	return e, s.env.Put(ctx, e)
}

func (s *EnvironmentService) Update(
	ctx context.Context,
	id string,
	patch domain.EnvironmentPatch,
) (domain.Environment, error) {
	current, err := s.env.Get(ctx, id)
	if err != nil {
		return domain.Environment{}, err
	}
	if current.ArchivedAt != nil {
		return domain.Environment{}, domain.Validation("archived environment is read-only")
	}
	if patch.Scope != nil && *patch.Scope == "" {
		return domain.Environment{}, domain.Validation("scope must be organization or account")
	}

	if patch.Config != nil {
		rawConfig := cloneEnvironmentConfigValue(*patch.Config).(map[string]any)
		configType, ok := rawConfig["type"].(string)
		if !ok {
			return domain.Environment{}, domain.Validation("config type must be cloud or self_hosted")
		}
		if value, present := rawConfig["networking"]; present && value == nil {
			return domain.Environment{}, domain.Validation("environment networking configuration cannot be null")
		}
		if value, present := rawConfig["packages"]; present && value == nil {
			return domain.Environment{}, domain.Validation("environment package configuration cannot be null")
		}
		// Cloud config is a nested patch in the public contract: omitting
		// networking or packages on update preserves that field. Changing the
		// Environment type starts from the new type's defaults instead.
		if configType == "cloud" && current.ConfigType == "cloud" {
			for _, field := range []string{"networking", "packages"} {
				if _, present := rawConfig[field]; present {
					continue
				}
				if existing, present := current.Config[field]; present {
					rawConfig[field] = cloneEnvironmentConfigValue(existing)
				}
			}
			mergeLimitedNetworkUpdate(rawConfig, current.Config)
		}
		if err := validateEnvironmentConfig(rawConfig, configType); err != nil {
			return domain.Environment{}, err
		}
		if err := validateEnvironmentRuntimeConfig(rawConfig, configType, s.capabilities); err != nil {
			return domain.Environment{}, err
		}
		normalized := normalizeEnvironmentConfig(rawConfig, configType)
		patch.Config = &normalized
		if configType == "cloud" && patch.Scope == nil {
			clearedScope := ""
			patch.Scope = &clearedScope
		}
	}

	next, changed := current.Apply(patch)
	if patch.Config != nil {
		next.ConfigType = (*patch.Config)["type"].(string)
	}
	if err := validateEnvironment(next); err != nil {
		return domain.Environment{}, err
	}
	if !changed {
		return current, nil
	}
	next.UpdatedAt = s.clock.Now().UTC()
	return s.env.Update(ctx, next)
}

func validateEnvironmentRuntimeConfig(
	config map[string]any,
	configType string,
	capabilities EnvironmentCapabilities,
) error {
	if configType != "cloud" {
		return nil
	}
	networking, _ := config["networking"].(map[string]any)
	if networking["type"] == "limited" && !capabilities.LimitedNetwork {
		return domain.Unsupported("limited environment networking is unavailable for the configured sandbox provider")
	}
	if capabilities.PackageSetup {
		return nil
	}
	packages, _ := config["packages"].(map[string]any)
	for _, manager := range []string{"apt", "cargo", "gem", "go", "npm", "pip"} {
		switch values := packages[manager].(type) {
		case []string:
			if len(values) > 0 {
				return domain.Unsupported("environment package installation is unavailable for the configured sandbox provider")
			}
		case []any:
			if len(values) > 0 {
				return domain.Unsupported("environment package installation is unavailable for the configured sandbox provider")
			}
		}
	}
	return nil
}

func mergeLimitedNetworkUpdate(update, current map[string]any) {
	updateNetworking, _ := update["networking"].(map[string]any)
	currentNetworking, _ := current["networking"].(map[string]any)
	if updateNetworking["type"] != "limited" || currentNetworking["type"] != "limited" {
		return
	}
	for _, field := range []string{
		"allow_mcp_servers", "allow_package_managers", "allowed_hosts",
	} {
		if _, present := updateNetworking[field]; present {
			continue
		}
		if value, present := currentNetworking[field]; present {
			updateNetworking[field] = cloneEnvironmentConfigValue(value)
		}
	}
}

func validateEnvironment(environment domain.Environment) error {
	if environment.Name == "" {
		return domain.Validation("name is required")
	}
	if environment.ConfigType != "cloud" && environment.ConfigType != "self_hosted" {
		return domain.Validation("config type must be cloud or self_hosted")
	}
	if environment.Scope != "" && environment.Scope != "organization" && environment.Scope != "account" {
		return domain.Validation("scope must be organization or account")
	}
	if environment.ConfigType == "cloud" && environment.Scope != "" {
		return domain.Validation("scope is only supported for self_hosted environments")
	}
	if err := validateMetadata(environment.Metadata); err != nil {
		return err
	}
	return validateEnvironmentConfig(environment.Config, environment.ConfigType)
}

func validateEnvironmentConfig(config map[string]any, configType string) error {
	if value, present := config["type"]; present {
		valueType, ok := value.(string)
		if !ok || valueType != configType {
			return domain.Validation("config type must be cloud or self_hosted")
		}
	}
	allowed := map[string]struct{}{"type": {}}
	if configType == "cloud" {
		allowed["networking"] = struct{}{}
		allowed["packages"] = struct{}{}
	}
	if err := rejectUnknownEnvironmentFields(config, allowed, "config"); err != nil {
		return err
	}
	if value, present := config["networking"]; present {
		if err := validateEnvironmentNetworking(value); err != nil {
			return err
		}
	}
	if value, present := config["packages"]; present {
		if err := validateEnvironmentPackages(value); err != nil {
			return err
		}
	}
	return nil
}

func validateEnvironmentNetworking(value any) error {
	networking, ok := value.(map[string]any)
	if !ok || networking == nil {
		return domain.Validation("environment networking configuration must be an object")
	}
	typeValue, ok := networking["type"].(string)
	if !ok {
		return domain.Validation("environment networking type must be unrestricted or limited")
	}
	if typeValue == "limited" {
		allowed := map[string]struct{}{
			"type": {}, "allow_mcp_servers": {}, "allow_package_managers": {}, "allowed_hosts": {},
		}
		if err := rejectUnknownEnvironmentFields(networking, allowed, "networking"); err != nil {
			return err
		}
		for _, field := range []string{"allow_mcp_servers", "allow_package_managers"} {
			if fieldValue, present := networking[field]; present {
				if _, ok := fieldValue.(bool); !ok {
					return domain.Validation(
						fmt.Sprintf("environment networking.%s must be a boolean", field),
					)
				}
			}
		}
		if hosts, present := networking["allowed_hosts"]; present {
			if err := validateEnvironmentStringList(hosts, "networking.allowed_hosts"); err != nil {
				return err
			}
			if err := validateEnvironmentHosts(hosts); err != nil {
				return err
			}
		}
		return nil
	}
	if typeValue != "unrestricted" {
		return domain.Validation("environment networking type must be unrestricted or limited")
	}
	return rejectUnknownEnvironmentFields(
		networking,
		map[string]struct{}{"type": {}},
		"networking",
	)
}

func validateEnvironmentHosts(value any) error {
	var hosts []string
	switch values := value.(type) {
	case []string:
		hosts = values
	case []any:
		hosts = make([]string, len(values))
		for index, entry := range values {
			hosts[index], _ = entry.(string)
		}
	}
	for _, host := range hosts {
		if !validEnvironmentHost(host) {
			return domain.Validation(
				"environment networking.allowed_hosts must contain bare hostnames or *. wildcards",
			)
		}
	}
	return nil
}

func validEnvironmentHost(host string) bool {
	if host == "" || strings.TrimSpace(host) != host || len(host) > 253 ||
		strings.ContainsAny(host, "/:#?@[]") {
		return false
	}
	if address := net.ParseIP(host); address != nil {
		return address.To4() != nil
	}
	host = strings.TrimPrefix(host, "*.")
	if host == "" || strings.Contains(host, "*") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for index := 0; index < len(label); index++ {
			character := label[index]
			if (character >= 'a' && character <= 'z') ||
				(character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9') || character == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func validateEnvironmentPackages(value any) error {
	packages, ok := value.(map[string]any)
	if !ok || packages == nil {
		return domain.Validation("environment package configuration must be an object")
	}
	allowed := map[string]struct{}{
		"type": {}, "apt": {}, "cargo": {}, "gem": {}, "go": {}, "npm": {}, "pip": {},
	}
	if err := rejectUnknownEnvironmentFields(packages, allowed, "packages"); err != nil {
		return err
	}
	if value, present := packages["type"]; present {
		packageType, ok := value.(string)
		if !ok || packageType != "packages" {
			return domain.Validation("environment package configuration type must be packages")
		}
	}
	for _, manager := range []string{"apt", "cargo", "gem", "go", "npm", "pip"} {
		value, present := packages[manager]
		if !present {
			continue
		}
		if err := validateEnvironmentStringList(value, "packages."+manager); err != nil {
			return err
		}
		if err := validateEnvironmentPackageNames(value, "packages."+manager); err != nil {
			return err
		}
	}
	return nil
}

func validateEnvironmentPackageNames(value any, field string) error {
	var names []string
	switch values := value.(type) {
	case []string:
		names = values
	case []any:
		names = make([]string, len(values))
		for index, entry := range values {
			names[index], _ = entry.(string)
		}
	}
	for _, name := range names {
		if strings.TrimSpace(name) == "" || strings.HasPrefix(name, "-") ||
			strings.ContainsAny(name, "\r\n\x00") {
			return domain.Validation(
				fmt.Sprintf("environment %s contains an invalid package name", field),
			)
		}
	}
	return nil
}

func normalizeEnvironmentConfig(config map[string]any, configType string) map[string]any {
	normalized := map[string]any{"type": configType}
	if configType != "cloud" {
		return normalized
	}
	if networking, present := config["networking"]; present {
		normalized["networking"] = normalizeEnvironmentNetworking(networking)
	}
	if packages, present := config["packages"]; present {
		normalized["packages"] = cloneEnvironmentConfigValue(packages)
	}
	return normalized
}

func normalizeEnvironmentNetworking(value any) map[string]any {
	networking, _ := value.(map[string]any)
	if networking["type"] != "limited" {
		return map[string]any{"type": "unrestricted"}
	}
	normalized := map[string]any{
		"type":                   "limited",
		"allow_mcp_servers":      false,
		"allow_package_managers": false,
		"allowed_hosts":          []any{},
	}
	for _, field := range []string{
		"allow_mcp_servers", "allow_package_managers", "allowed_hosts",
	} {
		if configured, present := networking[field]; present {
			normalized[field] = cloneEnvironmentConfigValue(configured)
		}
	}
	return normalized
}

func cloneEnvironmentConfigValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, nested := range typed {
			cloned[key] = cloneEnvironmentConfigValue(nested)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for index, nested := range typed {
			cloned[index] = cloneEnvironmentConfigValue(nested)
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

func validateEnvironmentStringList(value any, field string) error {
	switch values := value.(type) {
	case []string:
		return nil
	case []any:
		for _, entry := range values {
			if _, ok := entry.(string); !ok {
				return domain.Validation(
					fmt.Sprintf("environment %s values must be strings", field),
				)
			}
		}
		return nil
	default:
		return domain.Validation(
			fmt.Sprintf("environment %s must be an array", field),
		)
	}
}

func rejectUnknownEnvironmentFields(
	values map[string]any,
	allowed map[string]struct{},
	object string,
) error {
	unknown := make([]string, 0)
	for key := range values {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return domain.Validation(fmt.Sprintf("unknown environment %s field %q", object, unknown[0]))
}

func (s *EnvironmentService) Get(ctx context.Context, id string) (domain.Environment, error) {
	return s.env.Get(ctx, id)
}

func (s *EnvironmentService) List(
	ctx context.Context,
	query EnvironmentListQuery,
) (EnvironmentListPage, error) {
	if query.Limit <= 0 {
		query.Limit = DefaultEnvironmentListLimit
	}
	return s.env.List(ctx, query)
}

func (s *EnvironmentService) Archive(ctx context.Context, id string) (domain.Environment, error) {
	return s.env.Archive(ctx, id, s.clock.Now().UTC())
}

func (s *EnvironmentService) Delete(ctx context.Context, id string) error {
	return s.env.DeleteIfUnreferenced(ctx, id)
}
