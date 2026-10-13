package keycloak

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/stretchr/testify/require"
)

// tokenEndpointServer serves gocloak's client_credentials token endpoint,
// counting requests and returning a fresh access_token each call so tests
// can assert on cache-hit vs. cache-miss behavior via the call count.
func tokenEndpointServer(t *testing.T, expiresIn int) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":%d,"token_type":"Bearer"}`, n, expiresIn)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestTokenCache_CachesUntilExpiry(t *testing.T) {
	srv, calls := tokenEndpointServer(t, 3600)
	client := gocloak.NewClient(srv.URL)
	cache := newTokenCache(client, "master", "cid", func(_ context.Context) (string, error) { return "secret", nil })

	tok1, err := cache.Token(context.Background())
	require.NoError(t, err)
	tok2, err := cache.Token(context.Background())
	require.NoError(t, err)
	require.Equal(t, tok1, tok2)
	require.EqualValues(t, 1, atomic.LoadInt32(calls), "a live cached token must not trigger a second mint")
}

func TestTokenCache_InvalidateForcesRefresh(t *testing.T) {
	srv, calls := tokenEndpointServer(t, 3600)
	client := gocloak.NewClient(srv.URL)
	cache := newTokenCache(client, "master", "cid", func(_ context.Context) (string, error) { return "secret", nil })

	tok1, err := cache.Token(context.Background())
	require.NoError(t, err)
	cache.Invalidate()
	tok2, err := cache.Token(context.Background())
	require.NoError(t, err)
	require.NotEqual(t, tok1, tok2)
	require.EqualValues(t, 2, atomic.LoadInt32(calls))
}

func TestTokenCache_ExpiredTokenTriggersRefresh(t *testing.T) {
	// expires_in=0 minus the 30s safety margin puts expiresAt already in
	// the past, so the very next Token() call must re-mint.
	srv, calls := tokenEndpointServer(t, 0)
	client := gocloak.NewClient(srv.URL)
	cache := newTokenCache(client, "master", "cid", func(_ context.Context) (string, error) { return "secret", nil })

	_, err := cache.Token(context.Background())
	require.NoError(t, err)
	_, err = cache.Token(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 2, atomic.LoadInt32(calls))
}

func TestTokenCache_SingleFlightConcurrentRefresh(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-single","expires_in":3600,"token_type":"Bearer"}`))
	}))
	t.Cleanup(srv.Close)
	client := gocloak.NewClient(srv.URL)
	cache := newTokenCache(client, "master", "cid", func(_ context.Context) (string, error) { return "secret", nil })

	const n = 5
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, err := cache.Token(context.Background())
			results[i] = tok
			errs[i] = err
		}(i)
	}
	// Give every goroutine time to observe the in-flight refresh and block
	// on it before the single mint is allowed to complete.
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&calls) == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the single in-flight refresh to start")
		case <-time.After(time.Millisecond):
		}
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i := range results {
		require.NoError(t, errs[i])
		require.Equal(t, results[0], results[i])
	}
	require.EqualValues(t, 1, atomic.LoadInt32(&calls), "a burst of concurrent Token() calls must trigger exactly one HTTP refresh")
}

func TestTokenCache_RefreshErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"bad secret"}`))
	}))
	t.Cleanup(srv.Close)
	client := gocloak.NewClient(srv.URL)
	cache := newTokenCache(client, "master", "cid", func(_ context.Context) (string, error) { return "wrong-secret", nil })

	_, err := cache.Token(context.Background())
	require.Error(t, err)
}

func TestTokenCache_ContextCanceledWhileWaitingOnInflight(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`))
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	client := gocloak.NewClient(srv.URL)
	cache := newTokenCache(client, "master", "cid", func(_ context.Context) (string, error) { return "secret", nil })

	go func() { _, _ = cache.Token(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // let the first call take the inflight slot

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := cache.Token(ctx)
	require.ErrorIs(t, err, context.Canceled)
}
