package eventbus

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoopCodec_PassesThroughUnchanged(t *testing.T) {
	payload := []byte(`{"tenant_id":"11111111-1111-1111-1111-111111111111"}`)
	out, versionID, err := NoopCodec{}.Encode(context.Background(), "TrialTenantProvisioned", payload)
	require.NoError(t, err)
	assert.Equal(t, payload, out)
	assert.Empty(t, versionID)
}
