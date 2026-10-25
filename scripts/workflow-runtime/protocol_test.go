package workflowruntime_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
)

// This protocol belongs only to the feasibility fixture, not Mango HTTP/SDKs.
const maxFrame = 64 << 10

type failure struct {
	Code string `json:"code"`
}

type frame struct {
	Kind    string          `json:"kind"`
	ID      int             `json:"id,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Error   *failure        `json:"error,omitempty"`
	Code    string          `json:"code,omitempty"`
	Pending []int           `json:"pending,omitempty"`
}

func decodeFrame(data []byte) (frame, error) {
	if len(data) > maxFrame {
		return frame{}, errors.New("frame_limit")
	}
	var f frame
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&f); err != nil {
		return frame{}, errors.New("invalid_frame")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return frame{}, errors.New("invalid_frame")
	}
	valid := false
	switch f.Kind {
	case "invoke":
		valid = f.ID > 0 && f.ID <= 128 && len(f.Args) > 0 && len(f.Value) == 0 && f.Error == nil && f.Code == "" && len(f.Pending) == 0
	case "deliver":
		valid = f.ID > 0 && f.ID <= 128 && len(f.Args) == 0 && (len(f.Value) > 0) != (f.Error != nil) && f.Code == "" && len(f.Pending) == 0
	case "return":
		valid = len(f.Value) > 0 && f.ID == 0 && len(f.Args) == 0 && f.Error == nil && f.Code == "" && len(f.Pending) == 0
	case "failure":
		valid = f.Code != "" && f.ID == 0 && len(f.Args) == 0 && len(f.Value) == 0 && f.Error == nil && len(f.Pending) == 0
	case "wait":
		valid = len(f.Pending) > 0 && len(f.Pending) <= 128 && f.ID == 0 && len(f.Args) == 0 && len(f.Value) == 0 && f.Error == nil && f.Code == ""
		seen := map[int]bool{}
		for _, id := range f.Pending {
			if id <= 0 || id > 128 || seen[id] {
				valid = false
			}
			seen[id] = true
		}
	}
	if !valid {
		return frame{}, errors.New("invalid_frame_shape")
	}
	if f.Error != nil && (f.Error.Code == "" || len(f.Error.Code) > 128) {
		return frame{}, errors.New("invalid_error_code")
	}
	for _, payload := range []json.RawMessage{f.Args, f.Value} {
		if len(payload) > 0 && !boundedJSON(payload) {
			return frame{}, errors.New("json_value_limit")
		}
	}
	return f, nil
}

func boundedJSON(data []byte) bool {
	if len(data) > 32768 {
		return false
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return false
	}
	remaining := 4096
	var visit func(any, int) bool
	visit = func(value any, depth int) bool {
		remaining--
		if depth > 32 || remaining < 0 {
			return false
		}
		switch value := value.(type) {
		case []any:
			for _, child := range value {
				if !visit(child, depth+1) {
					return false
				}
			}
		case map[string]any:
			for _, child := range value {
				if !visit(child, depth+1) {
					return false
				}
			}
		}
		return true
	}
	return visit(value, 0)
}

func canonical(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		panic(err)
	}
	data, err = json.Marshal(decoded)
	if err != nil {
		panic(err)
	}
	return data
}

func TestFrameValidation(t *testing.T) {
	for i, input := range []string{
		`{"kind":"invoke","id":0,"args":{}}`,
		`{"kind":"invoke","id":1,"args":{},"value":1}`,
		`{"kind":"return","value":1,"authority":"admin"}`,
		`{"kind":"deliver","id":1,"value":1,"error":{"code":"x"}}`,
		`{"kind":"wait","pending":[1,1]}`,
		`{"kind":"wait","pending":[129]}`,
		`{"kind":"return","value":1} {}`,
		`{"kind":"unknown"}`,
		string(bytes.Repeat([]byte("x"), maxFrame+1)),
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if _, err := decodeFrame([]byte(input)); err == nil {
				t.Fatal("accepted invalid IPC")
			}
		})
	}
	if _, err := decodeFrame([]byte(`{"kind":"return","value":null}`)); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{
		canonical(string(bytes.Repeat([]byte("x"), 32769))),
		append(append(bytes.Repeat([]byte("["), 34), []byte("0")...), bytes.Repeat([]byte("]"), 34)...),
	} {
		if boundedJSON(data) {
			t.Fatal("accepted oversized/deep value")
		}
	}
}
