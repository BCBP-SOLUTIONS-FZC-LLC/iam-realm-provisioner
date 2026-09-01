package eventbus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/port"
)

// TestPrependGlueHeader_ExactByteLayout locks down the AWS Glue Schema
// Registry wire format byte-for-byte (SECURITY.md/§7.3.1 territory — a
// malformed header here would make every event silently unreadable to
// every downstream consumer): byte 0 = 0x03 (version), byte 1 = 0x00 (no
// compression), bytes 2..17 = the 16-byte schema-version UUID, byte 18
// onward = the JSON payload verbatim.
func TestPrependGlueHeader_ExactByteLayout(t *testing.T) {
	schemaVersionID := "b6f8f6d0-4b1a-4b1a-8b1a-1234567890ab"
	payload := []byte(`{"tenant_id":"x"}`)

	out, err := prependGlueHeader(schemaVersionID, payload)
	require.NoError(t, err)

	require.Len(t, out, glueHeaderSize+len(payload))
	assert.Equal(t, byte(0x03), out[0], "byte 0 must be the Glue wire-format version magic byte")
	assert.Equal(t, byte(0x00), out[1], "byte 1 must be the no-compression marker")

	wantUUID := uuid.MustParse(schemaVersionID)
	assert.Equal(t, wantUUID[:], out[2:18], "bytes 2..17 must be the raw 16-byte schema-version UUID")
	assert.Equal(t, payload, out[18:], "bytes 18.. must be the JSON payload, byte-for-byte unchanged")
}

func TestPrependGlueHeader_InvalidUUIDErrors(t *testing.T) {
	_, err := prependGlueHeader("not-a-uuid", []byte(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse schema version UUID")
}

// TestStripGlueHeader_RoundTripsWithPrepend confirms strip(prepend(x)) == x
// for the payload and recovers nothing but the payload.
func TestStripGlueHeader_RoundTripsWithPrepend(t *testing.T) {
	schemaVersionID := uuid.New().String()
	payload := []byte(`{"a":1,"b":"two"}`)

	encoded, err := prependGlueHeader(schemaVersionID, payload)
	require.NoError(t, err)

	decoded, err := stripGlueHeader(encoded)
	require.NoError(t, err)
	assert.JSONEq(t, string(payload), string(decoded))
}

func TestStripGlueHeader_TooShortErrors(t *testing.T) {
	_, err := stripGlueHeader([]byte{0x03, 0x00, 0x01})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shorter than")
}

func TestStripGlueHeader_WrongVersionByteErrors(t *testing.T) {
	encoded, err := prependGlueHeader(uuid.New().String(), []byte(`{}`))
	require.NoError(t, err)
	encoded[0] = 0x99 // corrupt the version byte

	_, err = stripGlueHeader(encoded)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected header version byte")
}

// TestGlueCodec_Encode_UsesCachedVersionID exercises Encode/versionID via a
// directly-constructed GlueCodec with a pre-populated versionCache — this
// avoids needing a live (or wire-protocol-faked) AWS Glue client for the
// cache-hit path, which is what every real Encode call takes after the
// once-at-startup NewGlueCodec prefetch.
func TestGlueCodec_Encode_UsesCachedVersionID(t *testing.T) {
	versionID := uuid.New().String()
	g := &GlueCodec{
		registryName: "iam-tenant-events",
		versionCache: map[string]string{"TrialTenantProvisioned": versionID},
	}

	payload := []byte(`{"tenant_id":"x"}`)
	encoded, gotVersion, err := g.Encode(context.Background(), "TrialTenantProvisioned", payload)
	require.NoError(t, err)
	assert.Equal(t, versionID, gotVersion)
	assert.Equal(t, byte(0x03), encoded[0])

	decoded, err := g.Decode(context.Background(), "TrialTenantProvisioned", encoded)
	require.NoError(t, err)
	assert.JSONEq(t, string(payload), string(decoded))
}

// newFakeGlueServer fakes just enough of the AWS Glue JSON-1.1 protocol for
// GetSchemaVersion: a schema name present in versions succeeds with that
// version UUID; any other name (or an empty versions map) responds like a
// real registry miss (400 + EntityNotFoundException). hits, if non-nil, is
// incremented once per request so tests can assert on cache-hit behavior
// without a network round trip.
func newFakeGlueServer(t *testing.T, versions map[string]string, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		var body struct {
			SchemaID struct {
				SchemaName string `json:"SchemaName"`
			} `json:"SchemaId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		id, ok := versions[body.SchemaID.SchemaName]
		if !ok {
			w.Header().Set("X-Amzn-ErrorType", "EntityNotFoundException")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"__type":"EntityNotFoundException","Message":"schema not found"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"SchemaVersionId":"` + id + `"}`))
	}))
}

// newTestGlueClient builds a real *glue.Client pointed at a local fake
// server via BaseEndpoint — this is the seam that lets fetchVersionID's
// actual HTTP/JSON-protocol call be exercised without a live AWS Glue
// registry: NewGlueCodec and GlueCodec take a concrete *glue.Client, and the
// AWS SDK v2 wire protocol works against any http.Handler that answers the
// AWSGlue.GetSchemaVersion shape, not just the real AWS endpoint.
func newTestGlueClient(t *testing.T, url string) *glue.Client {
	t.Helper()
	return glue.New(glue.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: aws.String(url),
		Retryer:      aws.NopRetryer{},
	})
}

func TestGlueCodec_fetchVersionID_Success(t *testing.T) {
	versionID := uuid.New().String()
	srv := newFakeGlueServer(t, map[string]string{"TrialTenantProvisioned": versionID}, nil)
	defer srv.Close()

	g := &GlueCodec{client: newTestGlueClient(t, srv.URL), registryName: "iam-tenant-events"}
	got, err := g.fetchVersionID(context.Background(), "TrialTenantProvisioned")
	require.NoError(t, err)
	assert.Equal(t, versionID, got)
}

func TestGlueCodec_fetchVersionID_RegistryMissErrors(t *testing.T) {
	srv := newFakeGlueServer(t, map[string]string{}, nil)
	defer srv.Close()

	g := &GlueCodec{client: newTestGlueClient(t, srv.URL), registryName: "iam-tenant-events"}
	_, err := g.fetchVersionID(context.Background(), "Unregistered")
	require.Error(t, err)
}

func TestGlueCodec_fetchVersionID_NilSchemaVersionIDErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	g := &GlueCodec{client: newTestGlueClient(t, srv.URL), registryName: "iam-tenant-events"}
	_, err := g.fetchVersionID(context.Background(), "TrialTenantProvisioned")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil SchemaVersionId")
}

func TestNewGlueCodec_PrefetchesEveryNameOnConstruction(t *testing.T) {
	v1, v2 := uuid.New().String(), uuid.New().String()
	srv := newFakeGlueServer(t, map[string]string{"A": v1, "B": v2}, nil)
	defer srv.Close()

	c, err := NewGlueCodec(context.Background(), newTestGlueClient(t, srv.URL), "iam-tenant-events", []string{"A", "B"})
	require.NoError(t, err)
	assert.Equal(t, v1, c.versionCache["A"])
	assert.Equal(t, v2, c.versionCache["B"])
}

// TestNewGlueCodec_PrefetchFailureIsFatal locks down §"NewGlueCodec"'s
// documented contract: a schema this service expects to be registered but
// isn't must fail construction (crash the pod), never silently start up
// with a hole in versionCache.
func TestNewGlueCodec_PrefetchFailureIsFatal(t *testing.T) {
	srv := newFakeGlueServer(t, map[string]string{}, nil)
	defer srv.Close()

	_, err := NewGlueCodec(context.Background(), newTestGlueClient(t, srv.URL), "iam-tenant-events", []string{"Missing"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `prefetch glue schema "Missing"`)
}

func TestGlueCodec_WithLogger_ReturnsSameCodecForChaining(t *testing.T) {
	g := &GlueCodec{versionCache: map[string]string{}}
	got := g.WithLogger(nil)
	assert.Same(t, g, got)
}

func TestGlueCodec_versionID_CacheMissFetchesThenCachesHit(t *testing.T) {
	versionID := uuid.New().String()
	var hits int32
	srv := newFakeGlueServer(t, map[string]string{"X": versionID}, &hits)
	defer srv.Close()

	g := &GlueCodec{client: newTestGlueClient(t, srv.URL), registryName: "iam-tenant-events", versionCache: map[string]string{}}

	got, err := g.versionID(context.Background(), "X")
	require.NoError(t, err)
	assert.Equal(t, versionID, got)
	assert.EqualValues(t, 1, atomic.LoadInt32(&hits))

	got2, err := g.versionID(context.Background(), "X")
	require.NoError(t, err)
	assert.Equal(t, versionID, got2)
	assert.EqualValues(t, 1, atomic.LoadInt32(&hits), "a cache hit must not re-fetch")
}

func TestGlueCodec_versionID_CacheMissFetchErrorPropagates(t *testing.T) {
	srv := newFakeGlueServer(t, map[string]string{}, nil)
	defer srv.Close()

	g := &GlueCodec{client: newTestGlueClient(t, srv.URL), registryName: "iam-tenant-events", versionCache: map[string]string{}}
	_, err := g.versionID(context.Background(), "Missing")
	require.Error(t, err)
}

func TestGlueCodec_Encode_VersionIDLookupFailurePropagates(t *testing.T) {
	srv := newFakeGlueServer(t, map[string]string{}, nil)
	defer srv.Close()

	g := &GlueCodec{client: newTestGlueClient(t, srv.URL), registryName: "iam-tenant-events", versionCache: map[string]string{}}
	_, _, err := g.Encode(context.Background(), "Missing", []byte(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get glue schema version")
}

// TestGlueCodec_Encode_PrependHeaderFailurePropagates covers Encode's second
// error branch — a cached version string that fails to parse as a UUID
// (e.g. registry corruption) must surface, not panic.
func TestGlueCodec_Encode_PrependHeaderFailurePropagates(t *testing.T) {
	g := &GlueCodec{versionCache: map[string]string{"Bad": "not-a-uuid"}}
	_, _, err := g.Encode(context.Background(), "Bad", []byte(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse schema version UUID")
}

func TestGlueCodec_StartRefresher_UpdatesCacheOnTick(t *testing.T) {
	v1, v2 := uuid.New().String(), uuid.New().String()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		id := v1
		if n > 1 {
			id = v2
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"SchemaVersionId":"` + id + `"}`))
	}))
	defer srv.Close()

	g := &GlueCodec{
		client:       newTestGlueClient(t, srv.URL),
		registryName: "iam-tenant-events",
		versionCache: map[string]string{"X": v1},
		log:          port.NewSlogStyleLogger(nil),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.StartRefresher(ctx, 5*time.Millisecond)

	require.Eventually(t, func() bool {
		g.mu.RLock()
		defer g.mu.RUnlock()
		return g.versionCache["X"] == v2
	}, time.Second, 5*time.Millisecond, "refresher must overwrite the cache with the newly-fetched version")
}

// fakeWarnLogger implements port.Logger, recording Warn/Error calls —
// shared across this package's test files (glue refresher warnings,
// publisher enqueue-failure errors) to confirm a WithLogger-attached logger
// is actually the sink a failure path writes through, not a stand-in that's
// merely stored and never called.
type fakeWarnLogger struct {
	mu    sync.Mutex
	warn  []string
	error []string
}

func (f *fakeWarnLogger) Debug(string, map[string]any) {}
func (f *fakeWarnLogger) Info(string, map[string]any)  {}
func (f *fakeWarnLogger) Warn(msg string, _ map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.warn = append(f.warn, msg)
}
func (f *fakeWarnLogger) Error(msg string, _ map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.error = append(f.error, msg)
}

func (f *fakeWarnLogger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.warn)
}

func (f *fakeWarnLogger) errorCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.error)
}

// TestGlueCodec_StartRefresher_LogsWarnAndKeepsStaleCacheOnFetchFailure
// covers the non-fatal-refresh-error branch: a failed background refetch
// must not crash the refresher goroutine nor clobber the last-known-good
// cached version.
func TestGlueCodec_StartRefresher_LogsWarnAndKeepsStaleCacheOnFetchFailure(t *testing.T) {
	srv := newFakeGlueServer(t, map[string]string{}, nil) // every schema name 404s
	defer srv.Close()

	fl := &fakeWarnLogger{}
	g := &GlueCodec{
		client:       newTestGlueClient(t, srv.URL),
		registryName: "iam-tenant-events",
		versionCache: map[string]string{"X": "seed-version"},
		log:          port.NewSlogStyleLogger(fl),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.StartRefresher(ctx, 5*time.Millisecond)

	require.Eventually(t, func() bool { return fl.count() > 0 }, time.Second, 5*time.Millisecond)

	g.mu.RLock()
	defer g.mu.RUnlock()
	assert.Equal(t, "seed-version", g.versionCache["X"], "a failed refresh must not clobber the last-known-good cache entry")
}

// TestGlueDecodeCodec_DecodeOnly confirms GlueDecodeCodec strips headers
// without needing any Glue client/registry configuration (used by this
// service's SQS consumers to decode events from producers whose Glue
// registry this service does not have configured).
func TestGlueDecodeCodec_DecodeOnly(t *testing.T) {
	versionID := uuid.New().String()
	payload := []byte(`{"x":1}`)
	encoded, err := prependGlueHeader(versionID, payload)
	require.NoError(t, err)

	decoded, err := GlueDecodeCodec{}.Decode(context.Background(), "SomeEvent", encoded)
	require.NoError(t, err)
	assert.JSONEq(t, string(payload), string(decoded))

	_, _, err = GlueDecodeCodec{}.Encode(context.Background(), "SomeEvent", payload)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode-only")
}
