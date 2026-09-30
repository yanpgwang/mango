package domain

import (
	"encoding/json"
	"testing"
)

func TestSkillReferenceRejectsNonObjectJSON(t *testing.T) {
	for _, input := range []string{`"opaque-value"`, `42`, `[]`, `true`} {
		t.Run(input, func(t *testing.T) {
			var reference SkillReference
			if err := json.Unmarshal([]byte(input), &reference); err == nil {
				t.Fatal("non-object Skill reference was accepted")
			}
		})
	}
}
