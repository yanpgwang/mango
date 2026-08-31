package sandbox

import (
	"errors"
	"os"
	"path/filepath"
)

// canonicalHostPath resolves symlinks through the deepest existing ancestor.
// Docker bind ownership checks must work even when an operator removed a mount
// source or its parent directory; this helper never creates the missing path.
func canonicalHostPath(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	current := absolute
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}
