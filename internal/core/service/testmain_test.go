package service

import (
	"os"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

func TestMain(m *testing.M) {
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		ServiceName:  "iam-realm-provisioner",
		BuildVersion: "test",
	})
	metrics.Register()
	os.Exit(m.Run())
}
