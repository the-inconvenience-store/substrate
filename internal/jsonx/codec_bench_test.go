package jsonx_test

import (
	"encoding/json"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/substrate/substrate/internal/jsonx"
)

// These benchmarks isolate the JSON codec under sustained concurrent load to
// answer a specific question: sonic (jsonx) cuts allocation COUNT and per-op
// latency but allocates more BYTES per op. Under saturated parallelism, does the
// extra byte-churn trigger enough additional GC work to erode the latency win?
//
// Each benchmark runs the same decode workload under b.RunParallel (saturating
// GOMAXPROCS) for both encoding/json and jsonx, and reports — beyond the standard
// ns/op, B/op, allocs/op — two GC metrics measured across the timed window:
//   GCs/Mop      garbage collections per million ops (GC frequency under load)
//   gcPauseNs/op stop-the-world pause time attributable to each op
// Compare *_Std vs *_Sonic on the same workload to see the real tradeoff.

// row mirrors benchfix.Payload: ~1KB, 6 mixed-type fields, the shape List/Get
// decode per record.
func row(marker string) map[string]any {
	return map[string]any{
		"title":    "benchmark record " + marker,
		"status":   "active",
		"priority": 3,
		"score":    42.5,
		"tags":     []any{"alpha", "beta", "gamma"},
		"filler":   strings.Repeat("x", 900),
	}
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

var (
	rowJSON = mustMarshal(row("single"))
	// pageJSON is a 50-row array — the per-request decode churn of a List page,
	// where sonic's B/op regression was largest (+69%).
	pageJSON = func() []byte {
		rows := make([]map[string]any, 50)
		for i := range rows {
			rows[i] = row(strconv.Itoa(i))
		}
		return mustMarshal(rows)
	}()
)

// decodeUnderLoad runs `decode` across all cores via RunParallel and reports GC
// frequency and pause time observed during the timed window.
func decodeUnderLoad(b *testing.B, decode func(*testing.PB)) {
	b.ReportAllocs()
	var start, end runtime.MemStats
	runtime.GC() // clean slate so the pause delta reflects only this run
	runtime.ReadMemStats(&start)
	b.ResetTimer()
	b.RunParallel(decode)
	b.StopTimer()
	runtime.ReadMemStats(&end)
	b.ReportMetric(float64(end.NumGC-start.NumGC)/(float64(b.N)/1e6), "GCs/Mop")
	b.ReportMetric(float64(end.PauseTotalNs-start.PauseTotalNs)/float64(b.N), "gcPauseNs/op")
}

func decodeRow(unmarshal func([]byte, any) error) func(*testing.PB) {
	return func(pb *testing.PB) {
		for pb.Next() {
			var m map[string]any
			if err := unmarshal(rowJSON, &m); err != nil {
				panic(err)
			}
		}
	}
}

func decodePage(unmarshal func([]byte, any) error) func(*testing.PB) {
	return func(pb *testing.PB) {
		for pb.Next() {
			var rows []map[string]any
			if err := unmarshal(pageJSON, &rows); err != nil {
				panic(err)
			}
		}
	}
}

func BenchmarkDecodeRowParallel_Std(b *testing.B) {
	decodeUnderLoad(b, decodeRow(json.Unmarshal))
}

func BenchmarkDecodeRowParallel_Sonic(b *testing.B) {
	decodeUnderLoad(b, decodeRow(jsonx.Unmarshal))
}

func BenchmarkDecodePageParallel_Std(b *testing.B) {
	decodeUnderLoad(b, decodePage(json.Unmarshal))
}

func BenchmarkDecodePageParallel_Sonic(b *testing.B) {
	decodeUnderLoad(b, decodePage(jsonx.Unmarshal))
}
