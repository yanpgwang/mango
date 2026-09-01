package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yanpgwang/mango/internal/domain"
)

func validateFileResourceMount(mount FileResourceMount) error {
	if err := validateResourceIdentity(mount.Identity); err != nil {
		return err
	}
	if mount.SizeBytes < 0 {
		return errors.New("sandbox: file resource size cannot be negative")
	}
	decoded, err := hex.DecodeString(mount.ChecksumSHA256)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(mount.ChecksumSHA256) != mount.ChecksumSHA256 {
		return errors.New("sandbox: file resource checksum must be a lowercase SHA-256 digest")
	}
	_, err = resourceRelativePath(mount.RuntimePath)
	return err
}

func validateResourceIdentity(identity string) error {
	if identity == "" || len(identity) > 255 || !utf8.ValidString(identity) {
		return errors.New("sandbox: file resource identity is invalid")
	}
	for _, character := range identity {
		if unicode.IsControl(character) {
			return errors.New("sandbox: file resource identity contains a control character")
		}
	}
	return nil
}

func resourceRelativePath(runtimePath string) (string, error) {
	if len(runtimePath) > domain.MaxSessionFileMountPathBytes {
		return "", errors.New("sandbox: file resource path exceeds 1024 bytes")
	}
	if !utf8.ValidString(runtimePath) {
		return "", errors.New("sandbox: file resource path must be valid UTF-8")
	}
	for _, character := range runtimePath {
		if unicode.IsControl(character) {
			return "", errors.New("sandbox: file resource path contains a control character")
		}
	}
	clean := path.Clean(runtimePath)
	if clean == SessionUploadsRoot || !strings.HasPrefix(clean, SessionUploadsRoot+"/") {
		return "", fmt.Errorf(
			"sandbox: file resource path %q must be beneath %s",
			runtimePath,
			SessionUploadsRoot,
		)
	}
	relative := strings.TrimPrefix(clean, SessionUploadsRoot+"/")
	for _, component := range strings.Split(relative, "/") {
		if len(component) > domain.MaxSessionFileMountComponentBytes {
			return "", errors.New("sandbox: file resource path component exceeds 255 bytes")
		}
	}
	return relative, nil
}

func resourceMarker(mount FileResourceMount) string {
	return mount.Identity + "\n" + strconv.FormatInt(mount.SizeBytes, 10) + "\n" +
		mount.ChecksumSHA256 + "\n"
}

func resourceMarkerIdentity(marker []byte) string {
	identity, _, _ := strings.Cut(string(marker), "\n")
	return identity
}
