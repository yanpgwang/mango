// Package buildinfo identifies a runtime binary without connecting to services.
package buildinfo

import (
	"encoding/json"
	"io"
)

// Version and Revision are injected by release builds with linker -X flags.
var (
	Version  = "dev"
	Revision = "unknown"
)

// Write emits the binary's identity as one JSON object.
func Write(writer io.Writer) error {
	return json.NewEncoder(writer).Encode(struct {
		Version  string `json:"version"`
		Revision string `json:"revision"`
	}{Version: Version, Revision: Revision})
}
