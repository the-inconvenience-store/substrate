//go:build integration

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// drain reads and closes a response body so connections are reused and the body
// allocation is realistic and not leaked across iterations.
func drain(b *testing.B, resp *http.Response, want int) {
	b.Helper()
	if resp.StatusCode != want {
		b.Fatalf("status = %d, want %d", resp.StatusCode, want)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func benchPayload(marker string) map[string]any {
	return map[string]any{"data": map[string]any{
		"title": "bench " + marker, "status": "active", "priority": 3,
		"filler": strings.Repeat("x", 900),
	}}
}

// seedHTTP creates the "bench" collection and n records over HTTP, returning the
// first record id (for Get/History). Runs before the timed loop.
func seedHTTP(b *testing.B, url, key string, n int) string {
	b.Helper()
	resp := doAs(b, "POST", url+"/v1/collections", key, "agent-1", map[string]any{"name": "bench"})
	drain(b, resp, http.StatusCreated)
	first := ""
	for i := 0; i < n; i++ {
		resp := doAs(b, "POST", url+"/v1/collections/bench/records", key, "agent-1", benchPayload(strconv.Itoa(i)))
		if resp.StatusCode != http.StatusCreated {
			b.Fatalf("seed create status = %d", resp.StatusCode)
		}
		if first == "" {
			var created struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&created)
			first = created.ID
		} else {
			_, _ = io.Copy(io.Discard, resp.Body)
		}
		resp.Body.Close()
	}
	return first
}

func BenchmarkHTTP_CreateRecord(b *testing.B) {
	srv, key, _, _ := newProjServer(b)
	resp := doAs(b, "POST", srv.URL+"/v1/collections", key, "agent-1", map[string]any{"name": "bench"})
	drain(b, resp, http.StatusCreated)
	body := benchPayload("create")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := doAs(b, "POST", srv.URL+"/v1/collections/bench/records", key, "agent-1", body)
		drain(b, resp, http.StatusCreated)
	}
}

func BenchmarkHTTP_GetRecord(b *testing.B) {
	srv, key, _, _ := newProjServer(b)
	id := seedHTTP(b, srv.URL, key, 1)
	url := srv.URL + "/v1/collections/bench/records/" + id
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := doAs(b, "GET", url, key, "agent-1", nil)
		drain(b, resp, http.StatusOK)
	}
}

func benchmarkHTTPList(b *testing.B, n int) {
	b.Helper()
	srv, key, _, _ := newProjServer(b)
	seedHTTP(b, srv.URL, key, n)
	url := srv.URL + "/v1/collections/bench/records?sort=-created_at&limit=50"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := doAs(b, "GET", url, key, "agent-1", nil)
		drain(b, resp, http.StatusOK)
	}
}

func BenchmarkHTTP_ListRecords_Small(b *testing.B) { benchmarkHTTPList(b, 100) }
func BenchmarkHTTP_ListRecords_Large(b *testing.B) { benchmarkHTTPList(b, 10000) }

func BenchmarkHTTP_History(b *testing.B) {
	srv, key, _, _ := newProjServer(b)
	id := seedHTTP(b, srv.URL, key, 1)
	// Add a few revisions so history has content.
	for r := 0; r < 4; r++ {
		req := mustJSONReq(b, "PATCH", srv.URL+"/v1/collections/bench/records/"+id, benchPayload("u"+strconv.Itoa(r)))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Substrate-Actor", "agent-1")
		req.Header.Set("If-Match", `"`+strconv.Itoa(r+1)+`"`)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			b.Fatalf("seed update: %v", err)
		}
		drain(b, resp, http.StatusOK)
	}
	url := srv.URL + "/v1/collections/bench/records/" + id + "/history"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := doAs(b, "GET", url, key, "agent-1", nil)
		drain(b, resp, http.StatusOK)
	}
}
