package mango

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const sessionToolOutputDirectory = ".mango-tool-results"
const maxSessionToolOutputBytes = 32 << 20

type sessionToolOutputs struct {
	root *os.Root
	done map[string]bool
}

func newSessionToolOutputs(workdir string) (*sessionToolOutputs, error) {
	absolute, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	work, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, err
	}
	defer work.Close()
	if err := work.Mkdir(sessionToolOutputDirectory, 0o755); err != nil && !os.IsExist(err) {
		return nil, err
	}
	info, err := work.Lstat(sessionToolOutputDirectory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("tool output directory must be a real directory")
	}
	root, err := work.OpenRoot(sessionToolOutputDirectory)
	if err != nil {
		return nil, err
	}
	return &sessionToolOutputs{root: root, done: map[string]bool{}}, nil
}

func validToolOutputID(id string) bool {
	if len(id) < 6 || len(id) > 128 || id[:5] != "file_" {
		return false
	}
	for _, c := range id[5:] {
		valid := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if !valid {
			return false
		}
	}
	return true
}

// materialize accepts only server-assigned File IDs. File metadata, size and
// checksum are checked before an atomic confined write. A failed download never
// permits subsequent local tools to execute against a partial result.
func (s *sessionToolOutputs) materialize(ctx context.Context, client *Client, id string) error {
	if !validToolOutputID(id) {
		return errors.New("invalid MCP output File ID")
	}
	if s.done[id] {
		return nil
	}
	file, err := client.Files.Get(ctx, id)
	if err != nil {
		return err
	}
	checksum, ok := file.ChecksumSHA256.Get()
	if file.ID != id || file.MimeType != "text/plain" || file.SizeBytes < 0 || file.SizeBytes > maxSessionToolOutputBytes || !ok || !validSessionSkillChecksum(checksum) {
		return errors.New("invalid MCP output File metadata")
	}
	destination := id + ".txt"
	if info, err := s.root.Lstat(destination); err == nil && !info.Mode().IsRegular() {
		return errors.New("MCP output destination is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	download, err := client.Files.Download(ctx, id)
	if err != nil {
		return err
	}
	defer download.Close()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := ".download-" + hex.EncodeToString(nonce[:])
	output, err := s.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer s.root.Remove(temporary)
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(download, file.SizeBytes+1))
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if written != file.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != checksum {
		return fmt.Errorf("MCP output File %s failed integrity verification", id)
	}
	if err := replaceRootFile(s.root, temporary, destination); err != nil {
		return err
	}
	s.done[id] = true
	return nil
}

type toolOutputPreparationError struct{ err error }

func (e *toolOutputPreparationError) Error() string { return "prepare MCP output: " + e.err.Error() }
func (e *toolOutputPreparationError) Unwrap() error { return e.err }

func (r *SessionToolRunner) prepareToolOutput(ctx context.Context, event SessionEvent) (err error) {
	defer func() {
		if err != nil {
			err = &toolOutputPreparationError{err: err}
		}
	}()
	if r.opts.Workdir == "" || event.AgentMCPToolResultEvent == nil {
		return nil
	}
	id, ok := event.AgentMCPToolResultEvent.FileID.Get()
	if !ok {
		return nil
	}
	if r.outputs == nil {
		setup, err := newSessionToolOutputs(r.opts.Workdir)
		if err != nil {
			return err
		}
		r.outputs = setup
	}
	return r.outputs.materialize(ctx, r.client, id)
}
