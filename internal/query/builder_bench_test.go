package query

import "testing"

// filters with one eq + several range/exists predicates to exercise the builder.
var benchFilters = []string{
	"status:eq:active",
	"priority:gte:3",
	"score:lt:100",
	"region:eq:us-east",
	"tags:in:a,b,c",
	"archived:exists:false",
}

func BenchmarkParse(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Parse(benchFilters, "-created_at", "50", ""); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuild(b *testing.B) {
	q, err := Parse(benchFilters, "-created_at", "50", "")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := Build(q); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCursorRoundTrip(b *testing.B) {
	in := cursorData{Sort: "-created_at", Value: "2026-05-30T12:00:00Z", ID: "11111111-2222-3333-4444-555555555555"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tok := encodeCursor(in)
		if _, err := decodeCursor(tok); err != nil {
			b.Fatal(err)
		}
	}
}
