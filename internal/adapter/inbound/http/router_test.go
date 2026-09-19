package http

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func newHealthRouter(pg, sysPg, outbox, kc Pinger) http.Handler {
	return NewRouter(RouterConfig{
		GinConfig: testGinConfig,
		Docs:      DocsConfig{Environment: "test", Enabled: true},
		Handlers:  newServiceDeps().handler(),

		Postgres:    pg,
		SysPostgres: sysPg,
		Outbox:      outbox,
		Keycloak:    kc,
	}).Handler()
}

func healthyPinger() *fakePinger {
	return &fakePinger{HealthFn: func(ctx context.Context) error { return nil }}
}

func downPinger() *fakePinger {
	return &fakePinger{HealthFn: func(ctx context.Context) error { return errors.New("down") }}
}

func TestHealthz(t *testing.T) {
	// healthz is a pure liveness probe — never inspects dependencies, so
	// even all-nil pingers must return 200.
	r := newHealthRouter(nil, nil, nil, nil)
	rec := doRequest(r, http.MethodGet, "/healthz", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	decodeInto(t, rec, &body)
	if body["status"] != "ok" {
		t.Fatalf("expected status ok, got %+v", body)
	}
}

func TestReadyz_AllHealthy(t *testing.T) {
	r := newHealthRouter(healthyPinger(), healthyPinger(), healthyPinger(), healthyPinger())
	rec := doRequest(r, http.MethodGet, "/readyz", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	decodeInto(t, rec, &body)
	if body.Status != "ready" {
		t.Fatalf("expected ready, got %+v", body)
	}
	for _, name := range []string{"database", "sys_database", "outbox", "keycloak"} {
		if body.Checks[name] != "ok" {
			t.Fatalf("expected %s ok, got %+v", name, body.Checks)
		}
	}
}

func TestReadyz_SysPostgresNilIsSkipped(t *testing.T) {
	r := newHealthRouter(healthyPinger(), nil, healthyPinger(), healthyPinger())
	rec := doRequest(r, http.MethodGet, "/readyz", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Checks map[string]string `json:"checks"`
	}
	decodeInto(t, rec, &body)
	if _, ok := body.Checks["sys_database"]; ok {
		t.Fatalf("expected sys_database to be omitted when nil, got %+v", body.Checks)
	}
}

func TestReadyz_EachDependencyDown(t *testing.T) {
	tests := []struct {
		name       string
		down       string // which of database/sys_database/outbox/keycloak is down
		checkedKey string
	}{
		{"postgres down", "database", "database"},
		{"sys postgres down", "sys_database", "sys_database"},
		{"outbox down", "outbox", "outbox"},
		{"keycloak down", "keycloak", "keycloak"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pick := func(name string) Pinger {
				if name == tc.down {
					return downPinger()
				}
				return healthyPinger()
			}
			r := newHealthRouter(pick("database"), pick("sys_database"), pick("outbox"), pick("keycloak"))
			rec := doRequest(r, http.MethodGet, "/readyz", nil, "")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Status string            `json:"status"`
				Checks map[string]string `json:"checks"`
			}
			decodeInto(t, rec, &body)
			if body.Status != "not ready" {
				t.Fatalf("expected not ready, got %+v", body)
			}
			if body.Checks[tc.checkedKey] != "down" {
				t.Fatalf("expected %s down, got %+v", tc.checkedKey, body.Checks)
			}
		})
	}
}

// The 4 dependency checks must run concurrently, not sequentially — 4
// pingers each sleeping ~50ms must complete well under 4x that (200ms),
// proving they overlap rather than queue behind one another.
func TestReadyz_ChecksRunConcurrently(t *testing.T) {
	slowPinger := func() *fakePinger {
		return &fakePinger{HealthFn: func(ctx context.Context) error {
			time.Sleep(50 * time.Millisecond)
			return nil
		}}
	}
	r := newHealthRouter(slowPinger(), slowPinger(), slowPinger(), slowPinger())

	start := time.Now()
	rec := doRequest(r, http.MethodGet, "/readyz", nil, "")
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if elapsed >= 150*time.Millisecond {
		t.Fatalf("expected concurrent checks to finish well under 4x50ms, took %s", elapsed)
	}
}

func TestDocsRoutes_ActiveOutsideProduction(t *testing.T) {
	r := NewRouter(RouterConfig{
		GinConfig: testGinConfig,
		Docs:      DocsConfig{Environment: "staging"},
		Handlers:  newServiceDeps().handler(),
	}).Handler()

	rec := doRequest(r, http.MethodGet, "/asyncapi", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(r, http.MethodGet, "/asyncapi.yaml", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDocsRoutes_DisabledInProduction(t *testing.T) {
	r := NewRouter(RouterConfig{
		GinConfig: testGinConfig,
		Docs:      DocsConfig{Environment: "production", Enabled: false},
		Handlers:  newServiceDeps().handler(),
	}).Handler()

	rec := doRequest(r, http.MethodGet, "/asyncapi", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDocsRoutes_ProductionAuthToken(t *testing.T) {
	r := NewRouter(RouterConfig{
		GinConfig: testGinConfig,
		Docs:      DocsConfig{Environment: "production", Enabled: true, AuthToken: "s3cr3t"},
		Handlers:  newServiceDeps().handler(),
	}).Handler()

	t.Run("missing bearer token rejected", func(t *testing.T) {
		rec := doRequest(r, http.MethodGet, "/asyncapi", nil, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("wrong bearer token rejected", func(t *testing.T) {
		rec := doRequest(r, http.MethodGet, "/asyncapi", map[string]string{"Authorization": "Bearer wrong"}, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("correct bearer token passes", func(t *testing.T) {
		rec := doRequest(r, http.MethodGet, "/asyncapi", map[string]string{"Authorization": "Bearer s3cr3t"}, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// The /swagger/*any route dispatches three ways by path suffix — exercise
// all three so registerDocsRoutes' routing switch is fully covered, not
// just the plain-stdSwagger default branch.
func TestDocsRoutes_SwaggerAssetRouting(t *testing.T) {
	r := NewRouter(RouterConfig{
		GinConfig: testGinConfig,
		Docs:      DocsConfig{Environment: "staging"},
		Handlers:  newServiceDeps().handler(),
	}).Handler()

	t.Run("theme css", func(t *testing.T) {
		rec := doRequest(r, http.MethodGet, "/swagger/index.css", nil, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
			t.Fatalf("expected the branded CSS handler, got Content-Type %q", ct)
		}
	})
	t.Run("initializer js", func(t *testing.T) {
		rec := doRequest(r, http.MethodGet, "/swagger/swagger-initializer.js", nil, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/javascript; charset=utf-8" {
			t.Fatalf("expected the initializer JS handler, got Content-Type %q", ct)
		}
	})
	t.Run("default falls through to stdSwagger", func(t *testing.T) {
		rec := doRequest(r, http.MethodGet, "/swagger/doc.json", nil, "")
		if rec.Code == http.StatusNotFound {
			t.Fatalf("expected the standard swagger handler to serve doc.json, got 404")
		}
	})
}

func TestDocsRoutes_ProductionNoAuthToken_OpenAccess(t *testing.T) {
	r := NewRouter(RouterConfig{
		GinConfig: testGinConfig,
		Docs:      DocsConfig{Environment: "production", Enabled: true, AuthToken: ""},
		Handlers:  newServiceDeps().handler(),
	}).Handler()

	rec := doRequest(r, http.MethodGet, "/asyncapi", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (no auth token configured means open access), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRouter_UnknownRouteIs404(t *testing.T) {
	r := newServiceDeps().engine()
	rec := doRequest(r, http.MethodGet, "/nope", nil, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRouter_WrongMethodIs405(t *testing.T) {
	r := newServiceDeps().engine()
	// /healthz only registers GET.
	rec := doRequest(r, http.MethodPost, "/healthz", nil, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRequireSystemRole_GatesInternalRoutes(t *testing.T) {
	// A caller lacking iam-system is rejected by RequireSystemRole before
	// any handler — and therefore before any fake dependency — is reached.
	// Every relevant fake Fn is left nil (fakeKeycloakAdmin, etc.) so a
	// bypass would panic rather than silently succeed.
	d := newServiceDeps()
	r := d.engine()
	headers := sysHeaders()
	headers["x-tenant-roles"] = "some-other-role"
	rec := doRequest(r, http.MethodGet, "/api/v1/internal/tenants/"+"11111111-1111-1111-1111-111111111111"+"/realm", headers, "")
	assertStatusAndCode(t, rec, http.StatusForbidden, "insufficient_role")
}
