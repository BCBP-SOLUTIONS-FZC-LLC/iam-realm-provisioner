package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
)

// TestIdpAuditEntry_MetadataWithinCap_PassedThrough verifies that metadata
// under 8192 bytes is serialized and stored verbatim.
func TestIdpAuditEntry_MetadataWithinCap_PassedThrough(t *testing.T) {
	e := idpAuditEntry(context.Background(), uuid.New(), "alias", domain.AuditActionCreate,
		map[string]any{"protocol": "saml", "secret_rotated": false})
	var meta map[string]any
	require.NoError(t, json.Unmarshal(e.Metadata, &meta))
	assert.Equal(t, "saml", meta["protocol"])
	assert.LessOrEqual(t, len(e.Metadata), auditMetadataMaxBytes)
}

// TestIdpAuditEntry_MetadataExceedsAL5Cap_SubstitutesTruncatedMarker guards
// against AL-5 returning 422 metadata_too_large (→ ErrAuditEntryRejected →
// MarkFailed) when IdP validation checks produce an unusually large payload.
func TestIdpAuditEntry_MetadataExceedsAL5Cap_SubstitutesTruncatedMarker(t *testing.T) {
	big := strings.Repeat("x", 9000)
	e := idpAuditEntry(context.Background(), uuid.New(), "alias", domain.AuditActionUpdate,
		map[string]any{"checks": big})

	var meta map[string]any
	require.NoError(t, json.Unmarshal(e.Metadata, &meta), "fallback must be valid JSON")
	v, ok := meta["_truncated"]
	assert.True(t, ok, "fallback must carry _truncated key")
	assert.Equal(t, true, v)
	assert.LessOrEqual(t, len(e.Metadata), auditMetadataMaxBytes,
		"fallback metadata must itself be within the AL-5 cap")
}
