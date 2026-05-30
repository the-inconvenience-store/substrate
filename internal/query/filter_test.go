package query

import "testing"

func TestParse_Defaults(t *testing.T) {
	q, err := Parse(nil, "", "", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.Limit != 50 {
		t.Fatalf("default limit = %d, want 50", q.Limit)
	}
	if len(q.Sort) != 1 || q.Sort[0].Field != "created_at" || !q.Sort[0].Desc {
		t.Fatalf("default sort = %+v, want [-created_at]", q.Sort)
	}
	if len(q.Filters) != 0 {
		t.Fatalf("filters = %+v, want none", q.Filters)
	}
}

func TestParse_Filters(t *testing.T) {
	q, err := Parse([]string{"status:eq:open", "age:gt:21", "tags:in:a,b", "note:exists:false"}, "", "100", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.Limit != 100 {
		t.Fatalf("limit = %d, want 100", q.Limit)
	}
	if len(q.Filters) != 4 {
		t.Fatalf("filters = %d, want 4", len(q.Filters))
	}
	if q.Filters[0].Field != "status" || q.Filters[0].Op != "eq" || q.Filters[0].Value != "open" {
		t.Fatalf("filter0 = %+v", q.Filters[0])
	}
	if q.Filters[2].Op != "in" || len(q.Filters[2].List) != 2 {
		t.Fatalf("in filter = %+v", q.Filters[2])
	}
	if q.Filters[3].Op != "exists" || q.Filters[3].Value != "false" {
		t.Fatalf("exists filter = %+v", q.Filters[3])
	}
}

func TestParse_SortAscDesc(t *testing.T) {
	q, err := Parse(nil, "price", "", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.Sort[0].Field != "price" || q.Sort[0].Desc {
		t.Fatalf("sort = %+v, want price asc", q.Sort)
	}
}

func TestParse_LimitClamp(t *testing.T) {
	q, err := Parse(nil, "", "9999", "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.Limit != 200 {
		t.Fatalf("limit = %d, want clamp to 200", q.Limit)
	}
}

func TestParse_Errors(t *testing.T) {
	cases := [][]string{
		{"bad-syntax"},        // no colons
		{"f:bogus:v"},         // unknown op
		{"1bad:eq:v"},         // bad identifier
		{"note:exists:maybe"}, // exists needs true/false
	}
	for _, f := range cases {
		if _, err := Parse(f, "", "", ""); err == nil {
			t.Fatalf("expected error for %v", f)
		}
	}
	if _, err := Parse(nil, "", "abc", ""); err == nil {
		t.Fatal("expected error for non-numeric limit")
	}
	if _, err := Parse(nil, "1bad", "", ""); err == nil {
		t.Fatal("expected error for bad sort identifier")
	}
}
