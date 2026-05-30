//go:build integration

package workspace

import (
	"context"
	"testing"

	"github.com/substrate/substrate/internal/store"
)

func TestCreateAndVerifyKey(t *testing.T) {
	pool := store.NewTestPool(t)
	ctx := context.Background()
	svc := New(pool)

	ws, err := svc.CreateWorkspace(ctx, "acme")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	plaintext, _, err := svc.CreateAPIKey(ctx, ws.ID, "default")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if len(plaintext) < 10 {
		t.Fatalf("key too short: %q", plaintext)
	}

	gotWS, err := svc.VerifyKey(ctx, plaintext)
	if err != nil {
		t.Fatalf("verify key: %v", err)
	}
	if gotWS != ws.ID {
		t.Fatalf("verify returned %s, want %s", gotWS, ws.ID)
	}

	if _, err := svc.VerifyKey(ctx, "sk_wrong"); err == nil {
		t.Fatal("expected error for bad key")
	}
}
