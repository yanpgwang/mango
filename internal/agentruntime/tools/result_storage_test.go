package tools

import (
	"context"
	"strings"
	"testing"
)

func TestMaterializeLargeResult_WritesFullTextAndReturnsPreview(t *testing.T) {
	sb := newSB(t)
	full := strings.Repeat("界", MaxInlineResultChars+1)
	got, err := MaterializeLargeResult(
		context.Background(),
		sb,
		"sevt_large",
		textResult(full, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 1 || got.IsError {
		t.Fatalf("materialized result = %#v", got)
	}
	message := got.Content[0].(map[string]any)["text"].(string)
	if !strings.Contains(message, "tool-results/sevt_large.txt") ||
		!strings.Contains(message, "100001 characters") {
		t.Fatalf("preview message = %q", message)
	}
	stored, err := sb.ReadFile(context.Background(), "tool-results/sevt_large.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != full {
		t.Fatalf("stored output length = %d, want %d", len(stored), len(full))
	}
}

func TestMaterializeLargeResult_LeavesThresholdInline(t *testing.T) {
	sb := newSB(t)
	full := strings.Repeat("x", MaxInlineResultChars)
	got, err := MaterializeLargeResult(
		context.Background(),
		sb,
		"sevt_inline",
		textResult(full, true),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsError || got.Content[0].(map[string]any)["text"] != full {
		t.Fatalf("inline result changed: %#v", got)
	}
	if _, err := sb.ReadFile(
		context.Background(),
		"tool-results/sevt_inline.txt",
	); err == nil {
		t.Fatal("threshold-sized result should not be written")
	}
}

func TestMaterializeLargeResult_CanBeReadByLineRange(t *testing.T) {
	sb := newSB(t)
	full := strings.Repeat("line\n", MaxInlineResultChars/5+1)
	if _, err := MaterializeLargeResult(
		context.Background(),
		sb,
		"sevt_chunked",
		textResult(full, false),
	); err != nil {
		t.Fatal(err)
	}
	got := Registry()["read"](context.Background(), sb, map[string]any{
		"path":       "tool-results/sevt_chunked.txt",
		"view_range": []any{float64(2), float64(3)},
	})
	if got.IsError || resultText(t, got) != "line\nline" {
		t.Fatalf("ranged stored output = %#v", got)
	}
}
