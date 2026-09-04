// Package tools provides the built-in tool executors that an agent runtime
// runs inside a sandbox, plus the model-facing input schemas that describe
// them to the model.
//
// A tool execution error (bad input, missing file, unimplemented tool) is
// reported to the model via Result.IsError so the model can recover; it is not
// a session-level error.
package tools

import (
	"context"
	"sort"

	"github.com/yanpgwang/mango/internal/sandbox"
)

// Result is the outcome of a tool execution. Content is the wire block array
// (a list of {type:"text", text:string} maps) fed back to the model.
type Result struct {
	Content []any
	IsError bool
}

// Executor runs a single tool invocation inside the given sandbox.
type Executor func(ctx context.Context, sb sandbox.Sandbox, input map[string]any) Result

// textResult builds a Result carrying a single text block.
func textResult(s string, isErr bool) Result {
	return Result{
		Content: []any{map[string]any{"type": "text", "text": s}},
		IsError: isErr,
	}
}

// notImplemented is the defensive local fallback for tools whose execution is
// owned by another integration boundary. It always reports an error to the
// model if routing reaches this registry unexpectedly.
func notImplemented(name string) Executor {
	return func(_ context.Context, _ sandbox.Sandbox, _ map[string]any) Result {
		return textResult(name+": not implemented", true)
	}
}

// Registry returns the tool name to executor mapping. bash/read/write/edit and
// glob/grep execute locally. web_fetch/web_search normally route through the
// provider-native server-tool path; their entries here fail closed if that
// routing invariant is violated.
func Registry() map[string]Executor {
	return map[string]Executor{
		"bash":       execBash,
		"read":       execRead,
		"write":      execWrite,
		"edit":       execEdit,
		"glob":       execGlob,
		"grep":       execGrep,
		"web_fetch":  notImplemented("web_fetch"),
		"web_search": notImplemented("web_search"),
	}
}

// Names returns the sorted list of registered tool names.
func Names() []string {
	reg := Registry()
	names := make([]string, 0, len(reg))
	for n := range reg {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Schema returns the model-facing JSON input_schema for the named tool, or nil
// if the tool is unknown. Shapes adapt useful public tool conventions to the
// semantics Mango actually implements.
//
// web_fetch/web_search are declared to the model through their native routing
// path, but still need legal local schema objects for shared configuration and
// fallback validation. The real Anthropic API rejects "input_schema":null
// (400). They
// therefore get a minimal permissive {"type":"object"} schema. Only a genuinely
// unknown tool name (not one of the eight built-ins) returns nil.
func Schema(name string) map[string]any {
	switch name {
	case "bash":
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The shell command to run.",
				},
			},
			"required": []any{"command"},
		}
	case "read":
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path of the file to read, relative to the sandbox root.",
				},
				"view_range": map[string]any{
					"type":        "array",
					"description": "Optional [start, end] 1-based inclusive line range to view; a non-positive end reads through EOF.",
					"items":       map[string]any{"type": "integer"},
					"minItems":    2,
					"maxItems":    2,
				},
			},
			"required": []any{"path"},
		}
	case "write":
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path of the file to write, relative to the sandbox root.",
				},
				"file_text": map[string]any{
					"type":        "string",
					"description": "The full contents to write to the file.",
				},
			},
			"required": []any{"path", "file_text"},
		}
	case "edit":
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path of the file to edit, relative to the sandbox root.",
				},
				"old_str": map[string]any{
					"type":        "string",
					"description": "The exact, unique string to replace.",
				},
				"new_str": map[string]any{
					"type":        "string",
					"description": "The replacement string.",
				},
			},
			"required": []any{"path", "old_str", "new_str"},
		}
	case "glob":
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Glob pattern to match paths against (`*` and `?` within a path segment, `**` across segments).",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Optional root directory to search under, relative to the sandbox root. Defaults to the whole sandbox.",
				},
			},
			"required": []any{"pattern"},
		}
	case "grep":
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Extended regular expression to search file contents for.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Optional root directory to search under, relative to the sandbox root. Defaults to the whole sandbox.",
				},
			},
			"required": []any{"pattern"},
		}
	case "web_fetch", "web_search":
		// Declared to the model with a minimal legal schema; the executor is
		// still notImplemented (execution returns an is_error result). A
		// permissive object schema keeps the declaration valid so the real API
		// does not reject the request with input_schema:null.
		return map[string]any{"type": "object"}
	default:
		return nil
	}
}

// SelfHostedSchema returns the model-facing contract implemented by an
// Environment worker. Only the worker-owned bash tool has additional lifecycle
// controls; the transitional Mango-managed executor remains a one-shot process
// and continues to use Schema.
func SelfHostedSchema(name string) map[string]any {
	if name != "bash" {
		return Schema(name)
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Shell command to run. State such as the working directory and environment variables persists across calls.",
			},
			"restart": map[string]any{
				"type":        "boolean",
				"description": "Restart the persistent shell before running command. When command is omitted, only restart the shell.",
			},
			"timeout_ms": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"description": "Shell-call timeout in milliseconds. Zero or omission uses the worker default; the runner-wide tool deadline remains an upper bound.",
			},
		},
	}
}
