// Command sdk-contract exports Mango's OpenAPI document for SDK generators.
// It is intentionally offline: the checked-in server contract is the input.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type operation struct {
	ID                  string           `json:"id"`
	Method              string           `json:"method"`
	Path                string           `json:"path"`
	Tag                 string           `json:"tag"`
	Parameters          []map[string]any `json:"parameters"`
	RequestContentType  string           `json:"request_content_type"`
	RequestSchema       any              `json:"request_schema"`
	RequestRequired     bool             `json:"request_required"`
	ResponseContentType string           `json:"response_content_type"`
	ResponseSchema      any              `json:"response_schema"`
	ResponseStatus      int              `json:"response_status"`
	Public              bool             `json:"public"`
}

func main() {
	check := flag.Bool("check", false, "fail if the committed SDK contract is stale")
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	if err := run(*root, *check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(root string, check bool) error {
	const source = "internal/httpapi/openapi.yaml"
	data, err := os.ReadFile(filepath.Join(root, source))
	if err != nil {
		return err
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	ops, err := operations(document)
	if err != nil {
		return err
	}
	manifest := struct {
		SchemaVersion int         `json:"schema_version"`
		Source        string      `json:"source"`
		Operations    []operation `json:"operations"`
	}{1, source, ops}
	for _, output := range []struct {
		name  string
		value any
	}{{"openapi.json", document}, {"operations.json", manifest}} {
		encoded, err := json.MarshalIndent(output.value, "", "  ")
		if err != nil {
			return fmt.Errorf("encode %s: %w", output.name, err)
		}
		encoded = append(encoded, '\n')
		path := filepath.Join(root, "sdk", output.name)
		if check {
			existing, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(existing, encoded) {
				return fmt.Errorf("%s is stale; run make sdk-generate", path)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("SDK contract: %d operations (%s)\n", len(ops), map[bool]string{true: "checked", false: "exported"}[check])
	return nil
}

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func resolve(document map[string]any, value any) (map[string]any, error) {
	current := object(value)
	seen := map[string]bool{}
	for {
		ref, _ := current["$ref"].(string)
		if ref == "" {
			if current == nil {
				return nil, fmt.Errorf("expected OpenAPI object, got %T", value)
			}
			return current, nil
		}
		if !strings.HasPrefix(ref, "#/") || seen[ref] {
			return nil, fmt.Errorf("unsupported or cyclic reference %q", ref)
		}
		seen[ref] = true
		var next any = document
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			next = object(next)[part]
		}
		current = object(next)
	}
}

func content(value any) (string, any, error) {
	media := object(value)
	if len(media) == 0 {
		return "", nil, nil
	}
	if len(media) != 1 {
		return "", nil, fmt.Errorf("multiple media types require an explicit SDK selection policy")
	}
	for kind, item := range media {
		return kind, object(item)["schema"], nil
	}
	panic("unreachable")
}

func operations(document map[string]any) ([]operation, error) {
	var result []operation
	seen := map[string]bool{}
	for path, pathValue := range object(document["paths"]) {
		item := object(pathValue)
		for _, method := range []string{"get", "post", "put", "patch", "delete", "head", "options"} {
			definition := object(item[method])
			if definition == nil {
				continue
			}
			id, _ := definition["operationId"].(string)
			if id == "" || seen[id] {
				return nil, fmt.Errorf("missing or duplicate operationId on %s %s", method, path)
			}
			seen[id] = true
			op := operation{ID: id, Method: strings.ToUpper(method), Path: path, Tag: "System", Parameters: []map[string]any{}}
			if tags, ok := definition["tags"].([]any); ok && len(tags) > 0 {
				op.Tag, _ = tags[0].(string)
			}
			if security, ok := definition["security"].([]any); ok && len(security) == 0 {
				op.Public = true
			}
			for _, owner := range []map[string]any{item, definition} {
				parameters, _ := owner["parameters"].([]any)
				for _, raw := range parameters {
					parameter, err := resolve(document, raw)
					if err != nil {
						return nil, fmt.Errorf("%s parameter: %w", id, err)
					}
					replaced := false
					for i, existing := range op.Parameters {
						if existing["name"] == parameter["name"] && existing["in"] == parameter["in"] {
							op.Parameters[i], replaced = parameter, true
							break
						}
					}
					if !replaced {
						op.Parameters = append(op.Parameters, parameter)
					}
				}
			}
			if raw, ok := definition["requestBody"]; ok {
				body, err := resolve(document, raw)
				if err != nil {
					return nil, err
				}
				op.RequestRequired, _ = body["required"].(bool)
				op.RequestContentType, op.RequestSchema, err = content(body["content"])
				if err != nil {
					return nil, fmt.Errorf("%s request: %w", id, err)
				}
			}
			for status, raw := range object(definition["responses"]) {
				code, err := strconv.Atoi(status)
				if err != nil || code < 200 || code >= 300 {
					continue
				}
				if op.ResponseStatus != 0 {
					return nil, fmt.Errorf("%s has multiple success responses; extend SDK manifest explicitly", id)
				}
				response, err := resolve(document, raw)
				if err != nil {
					return nil, err
				}
				op.ResponseStatus = code
				op.ResponseContentType, op.ResponseSchema, err = content(response["content"])
				if err != nil {
					return nil, fmt.Errorf("%s response: %w", id, err)
				}
			}
			if op.ResponseStatus == 0 {
				return nil, fmt.Errorf("%s has no explicit successful response", id)
			}
			result = append(result, op)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
