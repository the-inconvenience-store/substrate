package query

import (
	"strings"
	"testing"
)

func mustBuild(t *testing.T, q ListQuery) (string, []any) {
	t.Helper()
	sql, args, err := Build(q)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return sql, args
}

func TestBuild_BaseFilterSortLimit(t *testing.T) {
	q, _ := Parse([]string{"status:eq:open"}, "-created_at", "10", "")
	sql, args := mustBuild(t, q)
	if !strings.Contains(sql, "workspace_id = $1 AND collection_id = $2") {
		t.Fatalf("missing scope predicate:\n%s", sql)
	}
	if !strings.Contains(sql, "status = 'active'") {
		t.Fatalf("missing active filter:\n%s", sql)
	}
	if !strings.Contains(sql, "data->>'status' = $3") {
		t.Fatalf("missing eq predicate:\n%s", sql)
	}
	if !strings.Contains(sql, "ORDER BY created_at DESC, id DESC") {
		t.Fatalf("bad order by:\n%s", sql)
	}
	if len(args) < 1 || args[len(args)-1] != "open" {
		t.Fatalf("args = %v, want last 'open'", args)
	}
}

func TestBuild_RangeNumericCast(t *testing.T) {
	q, _ := Parse([]string{"age:gt:21"}, "", "", "")
	sql, _ := mustBuild(t, q)
	if !strings.Contains(sql, "(data->>'age')::numeric > $3") {
		t.Fatalf("missing numeric cast:\n%s", sql)
	}
}

func TestBuild_InAndExists(t *testing.T) {
	q, _ := Parse([]string{"tags:in:a,b", "note:exists:false"}, "", "", "")
	sql, args := mustBuild(t, q)
	if !strings.Contains(sql, "data->>'tags' = ANY($3)") {
		t.Fatalf("missing in predicate:\n%s", sql)
	}
	if !strings.Contains(sql, "(data ? 'note') = $4") {
		t.Fatalf("missing exists predicate:\n%s", sql)
	}
	if _, ok := args[len(args)-2].([]string); !ok {
		t.Fatalf("in arg type = %T, want []string", args[len(args)-2])
	}
	if b, ok := args[len(args)-1].(bool); !ok || b != false {
		t.Fatalf("exists arg = %v, want bool false", args[len(args)-1])
	}
}

func TestBuild_KeysetFromCursor(t *testing.T) {
	tok := encodeCursor(cursorData{Sort: "-created_at", Value: "2026-05-30T12:00:00Z", ID: "11111111-1111-1111-1111-111111111111"})
	q, _ := Parse(nil, "-created_at", "", tok)
	sql, _ := mustBuild(t, q)
	if !strings.Contains(sql, "(created_at, id) < (") {
		t.Fatalf("missing keyset predicate:\n%s", sql)
	}
}

func TestBuild_CursorSortMismatch(t *testing.T) {
	tok := encodeCursor(cursorData{Sort: "price", Value: "9", ID: "11111111-1111-1111-1111-111111111111"})
	q, _ := Parse(nil, "-created_at", "", tok)
	if _, _, err := Build(q); err == nil {
		t.Fatal("expected cursor/sort mismatch error")
	}
}
