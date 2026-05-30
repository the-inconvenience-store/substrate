//go:build integration

package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	sharedOnce sync.Once
	sharedDSN  string // admin DSN to the shared container's "postgres" database
	sharedErr  error
	dbCounter  atomicCounter
)

type atomicCounter struct {
	mu sync.Mutex
	n  int
}

func (c *atomicCounter) next() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.n
}

// startShared boots a single Postgres container for the whole test binary.
func startShared(ctx context.Context) (string, error) {
	sharedOnce.Do(func() {
		container, err := tcpg.Run(ctx, "postgres:16-alpine",
			tcpg.WithDatabase("postgres"),
			tcpg.WithUsername("postgres"),
			tcpg.WithPassword("postgres"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second)),
		)
		if err != nil {
			sharedErr = err
			return
		}
		sharedDSN, sharedErr = container.ConnectionString(ctx, "sslmode=disable")
	})
	return sharedDSN, sharedErr
}

// NewTestPool creates a brand-new database in the shared container, applies all
// migrations, and returns a pool connected to it. Use for per-test isolation.
// Accepts testing.TB so both tests (*testing.T) and benchmarks (*testing.B) can call it.
func NewTestPool(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	ctx := context.Background()

	adminDSN, err := startShared(ctx)
	if err != nil {
		tb.Fatalf("start shared postgres: %v", err)
	}

	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		tb.Fatalf("connect admin: %v", err)
	}
	defer admin.Close()

	dbName := fmt.Sprintf("test_%d_%d", time.Now().UnixNano(), dbCounter.next())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		tb.Fatalf("create db %s: %v", dbName, err)
	}

	dsn := replaceDBName(adminDSN, dbName)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		tb.Fatalf("connect %s: %v", dbName, err)
	}
	if err := Migrate(ctx, pool); err != nil {
		tb.Fatalf("migrate: %v", err)
	}
	tb.Cleanup(func() { pool.Close() })
	return pool
}

// replaceDBName swaps the database path segment of a postgres URL.
func replaceDBName(dsn, db string) string {
	// DSNs from testcontainers look like postgres://user:pass@host:port/postgres?sslmode=disable
	slash := -1
	q := len(dsn)
	for i := len(dsn) - 1; i >= 0; i-- {
		if dsn[i] == '?' {
			q = i
		}
		if dsn[i] == '/' {
			slash = i
			break
		}
	}
	return dsn[:slash+1] + db + dsn[q:]
}
