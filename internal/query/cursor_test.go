package query

import "testing"

func TestCursor_RoundTrip(t *testing.T) {
	c := cursorData{Sort: "-created_at", Value: "2026-05-30T12:00:00Z", ID: "11111111-1111-1111-1111-111111111111"}
	tok := encodeCursor(c)
	if tok == "" {
		t.Fatal("empty token")
	}
	got, err := decodeCursor(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != c {
		t.Fatalf("round-trip mismatch: %+v != %+v", got, c)
	}
}

func TestCursor_Garbled(t *testing.T) {
	if _, err := decodeCursor("not-base64-!!!"); err == nil {
		t.Fatal("expected error for garbled cursor")
	}
	if _, err := decodeCursor("YWJj"); err == nil { // "abc", not JSON
		t.Fatal("expected error for non-JSON cursor")
	}
}
