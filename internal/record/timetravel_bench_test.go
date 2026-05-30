//go:build integration

package record_test

import (
	"testing"

	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/store/benchfix"
)

func benchmarkHistory(b *testing.B, revisions int) {
	b.Helper()
	pool := store.NewTestPool(b)
	f := benchfix.Setup(b, pool)
	svc := underTest(f)
	ctx := b.Context()
	id := f.SeedHistory(b, revisions)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.History(ctx, f.Workspace, f.Collection, id, "bench"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHistory_Short(b *testing.B) { benchmarkHistory(b, 5) }
func BenchmarkHistory_Long(b *testing.B)  { benchmarkHistory(b, 500) }

func BenchmarkGetAsOf_Deep(b *testing.B) {
	pool := store.NewTestPool(b)
	f := benchfix.Setup(b, pool)
	svc := underTest(f)
	ctx := b.Context()
	id := f.SeedHistory(b, 500)
	at := record.AsOf{Revision: 250} // mid-history point against a deep event stream
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.GetAsOf(ctx, f.Workspace, f.Collection, id, at, "bench"); err != nil {
			b.Fatal(err)
		}
	}
}
