package http

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// cd19CommercialRoutes is the CD-19 app-layer allow-list for the
// commercial-system principal (Billing): RP-3, RP-4 and RP-11. It must match
// the Istio AuthorizationPolicy's Billing rule, which
// .github/scripts/check-cd19-access-matrix.py asserts against the rendered
// prod and staging charts (and against router.go) in CI.
var cd19CommercialRoutes = map[string]bool{
	"POST /api/v1/internal/tenants/:id/convert":           true, // RP-3
	"POST /api/v1/internal/tenants/:id/revert-conversion": true, // RP-4
	"GET /api/v1/internal/tenants/:id/realm":              true, // RP-11
}

// TestCD19_CommercialSystem_RouteMatrix walks EVERY registered
// /api/v1/internal route and calls it as commercial-system: exactly the
// three allow-listed routes get past the role gate; every other route is
// 403 INSUFFICIENT_ROLE. A route added to the commercial group, or a role
// gate loosened, fails here even if no per-route test exists for it.
func TestCD19_CommercialSystem_RouteMatrix(t *testing.T) {
	d := newServiceDeps()
	router := NewRouter(RouterConfig{GinConfig: testGinConfig, Handlers: d.handler()})

	reached := map[string]bool{}
	internalRoutes := 0
	for _, rt := range router.engine.Routes() {
		if !strings.HasPrefix(rt.Path, "/api/v1/internal/") {
			continue
		}
		internalRoutes++
		key := rt.Method + " " + rt.Path
		path := concretePath(rt.Path)
		headers := withHeader(commercialHeaders(), "Idempotency-Key", "cd19-matrix-"+uuid.NewString())
		rec := doRequest(router.Handler(), rt.Method, path, headers, "{}")

		denied := rec.Code == http.StatusForbidden && decodeError(t, rec).ErrorCode == "INSUFFICIENT_ROLE"
		if cd19CommercialRoutes[key] {
			if denied {
				t.Errorf("%s: commercial-system must pass the role gate (CD-19 allow-list), got 403 INSUFFICIENT_ROLE", key)
			}
			reached[key] = true
			continue
		}
		if !denied {
			t.Errorf("%s: commercial-system must get 403 INSUFFICIENT_ROLE (not on the CD-19 allow-list), got %d: %s",
				key, rec.Code, rec.Body.String())
		}
	}

	if internalRoutes == 0 {
		t.Fatal("no /api/v1/internal routes found — the walk is broken")
	}
	for key := range cd19CommercialRoutes {
		if !reached[key] {
			t.Errorf("%s: allow-listed route is not registered", key)
		}
	}
}

// concretePath fills every :param segment with a UUID (every path parameter
// here is a tenant id, a Keycloak user id or an IdP alias; a UUID is a valid
// value for all three).
func concretePath(pattern string) string {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ":") || strings.HasPrefix(p, "*") {
			parts[i] = uuid.NewString()
		}
	}
	return strings.Join(parts, "/")
}
