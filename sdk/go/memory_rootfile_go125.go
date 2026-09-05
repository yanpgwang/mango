//go:build go1.25

package mango

import "os"

// replaceRootFile publishes the already-written sibling atomically while
// retaining os.Root's path confinement. Root.Rename is available in Go 1.25+.
func replaceRootFile(root *os.Root, temporary, destination string) error {
	return root.Rename(temporary, destination)
}
