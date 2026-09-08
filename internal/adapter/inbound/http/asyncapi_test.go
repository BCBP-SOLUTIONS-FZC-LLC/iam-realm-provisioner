package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() { gin.SetMode(gin.TestMode) }

// TestAsyncAPIHandler_RendersEmbeddedSpec drives the AsyncAPIHandler end-to-end
// against the real embedded spec. Exercises loadAsyncSpec → readAsyncSpec →
// renderPage → renderServers/renderMessage/renderSchema/renderPropsTable /
// resolveSchema / propType / typeHTML / snsEventType / sortedKeys / walkYAML —
// the entire docs-render surface with a single request.
func TestAsyncAPIHandler_RendersEmbeddedSpec(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", nil)

	AsyncAPIHandler(c)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.True(t, len(body) > 500, "rendered doc must be substantial HTML")
	assert.Contains(t, body, "<!DOCTYPE html>")
	assert.Contains(t, body, "AsyncAPI",
		"rendered doc must reference AsyncAPI in the title/body")
	assert.Contains(t, body, "TrialTenantProvisioned",
		"rendered doc must list this service's own produced events")
	assert.Contains(t, body, "TrialStarted",
		"rendered doc must list consumed events owned by iam-org-membership")
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
}

// TestAsyncAPIYAMLHandler_ServesRawSpec covers the raw-YAML sibling route.
func TestAsyncAPIYAMLHandler_ServesRawSpec(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi.yaml", nil)

	AsyncAPIYAMLHandler(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/yaml")
	assert.Contains(t, w.Body.String(), "asyncapi: 3.0.0")
}

// TestReadAsyncSpec_InvalidYAMLReturnsError covers the parse-error branch
// so readAsyncSpec's error path is not dead code.
func TestReadAsyncSpec_InvalidYAMLReturnsError(t *testing.T) {
	_, err := readAsyncSpec([]byte("::: not-valid ::: yaml"))
	assert.Error(t, err)
}

// TestReadAsyncSpec_MinimalValidYAML covers the happy path with a small
// hand-crafted spec so we don't depend on the embedded bytes for this
// case. Also exercises asyncSchema.UnmarshalYAML property-order capture and
// asyncMessage.isConsumed's tag-suffix match.
func TestReadAsyncSpec_MinimalValidYAML(t *testing.T) {
	src := []byte(`
asyncapi: "3.0.0"
info:
  title: "Test Spec"
  version: "0.1.0"
components:
  messages:
    Sample:
      title: Sample
      tags:
        - $ref: '#/components/tags/consumed'
      payload:
        $ref: '#/components/schemas/Sample'
  schemas:
    Sample:
      type: object
      required: [id]
      properties:
        id:
          type: string
        name:
          type: string
`)
	spec, err := readAsyncSpec(src)
	require.NoError(t, err)
	assert.Equal(t, "Test Spec", spec.Info.Title)
	assert.Equal(t, "0.1.0", spec.Info.Version)

	sch, ok := spec.Comps.Schemas["Sample"]
	require.True(t, ok)
	// UnmarshalYAML must capture insertion order.
	assert.Equal(t, []string{"id", "name"}, sch.PropertyOrder,
		"schema property order must be preserved for stable render output")

	msg, ok := spec.Comps.Messages["Sample"]
	require.True(t, ok)
	assert.True(t, msg.isConsumed(), "a message tagged .../consumed must report isConsumed() true")
}
