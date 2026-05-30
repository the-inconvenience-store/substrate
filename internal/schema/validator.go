package schema

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/substrate/substrate/internal/apierr"
)

// bytesReader adapts a byte slice to the io.Reader the jsonschema API expects.
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// activeResolver is the subset of the registry the validator needs.
type activeResolver interface {
	GetActive(ctx context.Context, col uuid.UUID) (ActiveSchema, error)
}

// Validator resolves a collection's active schema and validates record bodies.
// It satisfies record.Validator.
type Validator struct {
	reg   activeResolver
	mu    sync.RWMutex
	cache map[string]*jsonschema.Schema // key: "<collection>:<version>"
}

func NewValidator(reg activeResolver) *Validator {
	return &Validator{reg: reg, cache: map[string]*jsonschema.Schema{}}
}

// ValidateWrite validates data against the collection's active schema. Returns the
// active version to stamp (0 for flexible collections with no active schema).
func (v *Validator) ValidateWrite(ctx context.Context, col uuid.UUID, data map[string]any) (int, error) {
	active, err := v.reg.GetActive(ctx, col)
	if err != nil {
		if e, ok := apierr.As(err); ok && e.Code == apierr.NotFound {
			return 0, nil // flexible: no active schema
		}
		return 0, err
	}
	compiled, err := v.compiled(col, active)
	if err != nil {
		return 0, err
	}
	if err := compiled.Validate(data); err != nil {
		return 0, apierr.New(apierr.Validation, "record does not match schema").
			WithDetails(map[string]any{"errors": fmt.Sprintf("%v", err)})
	}
	return active.Version, nil
}

func (v *Validator) compiled(col uuid.UUID, active ActiveSchema) (*jsonschema.Schema, error) {
	key := fmt.Sprintf("%s:%d", col, active.Version)
	v.mu.RLock()
	c := v.cache[key]
	v.mu.RUnlock()
	if c != nil {
		return c, nil
	}
	parsed, err := jsonschema.UnmarshalJSON(bytesReader(active.Raw))
	if err != nil {
		return nil, fmt.Errorf("parse active schema: %w", err)
	}
	comp := jsonschema.NewCompiler()
	const resID = "substrate://active.json"
	if err := comp.AddResource(resID, parsed); err != nil {
		return nil, fmt.Errorf("add active schema: %w", err)
	}
	sch, err := comp.Compile(resID)
	if err != nil {
		return nil, fmt.Errorf("compile active schema: %w", err)
	}
	v.mu.Lock()
	v.cache[key] = sch
	v.mu.Unlock()
	return sch, nil
}
