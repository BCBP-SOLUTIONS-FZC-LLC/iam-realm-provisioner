package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

const (
	glueHeaderVersion byte = 0x03 // magic byte for the AWS Glue Schema Registry wire format
	glueNoCompression byte = 0x00
	glueHeaderSize         = 18 // 1 (version) + 1 (compression) + 16 (schema version UUID)
)

// GlueCodec implements events.Codec against the shared `iam-tenant-events`
// Glue registry (§7.3.1) — this service registers its 9 schema names
// there, disjoint from O&M's TenantCreated/TrialStarted (RP-D5).
type GlueCodec struct {
	client       *glue.Client
	registryName string
	mu           sync.RWMutex
	versionCache map[string]string // schema name -> schema version UUID string
	log          port.SlogStyleLogger
}

var _ events.Codec = (*GlueCodec)(nil)

// NewGlueCodec prefetches the current version ID of every name in
// schemaNames from registryName. Failure here is fatal at startup (the pod
// crashes and Kubernetes restarts it) — a pod that cannot resolve its
// schema versions would otherwise silently publish malformed events.
func NewGlueCodec(ctx context.Context, client *glue.Client, registryName string, schemaNames []string) (*GlueCodec, error) {
	c := &GlueCodec{client: client, registryName: registryName, versionCache: make(map[string]string, len(schemaNames))}
	for _, name := range schemaNames {
		id, err := c.fetchVersionID(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("prefetch glue schema %q in registry %q: %w", name, registryName, err)
		}
		c.versionCache[name] = id
	}
	return c, nil
}

// WithLogger attaches a logger for background refresh warnings and returns
// the codec for chaining.
func (g *GlueCodec) WithLogger(log port.Logger) *GlueCodec {
	g.log = port.NewSlogStyleLogger(log)
	return g
}

// Encode prepends the Glue Schema Registry wire header (schema version UUID
// resolved from the cache) to payload.
func (g *GlueCodec) Encode(ctx context.Context, schemaName string, payload json.RawMessage) (encoded []byte, schemaVersionID string, err error) {
	versionID, err := g.versionID(ctx, schemaName)
	if err != nil {
		return nil, "", fmt.Errorf("get glue schema version %q: %w", schemaName, err)
	}
	out, err := prependGlueHeader(versionID, payload)
	if err != nil {
		return nil, "", err
	}
	return out, versionID, nil
}

// Decode strips the Glue wire header from encoded, returning the raw JSON
// payload.
func (g *GlueCodec) Decode(_ context.Context, _ string, encoded []byte) (json.RawMessage, error) {
	return stripGlueHeader(encoded)
}

func (g *GlueCodec) versionID(ctx context.Context, schemaName string) (string, error) {
	g.mu.RLock()
	id, ok := g.versionCache[schemaName]
	g.mu.RUnlock()
	if ok {
		return id, nil
	}
	id, err := g.fetchVersionID(ctx, schemaName)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	g.versionCache[schemaName] = id
	g.mu.Unlock()
	return id, nil
}

func (g *GlueCodec) fetchVersionID(ctx context.Context, schemaName string) (string, error) {
	out, err := g.client.GetSchemaVersion(ctx, &glue.GetSchemaVersionInput{
		SchemaId:            &gluetypes.SchemaId{SchemaName: &schemaName, RegistryName: &g.registryName},
		SchemaVersionNumber: &gluetypes.SchemaVersionNumber{LatestVersion: true},
	})
	if err != nil {
		return "", err
	}
	if out.SchemaVersionId == nil {
		return "", fmt.Errorf("nil SchemaVersionId for schema %q in registry %q", schemaName, g.registryName)
	}
	return *out.SchemaVersionId, nil
}

// StartRefresher re-fetches every cached schema version ID on an interval
// (background goroutine, exits on ctx.Done()); fetch errors are non-fatal
// and logged — a new Glue schema version takes effect without a pod
// restart.
func (g *GlueCodec) StartRefresher(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				g.mu.RLock()
				names := make([]string, 0, len(g.versionCache))
				for name := range g.versionCache {
					names = append(names, name)
				}
				g.mu.RUnlock()
				for _, name := range names {
					id, err := g.fetchVersionID(ctx, name)
					if err != nil {
						g.log.WarnContext(ctx, "glue codec refresh failed", "schema", name, "error", err.Error())
						continue
					}
					g.mu.Lock()
					g.versionCache[name] = id
					g.mu.Unlock()
				}
			}
		}
	}()
}

func prependGlueHeader(schemaVersionID string, payload []byte) ([]byte, error) {
	id, err := uuid.Parse(schemaVersionID)
	if err != nil {
		return nil, fmt.Errorf("parse schema version UUID %q: %w", schemaVersionID, err)
	}
	out := make([]byte, glueHeaderSize+len(payload))
	out[0] = glueHeaderVersion
	out[1] = glueNoCompression
	copy(out[2:18], id[:])
	copy(out[glueHeaderSize:], payload)
	return out, nil
}

func stripGlueHeader(encoded []byte) (json.RawMessage, error) {
	if len(encoded) < glueHeaderSize {
		return nil, fmt.Errorf("glue codec: encoded payload is %d bytes — shorter than the %d-byte Glue header", len(encoded), glueHeaderSize)
	}
	if encoded[0] != glueHeaderVersion {
		return nil, fmt.Errorf("glue codec: unexpected header version byte 0x%02x — want 0x%02x", encoded[0], glueHeaderVersion)
	}
	return json.RawMessage(encoded[glueHeaderSize:]), nil
}

// GlueDecodeCodec decodes Glue-wire-format payloads WITHOUT a Glue client,
// registry name, or schema names — stripping the header is fully
// self-describing, so no registry lookup is needed to decode, only to
// encode. Used by this service's 3 SQS consumers, which decode events
// produced by O&M/Billing and may not have those producers' Glue registry
// configured (§7.1: "both consumers need a decode-capable codec regardless
// of this service's own publish-side Glue configuration").
type GlueDecodeCodec struct{}

var _ events.Codec = GlueDecodeCodec{}

// Encode always fails — GlueDecodeCodec is decode-only.
func (GlueDecodeCodec) Encode(_ context.Context, eventType string, _ json.RawMessage) (encoded []byte, schemaVersionID string, err error) {
	return nil, "", fmt.Errorf("glue decode codec: Encode is not supported (decode-only, event type %q) — use GlueCodec to publish", eventType)
}

// Decode strips the Glue wire header from encoded, returning the raw JSON
// payload.
func (GlueDecodeCodec) Decode(_ context.Context, _ string, encoded []byte) (json.RawMessage, error) {
	return stripGlueHeader(encoded)
}
