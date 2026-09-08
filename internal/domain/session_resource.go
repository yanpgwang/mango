package domain

import (
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	SessionMemoryRoot                 = "/mnt/memory"
	SessionWorkspaceRoot              = "/workspace"
	MaxSessionMemoryStores            = 8
	MaxSessionMemoryInstructionsChars = 4096
	maxSessionMemoryMountSlugBytes    = 255
)

const (
	SessionResourceTypeMemoryStore = "memory_store"
	MemoryAccessReadWrite          = "read_write"
	MemoryAccessReadOnly           = "read_only"
)

type SessionResourceState string

const (
	SessionResourceActive   SessionResourceState = "active"
	SessionResourceDeleting SessionResourceState = "deleting"
)

// SessionResource is a durable Memory Store binding. File and repository
// staging belong to the operator-owned self-hosted worker workspace.
type SessionResource struct {
	ID                     string
	SessionID              string
	ResourceType           string
	MemoryStoreID          string
	MemoryAccess           string
	MemoryInstructions     string
	MemoryStoreName        string
	MemoryStoreDescription string
	MountPath              string
	CreatedAt              time.Time
	UpdatedAt              time.Time
	State                  SessionResourceState
}

func (r SessionResource) Type() string { return r.ResourceType }

// NormalizeSessionMemoryStoreMountPath follows the Managed Agents convention:
// lowercase the Store name and collapse each run of non-alphanumeric Unicode
// characters to one hyphen beneath /mnt/memory.
func NormalizeSessionMemoryStoreMountPath(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", Validation("memory store name must be valid UTF-8")
	}
	var slug strings.Builder
	lastHyphen := false
	for _, character := range strings.ToLower(name) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			slug.WriteRune(character)
			lastHyphen = false
			continue
		}
		if slug.Len() > 0 && !lastHyphen {
			slug.WriteByte('-')
			lastHyphen = true
		}
	}
	value := strings.TrimSuffix(slug.String(), "-")
	if value == "" {
		return "", Validation("memory store name must produce a non-empty mount slug")
	}
	if len(value) > maxSessionMemoryMountSlugBytes {
		return "", Validation("memory store mount slug cannot exceed 255 bytes")
	}
	return path.Join(SessionMemoryRoot, value), nil
}
