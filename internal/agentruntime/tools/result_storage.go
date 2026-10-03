package tools

import (
	"strings"
	"unicode/utf8"
)

const (
	// CMA documents a file-backed preview above 100,000 characters. Mango
	// applies the same threshold to its projected model-visible UTF-8 text.
	MaxInlineResultChars = 100_000
	ResultPreviewChars   = 2_000
)

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit])
}

// ResultPreview returns the bounded UTF-8 preview used after full output retention.
func ResultPreview(text string) string {
	return strings.ReplaceAll(truncateRunes(text, ResultPreviewChars), "\x00", `\u0000`)
}
