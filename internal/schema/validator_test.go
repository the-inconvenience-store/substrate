//go:build integration

package schema

import (
	"context"
	"testing"

	"github.com/substrate/substrate/internal/apierr"
)

func TestValidator_FlexibleReturnsZero(t *testing.T) {
	svc, _, col := setup(t)
	v := NewValidator(svc)
	ver, err := v.ValidateWrite(context.Background(), col, map[string]any{"anything": true})
	if err != nil || ver != 0 {
		t.Fatalf("flexible collection should validate trivially as version 0, got v%d err=%v", ver, err)
	}
}

func TestValidator_TypedValidatesAndStamps(t *testing.T) {
	svc, ws, col := setup(t)
	ctx := context.Background()
	_, _ = svc.Register(ctx, RegisterCmd{Workspace: ws, Collection: col, JSONSchema: personSchema()})
	v := NewValidator(svc)

	ver, err := v.ValidateWrite(ctx, col, map[string]any{"name": "Ada"})
	if err != nil || ver != 1 {
		t.Fatalf("valid write should pass as v1, got v%d err=%v", ver, err)
	}

	_, err = v.ValidateWrite(ctx, col, map[string]any{"name": 123}) // wrong type
	e, ok := apierr.As(err)
	if !ok || e.Code != apierr.Validation {
		t.Fatalf("invalid write should be validation_failed, got %v", err)
	}
}
