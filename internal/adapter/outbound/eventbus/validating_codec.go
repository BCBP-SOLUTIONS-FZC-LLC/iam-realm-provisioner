package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/eventschema"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidatingCodec wraps inner (always NoopCodec at enqueue time) and
// validates each event's payload against the embedded JSON Schema for its
// event type before allowing the outbox insert to proceed. Compiles every
// schema in internal/eventschema once at construction time.
type ValidatingCodec struct {
	inner   Codec
	schemas map[string]*jsonschema.Schema
	mu      sync.RWMutex
}

// NewValidatingCodec compiles the 9 embedded schemas and returns a codec
// that validates against them, falling through to inner. Compilation
// failure is a build-time programming error surfaced immediately (panics
// on startup via the caller's error return — CrashLoopBackoff rather than
// silently publishing malformed events, matching the sibling convention).
func NewValidatingCodec(inner Codec) (*ValidatingCodec, error) {
	c := &ValidatingCodec{inner: inner, schemas: make(map[string]*jsonschema.Schema, len(eventschema.ByEventType))}
	compiler := jsonschema.NewCompiler()
	for name, raw := range eventschema.ByEventType {
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("eventbus: parse embedded schema %s: %w", name, err)
		}
		if err := compiler.AddResource(name, doc); err != nil {
			return nil, fmt.Errorf("eventbus: add schema resource %s: %w", name, err)
		}
		sch, err := compiler.Compile(name)
		if err != nil {
			return nil, fmt.Errorf("eventbus: compile schema %s: %w", name, err)
		}
		c.schemas[name] = sch
	}
	return c, nil
}

// Encode validates payload against eventType's schema (a no-op pass-through
// when eventType has no registered schema — e.g. a future additive event
// not yet backed by a JSON Schema) and then delegates to inner.
func (c *ValidatingCodec) Encode(ctx context.Context, eventType string, payload []byte) (encoded []byte, schemaVersionID string, err error) {
	c.mu.RLock()
	sch, ok := c.schemas[eventType]
	c.mu.RUnlock()
	if ok {
		var doc any
		if err := json.Unmarshal(payload, &doc); err != nil {
			return nil, "", fmt.Errorf("validate %s: payload is not JSON: %w", eventType, err)
		}
		if err := sch.Validate(doc); err != nil {
			return nil, "", fmt.Errorf("validate %s: %w", eventType, err)
		}
	}
	return c.inner.Encode(ctx, eventType, payload)
}
