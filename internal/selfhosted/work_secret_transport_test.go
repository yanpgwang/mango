package selfhosted

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestWorkSecretTransportRoundTripWithPartialWrites(t *testing.T) {
	secret := "eyJzZXNzaW9uc190b2tlbiI6InNlc3NfbWFuZ29fdGVzdCJ9"
	var encoded bytes.Buffer
	if err := WriteWorkSecret(oneByteWriter{Writer: &encoded}, secret); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadWorkSecret(&encoded); err != nil || got != secret {
		t.Fatalf("ReadWorkSecret() = %q, %v", got, err)
	}
}

func TestWorkSecretTransportRejectsInvalidFrames(t *testing.T) {
	var oversized [4]byte
	binary.BigEndian.PutUint32(oversized[:], maxWorkSecretTransportBytes+1)
	for name, encoded := range map[string][]byte{
		"missing header": nil,
		"empty":          {0, 0, 0, 0},
		"oversized":      oversized[:],
		"short payload":  {0, 0, 0, 2, 'x'},
	} {
		t.Run(name, func(t *testing.T) {
			if secret, err := ReadWorkSecret(bytes.NewReader(encoded)); err == nil || secret != "" {
				t.Fatalf("ReadWorkSecret() = %q, %v", secret, err)
			}
		})
	}
	for name, secret := range map[string]string{
		"empty":     "",
		"oversized": strings.Repeat("x", maxWorkSecretTransportBytes+1),
	} {
		t.Run("write "+name, func(t *testing.T) {
			if err := WriteWorkSecret(io.Discard, secret); err == nil {
				t.Fatal("WriteWorkSecret() succeeded")
			}
		})
	}
	if err := WriteWorkSecret(errorWriter{}, "secret"); err == nil {
		t.Fatal("WriteWorkSecret() ignored writer error")
	}
}

type oneByteWriter struct{ io.Writer }

func (writer oneByteWriter) Write(payload []byte) (int, error) {
	if len(payload) > 1 {
		payload = payload[:1]
	}
	return writer.Writer.Write(payload)
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
