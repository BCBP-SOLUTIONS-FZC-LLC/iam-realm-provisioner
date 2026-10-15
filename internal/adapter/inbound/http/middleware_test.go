package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// testGinConfig is a minimal gincommon.Config for tests that need the real
// ProtectedMiddlewares chain (RequireAuth + ContextMiddleware) ahead of
// GUCBridgeMiddleware, matching router.go's actual wiring order.
// testGinConfig carries the same identity as TestMain's (BuildVersion
// included): gincommon resolves an empty BuildVersion from BUILD_VERSION —
// which `make test` exports — and rejects a second identity in one process.
var testGinConfig = gincommon.Config{Domain: "iam", Environment: "test", ServiceName: "iam-realm-provisioner", BuildVersion: "test"}

func init() { gin.SetMode(gin.TestMode) }

// newTestEngine builds a bare Gin engine with a single terminal handler
// wired behind the given middleware chain, for isolated middleware tests.
func newTestEngine(mw ...gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	chain := append(append([]gin.HandlerFunc{}, mw...), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	r.Any("/probe", chain...)
	return r
}

func doRequest(r http.Handler, method, path string, headers map[string]string, body string) *httptest.ResponseRecorder {
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	if headers["x-tenant-id"] == targetTenant {
		headers = resolveTargetTenant(headers, path, body)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestCommercialActorIDLiteralValue asserts the exact canonical UUID value of
// domain.CommercialActorID so that any accidental drift (e.g. a merge that
// reverts the a2→c1 change) is caught immediately. CD-19 alignment rule 1.
func TestCommercialActorIDLiteralValue(t *testing.T) {
	const want = "00000000-0000-0000-0000-0000000000c1"
	if domain.CommercialActorID != want {
		t.Fatalf("CommercialActorID = %q, want %q (CD-19 canonical Core value)", domain.CommercialActorID, want)
	}
}

func TestParseBridgedIdentity(t *testing.T) {
	validTenant := uuid.New().String()
	validUser := uuid.New().String()
	commercialUUID := uuid.MustParse(domain.CommercialActorID)

	tests := []struct {
		name       string
		in         bridgedIdentity
		wantErr    bool
		wantUserID *uuid.UUID // nil = not checked; &uuid.Nil = must be nil; &commercialUUID = exact match
	}{
		{"valid user and tenant", bridgedIdentity{UserIDStr: validUser, TenantIDStr: validTenant}, false, nil},
		// iam-system → uuid.Nil (§18.3)
		{"iam-system literal", bridgedIdentity{UserIDStr: "iam-system", TenantIDStr: validTenant}, false, func() *uuid.UUID { n := uuid.Nil; return &n }()},
		{"iam-system reserved uuid", bridgedIdentity{UserIDStr: domain.SystemActorID, TenantIDStr: validTenant}, false, func() *uuid.UUID { n := uuid.Nil; return &n }()},
		// CD-19: commercial-system → CommercialActorID (NOT uuid.Nil — unlike iam-system)
		{"commercial-system literal", bridgedIdentity{UserIDStr: "commercial-system", TenantIDStr: validTenant}, false, &commercialUUID},
		{"commercial-system reserved uuid", bridgedIdentity{UserIDStr: domain.CommercialActorID, TenantIDStr: validTenant}, false, &commercialUUID},
		{"invalid user id", bridgedIdentity{UserIDStr: "not-a-uuid", TenantIDStr: validTenant}, true, nil},
		{"invalid tenant id", bridgedIdentity{UserIDStr: validUser, TenantIDStr: "not-a-uuid"}, true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc, errMsg := parseBridgedIdentity(tc.in)
			if tc.wantErr {
				if errMsg == "" {
					t.Fatalf("expected an error message, got none")
				}
				if rc != nil {
					t.Fatalf("expected nil RequestContext on error, got %+v", rc)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected error: %s", errMsg)
			}
			if rc == nil {
				t.Fatal("expected non-nil RequestContext")
			}
			if tc.wantUserID != nil && rc.UserID != *tc.wantUserID {
				t.Fatalf("UserID = %s, want %s", rc.UserID, *tc.wantUserID)
			}
		})
	}
}

// TestGUCBridgeMiddleware_CommercialSystem_SetsActorUUID verifies that the
// GUCBridgeMiddleware populates requestctx.UserID with the canonical
// CommercialActorID UUID (not uuid.Nil) when the x-user-id header carries
// the commercial-system principal. This is the CD-19 audit-actor fix: audit
// entries and log lines reading rc.UserID will see the correct actor.
func TestGUCBridgeMiddleware_CommercialSystem_SetsActorUUID(t *testing.T) {
	commercialUUID := uuid.MustParse(domain.CommercialActorID)
	tenantID := uuid.New()
	var gotRC *requestctx.RequestContext
	r := gin.New()
	r.Use(gincommon.ProtectedMiddlewares(testGinConfig)...)
	r.Use(GUCBridgeMiddleware())
	r.GET("/probe", func(c *gin.Context) {
		rc, _ := requestctx.FromContext(c.Request.Context())
		gotRC = rc
		c.Status(http.StatusOK)
	})
	// Use the canonical UUID form (as the gateway sends it once it resolves
	// the principal — either form is accepted by parseBridgedIdentity).
	rec := doRequest(r, http.MethodGet, "/probe", map[string]string{
		"x-user-id": domain.CommercialActorID, "x-tenant-id": tenantID.String(), "x-tenant-roles": "commercial-system",
	}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotRC == nil {
		t.Fatal("requestctx not populated")
	}
	if gotRC.UserID != commercialUUID {
		t.Fatalf("UserID = %s, want CommercialActorID %s (CD-19 audit actor must not be uuid.Nil)", gotRC.UserID, commercialUUID)
	}
	if gotRC.UserID == uuid.Nil {
		t.Fatal("UserID must not be uuid.Nil for commercial-system (iam-system stays nil; commercial-system gets its canonical UUID)")
	}
}

// ── RequireSystemOrCommercial — CD-19 allow-list ─────────────────────────

// TestRequireSystemOrCommercial_AllowedPrincipals verifies that iam-system
// AND commercial-system both pass the CD-19 allow-list middleware.
func TestRequireSystemOrCommercial_AllowedPrincipals(t *testing.T) {
	newEngine := func() *gin.Engine {
		r := gin.New()
		r.Use(gincommon.ProtectedMiddlewares(testGinConfig)...)
		r.Use(GUCBridgeMiddleware())
		r.GET("/probe", RequireSystemOrCommercial(), func(c *gin.Context) {
			c.Status(http.StatusOK)
		})
		return r
	}
	for _, tc := range []struct {
		name   string
		userID string
		roles  string
	}{
		{"iam-system", domain.SystemActorID, "iam-system"},
		{"commercial-system literal userID", "commercial-system", "commercial-system"},
		{"commercial-system uuid userID", domain.CommercialActorID, "commercial-system"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tenantID := uuid.New()
			rec := doRequest(newEngine(), http.MethodGet, "/probe", map[string]string{
				"x-user-id": tc.userID, "x-tenant-id": tenantID.String(), "x-tenant-roles": tc.roles,
			}, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestRequireSystemOrCommercial_RejectsOtherCallers verifies that an unknown
// role gets 403.
func TestRequireSystemOrCommercial_RejectsOtherCallers(t *testing.T) {
	r := gin.New()
	r.Use(gincommon.ProtectedMiddlewares(testGinConfig)...)
	r.Use(GUCBridgeMiddleware())
	r.GET("/probe", RequireSystemOrCommercial(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	tenantID := uuid.New()
	rec := doRequest(r, http.MethodGet, "/probe", map[string]string{
		"x-user-id": uuid.New().String(), "x-tenant-id": tenantID.String(), "x-tenant-roles": "tenant_owner",
	}, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body.Error != "insufficient_role" {
		t.Fatalf("expected error=insufficient_role, got %q", body.Error)
	}
}

func TestGUCBridgeMiddleware(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()

	t.Run("no platform request context — passes through untouched", func(t *testing.T) {
		r := newTestEngine(GUCBridgeMiddleware())
		rec := doRequest(r, http.MethodGet, "/probe", nil, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("valid identity populates requestctx and GUC", func(t *testing.T) {
		var gotRC *requestctx.RequestContext
		var gotGUC pgdomain.GUCSet
		r := gin.New()
		r.Use(gincommon.ProtectedMiddlewares(testGinConfig)...)
		r.Use(GUCBridgeMiddleware())
		r.GET("/probe", func(c *gin.Context) {
			rc, _ := requestctx.FromContext(c.Request.Context())
			gotRC = rc
			gotGUC, _ = pgcommon.GUCSetFromContext(c.Request.Context())
			c.Status(http.StatusOK)
		})
		rec := doRequest(r, http.MethodGet, "/probe", map[string]string{
			"x-user-id": userID.String(), "x-tenant-id": tenantID.String(), "x-tenant-roles": "iam-system",
		}, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if gotRC == nil {
			t.Fatal("expected requestctx to be populated")
		}
		if gotRC.TenantID != tenantID {
			t.Fatalf("expected tenant id %s, got %s", tenantID, gotRC.TenantID)
		}
		if !gotRC.IsSystem() {
			t.Fatalf("expected iam-system role to be recognized, got roles %v", gotRC.Roles)
		}
		if gotGUC.TenantID != tenantID.String() {
			t.Fatalf("expected GUC tenant id %s, got %s", tenantID, gotGUC.TenantID)
		}
	})

	t.Run("missing identity headers rejected upstream by RequireAuth", func(t *testing.T) {
		r := gin.New()
		r.Use(gincommon.ProtectedMiddlewares(testGinConfig)...)
		r.Use(GUCBridgeMiddleware())
		r.GET("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
		rec := doRequest(r, http.MethodGet, "/probe", nil, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("malformed user id aborts with 401 from GUCBridgeMiddleware itself", func(t *testing.T) {
		r := gin.New()
		r.Use(gincommon.ProtectedMiddlewares(testGinConfig)...)
		r.Use(GUCBridgeMiddleware())
		r.GET("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
		rec := doRequest(r, http.MethodGet, "/probe", map[string]string{
			"x-user-id": "not-a-uuid", "x-tenant-id": tenantID.String(),
		}, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestRequireJSONContentType(t *testing.T) {
	r := newTestEngine(RequireJSONContentType())

	tests := []struct {
		name        string
		method      string
		contentType string
		body        string
		wantStatus  int
	}{
		{"GET is exempt", http.MethodGet, "", "", http.StatusOK},
		{"POST with json passes", http.MethodPost, "application/json", `{}`, http.StatusOK},
		{"POST with empty body passes regardless of content type", http.MethodPost, "", "", http.StatusOK},
		{"POST with wrong content type rejected", http.MethodPost, "text/plain", "hello", http.StatusUnsupportedMediaType},
		{"PUT with wrong content type rejected", http.MethodPut, "text/plain", "hello", http.StatusUnsupportedMediaType},
		{"PATCH with json passes", http.MethodPatch, "application/json", `{}`, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if tc.contentType != "" {
				headers["Content-Type"] = tc.contentType
			}
			rec := doRequest(r, tc.method, "/probe", headers, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tc.wantStatus, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRequireSystemRole(t *testing.T) {
	tests := []struct {
		name       string
		rc         *requestctx.RequestContext
		wantStatus int
	}{
		{"no request context", nil, http.StatusForbidden},
		{"missing role", &requestctx.RequestContext{Roles: []string{"platform_operator"}}, http.StatusForbidden},
		{"has iam-system role", &requestctx.RequestContext{Roles: []string{"iam-system"}}, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.Use(func(c *gin.Context) {
				if tc.rc != nil {
					c.Request = c.Request.WithContext(requestctx.WithContext(c.Request.Context(), tc.rc))
				}
				c.Next()
			})
			r.Use(RequireSystemRole())
			r.GET("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
			rec := doRequest(r, http.MethodGet, "/probe", nil, "")
			if rec.Code != tc.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tc.wantStatus, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRequireOperatorRole(t *testing.T) {
	tests := []struct {
		name       string
		rc         *requestctx.RequestContext
		wantStatus int
	}{
		{"no request context", nil, http.StatusForbidden},
		{"missing role", &requestctx.RequestContext{Roles: []string{"iam-system"}}, http.StatusForbidden},
		{"has platform_operator role", &requestctx.RequestContext{Roles: []string{"platform_operator"}}, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.Use(func(c *gin.Context) {
				if tc.rc != nil {
					c.Request = c.Request.WithContext(requestctx.WithContext(c.Request.Context(), tc.rc))
				}
				c.Next()
			})
			r.Use(RequireOperatorRole())
			r.GET("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
			rec := doRequest(r, http.MethodGet, "/probe", nil, "")
			if rec.Code != tc.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tc.wantStatus, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRequireIdempotencyKey(t *testing.T) {
	r := newTestEngine(RequireIdempotencyKey())

	tests := []struct {
		name       string
		key        string
		wantStatus int
	}{
		{"missing key rejected", "", http.StatusBadRequest},
		{"valid key passes", "a-valid-key", http.StatusOK},
		{"too-long key rejected", strings.Repeat("a", 257), http.StatusBadRequest},
		{"exactly 256 chars passes", strings.Repeat("a", 256), http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if tc.key != "" {
				headers["Idempotency-Key"] = tc.key
			}
			rec := doRequest(r, http.MethodPost, "/probe", headers, "")
			if rec.Code != tc.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tc.wantStatus, rec.Code, rec.Body.String())
			}
		})
	}
}

// silence unused-import warning if context stops being referenced directly
var _ = context.Background

var pathTenant = regexp.MustCompile(`/tenants/([0-9a-fA-F-]{36})(?:/|$)`)

// resolveTargetTenant returns a copy of headers with the targetTenant
// placeholder replaced by the request's target tenant: the /tenants/:id
// path segment, else the JSON body's tenant_id, else a fresh UUID (a route
// with no tenant, e.g. RP-13).
func resolveTargetTenant(headers map[string]string, path, body string) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		out[k] = v
	}
	if m := pathTenant.FindStringSubmatch(path); m != nil {
		out["x-tenant-id"] = m[1]
		return out
	}
	var b struct {
		TenantID string `json:"tenant_id"`
	}
	if json.Unmarshal([]byte(body), &b) == nil && uuid.Validate(b.TenantID) == nil {
		out["x-tenant-id"] = b.TenantID
		return out
	}
	out["x-tenant-id"] = uuid.NewString()
	return out
}
