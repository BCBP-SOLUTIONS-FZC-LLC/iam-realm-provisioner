package consumer

import (
	"os"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// TestMain registers business metrics before any test runs — without this,
// metrics.Register is never called in this package's test binary, so every
// metrics.* package var dedup.go/tenant_consumer.go/etc. guard with a
// nil-check stays nil and the increment inside each guard never executes,
// even though the nil-check itself (correctly) lets the surrounding
// dedup/ack logic proceed either way.
func TestMain(m *testing.M) {
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		ServiceName:  "iam-realm-provisioner",
		Domain:       "iam",
		Environment:  "test",
		BuildVersion: "test",
	})
	metrics.Register("test")
	os.Exit(m.Run())
}
