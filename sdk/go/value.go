package mango

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// Optional preserves absent, explicit null, and present zero values. Leave the
// zero value to omit a field, use Some(false) to send false, or Null[T]() to clear
// a nullable field. Null on non-nullable fields is rejected by the server.
// Go 1.24's omitzero support is required for these JSON semantics.
type Optional[T any] struct {
	value T
	set   bool
	null  bool
}

func Some[T any](value T) Optional[T] { return Optional[T]{value: value, set: true} }

// SomePtr is convenient for optional nullable fields such as an Agent's System.
func SomePtr[T any](value T) Optional[*T] { return Some(&value) }
func Null[T any]() Optional[T]            { return Optional[T]{set: true, null: true} }
func Ptr[T any](value T) *T               { return &value }
func (o Optional[T]) IsZero() bool        { return !o.set }
func (o Optional[T]) IsSet() bool         { return o.set }
func (o Optional[T]) IsNull() bool        { return o.set && o.null }
func (o Optional[T]) Get() (T, bool)      { return o.value, o.set && !o.null }
func (o Optional[T]) MarshalJSON() ([]byte, error) {
	if !o.set || o.null {
		return []byte("null"), nil
	}
	return json.Marshal(o.value)
}
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	*o = Optional[T]{set: true, null: bytes.Equal(bytes.TrimSpace(data), []byte("null"))}
	if o.null {
		return nil
	}
	return json.Unmarshal(data, &o.value)
}

func marshalUnion(raw json.RawMessage, variants ...any) ([]byte, error) {
	var chosen any
	for _, variant := range variants {
		if reflect.ValueOf(variant).IsNil() {
			continue
		}
		if chosen != nil {
			return nil, errors.New("mango: union must contain exactly one variant")
		}
		chosen = variant
	}
	if chosen != nil {
		return json.Marshal(chosen)
	}
	if len(raw) != 0 {
		if !json.Valid(raw) {
			return nil, errors.New("mango: invalid raw union JSON")
		}
		return raw, nil
	}
	return nil, errors.New("mango: union must contain a variant or Raw JSON")
}

type unionChoice struct {
	target    any
	kind      string
	required  []string
	constants map[string]string
}

func unmarshalUnion(data []byte, raw *json.RawMessage, choices ...unionChoice) error {
	data = bytes.TrimSpace(data)
	if !json.Valid(data) {
		return errors.New("mango: invalid union JSON")
	}
	kind := "number"
	switch data[0] {
	case '{':
		kind = "object"
	case '[':
		kind = "array"
	case '"':
		kind = "string"
	case 't', 'f':
		kind = "boolean"
	case 'n':
		kind = "null"
	}
	var object map[string]json.RawMessage
	if kind == "null" {
		*raw = append((*raw)[:0], data...)
		return nil
	}
	if kind == "object" {
		_ = json.Unmarshal(data, &object)
	}
	for _, choice := range choices {
		if choice.kind != "" && choice.kind != kind {
			continue
		}
		matches := true
		for _, name := range choice.required {
			if _, ok := object[name]; !ok {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		for name, expected := range choice.constants {
			var actualValue, expectedValue any
			if json.Unmarshal(object[name], &actualValue) != nil || json.Unmarshal([]byte(expected), &expectedValue) != nil || !reflect.DeepEqual(actualValue, expectedValue) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		if err := json.Unmarshal(data, choice.target); err == nil {
			return nil
		}
		// json.Unmarshal may allocate before a nested field fails. Clear that
		// partial variant before trying another or falling back to Raw.
		reflect.ValueOf(choice.target).Elem().SetZero()
	}
	// Unknown future variants retain their exact wire representation.
	*raw = append((*raw)[:0], data...)
	return nil
}

// DecodeRaw decodes an unknown union variant without discarding its wire shape.
func DecodeRaw[T any](raw json.RawMessage) (T, error) {
	var result T
	if len(raw) == 0 {
		return result, fmt.Errorf("mango: no raw union value")
	}
	err := json.Unmarshal(raw, &result)
	return result, err
}

func marshalExtendedObject(value any, additional map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err = json.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	for name, raw := range additional {
		if _, exists := object[name]; exists {
			return nil, fmt.Errorf("mango: additional property %q shadows a typed property", name)
		}
		object[name] = raw
	}
	return json.Marshal(object)
}

func unmarshalExtendedObject(data []byte, target any, additional *map[string]json.RawMessage, known []string) error {
	if err := json.Unmarshal(data, target); err != nil {
		return err
	}
	if err := json.Unmarshal(data, additional); err != nil {
		return err
	}
	for _, key := range known {
		delete(*additional, key)
	}
	return nil
}
