package http

import (
	"context"
	"errors"
	"net/http"
	"testing"
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
