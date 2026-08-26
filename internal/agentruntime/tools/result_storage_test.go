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

func TestMaterializeLargeResult_CanBeInspectedInBoundedBashChunks(t *testing.T) {
	sb := newSB(t)
	full := strings.Repeat("0123456789", MaxInlineResultChars/10+1)
	materialized, err := MaterializeLargeResult(
		context.Background(),
		sb,
		"sevt_chunked",
		textResult(full, false),
	)
	if err != nil {
		t.Fatal(err)
	}
	message := resultText(t, materialized)
	if !strings.Contains(message, "dd if=tool-results/sevt_chunked.txt") ||
		!strings.Contains(message, "bs=65536 skip=0 count=1") {
		t.Fatalf("materialized guidance = %q", message)
	}

	readResult := Registry()["read"](context.Background(), sb, map[string]any{
		"path":       "tool-results/sevt_chunked.txt",
		"view_range": []any{float64(1), float64(1)},
	})
	if !readResult.IsError || !strings.Contains(resultText(t, readResult), "use bash") {
		t.Fatalf("oversized line read = %#v", readResult)
	}
	rematerialized, err := MaterializeLargeResult(
		context.Background(), sb, "sevt_read_retry", readResult,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resultText(t, rematerialized) != resultText(t, readResult) {
		t.Fatalf("read error was materialized again: %#v", rematerialized)
	}

	chunk := Registry()["bash"](context.Background(), sb, map[string]any{
		"command": "dd if=tool-results/sevt_chunked.txt bs=65536 skip=1 count=1 2>/dev/null",
	})
	if chunk.IsError || resultText(t, chunk) != full[65536:] {
		t.Fatalf("bounded bash chunk = %#v", chunk)
	}
}
