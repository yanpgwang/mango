package selfhosted

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const maxWorkSecretTransportBytes = 16 << 10

// WriteWorkSecret writes one bounded, length-prefixed Work secret to a
// launcher-owned transport. The frame is internal to first-party launchers; it
// is not part of Mango's HTTP or SDK contract.
func WriteWorkSecret(writer io.Writer, secret string) error {
	if secret == "" {
		return errors.New("selfhosted: Work secret is required")
	}
	if len(secret) > maxWorkSecretTransportBytes {
		return errors.New("selfhosted: Work secret is too large")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(secret)))
	if err := writeAll(writer, header[:]); err != nil {
		return fmt.Errorf("selfhosted: write Work secret header: %w", err)
	}
	if err := writeAll(writer, []byte(secret)); err != nil {
		return fmt.Errorf("selfhosted: write Work secret payload: %w", err)
	}
	return nil
}

// ReadWorkSecret reads one frame written by WriteWorkSecret. Callers should
// close the transport immediately after this returns so later tool processes
// cannot recover the credential from an inherited descriptor.
func ReadWorkSecret(reader io.Reader) (string, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return "", fmt.Errorf("selfhosted: read Work secret header: %w", err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return "", errors.New("selfhosted: Work secret is required")
	}
	if size > maxWorkSecretTransportBytes {
		return "", errors.New("selfhosted: Work secret is too large")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return "", fmt.Errorf("selfhosted: read Work secret payload: %w", err)
	}
	return string(payload), nil
}

func writeAll(writer io.Writer, payload []byte) error {
	for len(payload) != 0 {
		written, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		payload = payload[written:]
	}
	return nil
}
