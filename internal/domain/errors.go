package domain

type ErrKind int

const (
	KindValidation ErrKind = iota
	KindConflict
	KindNotFound
	KindUnsupported
	KindTooLarge
	KindPrecondition
)

type DomainError struct {
	Kind    ErrKind
	Code    string
	Message string
}

func (e *DomainError) Error() string { return e.Message }

var ErrInvalidTransition = &DomainError{Kind: KindValidation, Message: "invalid status transition"}

func Validation(msg string) *DomainError  { return &DomainError{Kind: KindValidation, Message: msg} }
func Conflict(msg string) *DomainError    { return &DomainError{Kind: KindConflict, Message: msg} }
func NotFound(msg string) *DomainError    { return &DomainError{Kind: KindNotFound, Message: msg} }
func Unsupported(msg string) *DomainError { return &DomainError{Kind: KindUnsupported, Message: msg} }
func TooLarge(msg string) *DomainError    { return &DomainError{Kind: KindTooLarge, Message: msg} }
func Precondition(msg string) *DomainError {
	return &DomainError{Kind: KindPrecondition, Message: msg}
}

func MemoryPrecondition(msg string) *DomainError {
	return &DomainError{
		Kind: KindConflict, Code: "memory_precondition_failed_error", Message: msg,
	}
}

// SessionResourceNotFound identifies a source that disappeared or cannot be
// resolved while admitting a new immutable Session resource snapshot.
func SessionResourceNotFound(msg string) *DomainError {
	return &DomainError{
		Kind: KindValidation, Code: "session_resource_not_found_error", Message: msg,
	}
}
