//go:build integration

package record_test

import (
	"testing"

	"github.com/substrate/substrate/internal/policy"
	"github.com/substrate/substrate/internal/query"
	"github.com/substrate/substrate/internal/record"
	"github.com/substrate/substrate/internal/schema"
	"github.com/substrate/substrate/internal/store"
	"github.com/substrate/substrate/internal/store/benchfix"
)

// underTest builds a record.Service wired like main.go: real schema validator +
// policy evaluator. The collection is flexible (no active schema) and there are no
// policy rules, so GetActive and Authorize each do one realistic DB round-trip.
func underTest(pool *benchfix.Fixture) *record.Service {
	reg := schema.NewWithIndexer(pool.Pool, query.NewIndexer(pool.Pool))
	return record.New(pool.Pool, schema.NewValidator(reg)).WithEvaluator(policy.NewEngine(pool.Pool))
}

func BenchmarkCreate(b *testing.B) {
	pool := store.NewTestPool(b)
	f := benchfix.Setup(b, pool)
	svc := underTest(f)
	ctx := b.Context()
	data := benchfix.Payload("create")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Create(ctx, record.CreateCmd{
			Workspace: f.Workspace, Collection: f.Collection, Actor: "bench", Data: data,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUpdate(b *testing.B) {
	pool := store.NewTestPool(b)
	f := benchfix.Setup(b, pool)
	svc := underTest(f)
	ctx := b.Context()
	id := f.SeedRecords(b, 1)[0] // starts at revision 1
	data := benchfix.Payload("update")
	rev := int64(1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec, err := svc.Update(ctx, record.UpdateCmd{
			Workspace: f.Workspace, Collection: f.Collection, ID: id,
			ExpectedRevision: rev, Actor: "bench", Data: data,
		})
		if err != nil {
			b.Fatal(err)
		}
		rev = rec.Revision
	}
}

func BenchmarkDelete(b *testing.B) {
	pool := store.NewTestPool(b)
	f := benchfix.Setup(b, pool)
	svc := underTest(f)
	ctx := b.Context()
	ids := f.SeedRecords(b, b.N) // one distinct record per iteration
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := svc.Delete(ctx, f.Workspace, f.Collection, ids[i], "bench"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGet(b *testing.B) {
	pool := store.NewTestPool(b)
	f := benchfix.Setup(b, pool)
	svc := underTest(f)
	ctx := b.Context()
	id := f.SeedRecords(b, 1)[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Get(ctx, f.Workspace, f.Collection, id, "bench"); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkList(b *testing.B, n int) {
	b.Helper()
	pool := store.NewTestPool(b)
	f := benchfix.Setup(b, pool)
	svc := underTest(f)
	ctx := b.Context()
	f.SeedRecords(b, n)
	q, err := query.Parse(nil, "-created_at", "50", "")
	if err != nil {
		b.Fatalf("parse: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := svc.List(ctx, f.Workspace, f.Collection, "bench", q); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkList_Small(b *testing.B) { benchmarkList(b, 100) }
func BenchmarkList_Large(b *testing.B) { benchmarkList(b, 10000) }
