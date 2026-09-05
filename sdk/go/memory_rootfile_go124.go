//go:build !go1.25

package mango

import (
	"errors"
	"io"
	"os"
)

// replaceRootFile is the Go 1.24 fallback for the Root.Rename method added in
// Go 1.25. Worker reconciliation is serial, so publish through confined Root
// operations and remove any partial destination on failure. The server copy
// remains authoritative if the process exits between removal and completion.
func replaceRootFile(root *os.Root, temporary, destination string) error {
	source, err := root.Open(temporary)
	if err != nil {
		return err
	}
	info, err := source.Stat()
	if err != nil {
		_ = source.Close()
		return err
	}
	if current, statErr := root.Lstat(destination); statErr == nil {
		if !current.Mode().IsRegular() {
			_ = source.Close()
			return errors.New("Memory destination is not a regular file")
		}
		if err := root.Remove(destination); err != nil {
			_ = source.Close()
			return err
		}
	} else if !os.IsNotExist(statErr) {
		_ = source.Close()
		return statErr
	}
	destinationFile, err := root.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		_ = source.Close()
		return err
	}
	_, copyErr := io.Copy(destinationFile, source)
	if copyErr == nil {
		copyErr = destinationFile.Sync()
	}
	closeDestinationErr := destinationFile.Close()
	closeSourceErr := source.Close()
	result := errors.Join(copyErr, closeDestinationErr, closeSourceErr)
	if result != nil {
		_ = root.Remove(destination)
		return result
	}
	return root.Remove(temporary)
}
