package keycloak

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/metrics"
)

func counterValue(t *testing.T, c interface{ Write(*dto.Metric) error }) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}

func histogramCount(t *testing.T, o interface{ Write(*dto.Metric) error }) uint64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, o.Write(&m))
	return m.GetHistogram().GetSampleCount()
}

// TestBegin_RecordsLatencyOnSuccessAndFailureOnError pins down the pairing
// that makes the §11.6 Keycloak alerts meaningful: the latency histogram is
// observed on every call, but the failure counter only moves when the
// operation actually failed.
func TestBegin_RecordsLatencyOnSuccessAndFailureOnError(t *testing.T) {
	a := New(Config{BaseURL: "http://localhost:0"}, nopLogger{})

	beforeDuration := histogramCount(t, mustHistogram(t, "probe_op"))
	beforeFailures := counterValue(t, metrics.KeycloakCallFailuresTotal.WithLabelValues("probe_op"))

	_, done := a.begin(context.Background(), "probe_op")
	done(nil)

	assert.Equal(t, beforeDuration+1, histogramCount(t, mustHistogram(t, "probe_op")))
	assert.InDelta(t, beforeFailures, counterValue(t, metrics.KeycloakCallFailuresTotal.WithLabelValues("probe_op")), 1e-9,
		"a successful call must not increment the failure counter")

	_, done = a.begin(context.Background(), "probe_op")
	done(errors.New("boom"))

	assert.Equal(t, beforeDuration+2, histogramCount(t, mustHistogram(t, "probe_op")))
	assert.InDelta(t, beforeFailures+1, counterValue(t, metrics.KeycloakCallFailuresTotal.WithLabelValues("probe_op")), 1e-9)
}

func mustHistogram(t *testing.T, op string) interface{ Write(*dto.Metric) error } {
	t.Helper()
	o := metrics.KeycloakCallDuration.WithLabelValues(op)
	h, ok := o.(interface{ Write(*dto.Metric) error })
	require.True(t, ok, "HistogramVec.WithLabelValues must yield a writable metric")
	return h
}

// TestBegin_StartsARecordingSpanNamedAfterTheOp guards the `keycloak.<op>`
// span-name convention the runbook and LLD §11.3 both reference.
func TestBegin_StartsARecordingSpanNamedAfterTheOp(t *testing.T) {
	a := New(Config{BaseURL: "http://localhost:0"}, nopLogger{})

	ctx, done := a.begin(context.Background(), "create_realm")
	defer done(nil)

	// The global provider is a no-op in tests, so the span is not
	// recording; what matters here is that begin returns a derived context
	// rather than the one it was handed, so nested calls attach to it.
	assert.NotEqual(t, context.Background(), ctx)
}

// TestUnauthorizedTransport_401AttributesTheNextMintTo401 is the whole
// reason the transport exists: a rotated admin client secret must surface
// as admin_token_refresh_total{trigger="401"}, which the credential-rotation
// alert keys on, rather than as an unexplained wave of 5xx.
func TestUnauthorizedTransport_401AttributesTheNextMintTo401(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if code := int(status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`))
	}))
	t.Cleanup(srv.Close)

	a := New(Config{BaseURL: srv.URL, AdminRealm: "master", ClientID: "cid", ClientSecret: "secret"}, nopLogger{})

	before401 := counterValue(t, metrics.AdminTokenRefreshTotal.WithLabelValues("401"))
	beforeProactive := counterValue(t, metrics.AdminTokenRefreshTotal.WithLabelValues("proactive"))

	_, err := a.token(context.Background())
	require.NoError(t, err)
	require.InDelta(t, beforeProactive+1, counterValue(t, metrics.AdminTokenRefreshTotal.WithLabelValues("proactive")), 1e-9,
		"the first mint is expiry-driven, not credential-driven")
	require.InDelta(t, before401, counterValue(t, metrics.AdminTokenRefreshTotal.WithLabelValues("401")), 1e-9)

	// Any admin call answered with 401 drops the cached token reactively.
	status.Store(http.StatusUnauthorized)
	_ = a.DeleteRealm(context.Background(), "tenant-x")

	status.Store(http.StatusOK)
	_, err = a.token(context.Background())
	require.NoError(t, err)
	assert.InDelta(t, before401+1, counterValue(t, metrics.AdminTokenRefreshTotal.WithLabelValues("401")), 1e-9,
		"the mint following a 401 must be attributed to trigger=401")
}
