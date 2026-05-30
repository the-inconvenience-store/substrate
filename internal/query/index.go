package query

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Indexer creates the expression indexes that back declared indexed_fields.
// It implements the schema package's Indexer seam.
type Indexer struct {
	pool *pgxpool.Pool
}

func NewIndexer(pool *pgxpool.Pool) *Indexer { return &Indexer{pool: pool} }

// indexName derives a deterministic, <=63-char index name from a collection id
// and field. Postgres truncates identifiers at 63 bytes, so we hash to stay
// unique and bounded.
func indexName(col uuid.UUID, field string) string {
	h := sha1.Sum([]byte(col.String() + ":" + field))
	return "idx_rec_" + hex.EncodeToString(h[:])[:24]
}

// EnsureCollectionIndexes idempotently creates a partial text expression index
// per field, scoped to the collection's active records.
func (ix *Indexer) EnsureCollectionIndexes(ctx context.Context, col uuid.UUID, fields []string) error {
	for _, f := range fields {
		if !fieldRe.MatchString(f) {
			// indexed_fields entries are validated at registration; skip anything odd.
			continue
		}
		stmt := fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %s ON records ((data->>'%s')) WHERE collection_id = '%s' AND status = 'active'`,
			indexName(col, f), f, col.String(),
		)
		if _, err := ix.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("ensure index on %q: %w", f, err)
		}
	}
	return nil
}
