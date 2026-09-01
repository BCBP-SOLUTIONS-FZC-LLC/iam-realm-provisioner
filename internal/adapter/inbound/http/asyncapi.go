package http

import (
	"net/http"

	"github.com/gin-gonic/gin"

	apispec "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/api"
)

// AsyncAPIYAMLHandler serves the raw AsyncAPI 3.0 spec (§7.3).
func AsyncAPIYAMLHandler(c *gin.Context) {
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", apispec.AsyncAPISpec)
}

// AsyncAPIHandler renders a minimal HTML landing page linking to the raw
// spec — this service's event catalog is small (9 produced + 6 consumed
// message types on one shared topic), so a full interactive renderer isn't
// warranted; the raw YAML at /asyncapi.yaml is the source of truth CI's
// drift gate validates against.
func AsyncAPIHandler(c *gin.Context) {
	const page = `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>iam-realm-provisioner — Event Catalog</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; max-width: 860px; margin: 3rem auto; padding: 0 1rem; color: #1a1a1a; }
    h1 { font-size: 1.4rem; }
    code { background: #f2f2f2; padding: 0.15rem 0.4rem; border-radius: 3px; }
    ul { line-height: 1.8; }
    a { color: #0b5fff; }
  </style>
</head>
<body>
  <h1>iam-realm-provisioner — Event Catalog</h1>
  <p>Raw AsyncAPI 3.0 contract: <a href="/asyncapi.yaml">/asyncapi.yaml</a></p>
  <p>Publishes 9 tenant-lifecycle events on the shared <code>iam-tenant-events</code> SNS topic
     (two-producer topic with Org &amp; Membership, HLD §9.1.1):</p>
  <ul>
    <li>TrialTenantProvisioned</li>
    <li>TenantRealmReady</li>
    <li>TenantConverted</li>
    <li>DirectPaidSignup</li>
    <li>TrialExpired</li>
    <li>TrialReactivated</li>
    <li>TenantSuspended</li>
    <li>TenantOffboarded</li>
    <li>TenantReactivated (source=operator)</li>
  </ul>
  <p>Consumes three SQS queues: <code>membership-realm-q</code>, <code>tenant-realm-q</code>,
     <code>billing-realm-q</code>.</p>
</body>
</html>`
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(page))
}
