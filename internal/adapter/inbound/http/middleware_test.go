package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// testGinConfig is a minimal gincommon.Config for tests that need the real
// ProtectedMiddlewares chain (RequireAuth + ContextMiddleware) ahead of
// GUCBridgeMiddleware, matching router.go's actual wiring order.
var testGinConfig = gincommon.Config{ServiceName: "iam-realm-provisioner-test"}

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
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestParseBridgedIdentity(t *testing.T) {
	validTenant := uuid.New().String()
	validUser := uuid.New().String()

	tests := []struct {
		name    string
		in      bridgedIdentity
		wantErr bool
		wantSys bool
	}{
		{"valid user and tenant", bridgedIdentity{UserIDStr: validUser, TenantIDStr: validTenant}, false, false},
		{"system principal literal", bridgedIdentity{UserIDStr: "iam-system", TenantIDStr: validTenant}, false, true},
		{"system principal reserved uuid", bridgedIdentity{UserIDStr: systemPrincipalUserID, TenantIDStr: validTenant}, false, true},
		{"invalid user id", bridgedIdentity{UserIDStr: "not-a-uuid", TenantIDStr: validTenant}, true, false},
		{"invalid tenant id", bridgedIdentity{UserIDStr: validUser, TenantIDStr: "not-a-uuid"}, true, false},
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
			if tc.wantSys && rc.UserID != uuid.Nil {
				t.Fatalf("expected system principal to map to uuid.Nil, got %s", rc.UserID)
			}
		})
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
