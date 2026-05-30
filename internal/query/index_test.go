package query

import (
	"testing"

	"github.com/google/uuid"
)

func TestIndexName_Deterministic(t *testing.T) {
	col := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	a := indexName(col, "price")
	b := indexName(col, "price")
	if a != b {
		t.Fatalf("non-deterministic: %s != %s", a, b)
	}
	if indexName(col, "price") == indexName(col, "color") {
		t.Fatal("different fields must yield different names")
	}
}

func TestIndexName_LengthBound(t *testing.T) {
	col := uuid.New()
	long := "this_is_an_extremely_long_field_name_that_would_blow_past_the_postgres_identifier_limit"
	if n := indexName(col, long); len(n) > 63 {
		t.Fatalf("index name %q len %d > 63", n, len(n))
	}
}
