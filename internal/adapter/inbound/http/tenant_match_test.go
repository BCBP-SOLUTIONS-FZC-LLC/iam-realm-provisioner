package http

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// A caller whose x-tenant-id is not the request's target tenant is
// rejected before any handler work. The fakes' Keycloak funcs are nil, so a
// bypass that reached Keycloak would panic rather than pass.
func TestRequireTenantMatch_PathTenantMismatch_Forbidden(t *testing.T) {
	d := newServiceDeps()
	pathTenant := uuid.New()
	headers := withHeader(withHeader(sysHeaders(), "Idempotency-Key", "mismatch-1"), "x-tenant-id", uuid.NewString())

	rec := doRequest(d.engine(), http.MethodPost, convertPath(pathTenant), headers, `{"plan":"pro"}`)
	assertStatusAndCode(t, rec, http.StatusForbidden, "TENANT_MISMATCH")
}

func TestRequireTenantMatch_ProvisionBodyTenantMismatch_Forbidden(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(withHeader(sysHeaders(), "Idempotency-Key", "mismatch-2"), "x-tenant-id", uuid.NewString())
	body := `{"tenant_id":"` + uuid.NewString() + `","tenant_name":"Acme","tenant_slug":"acme","admin_email":"a@example.com","admin_name":"Admin","plan":"pro"}`

	rec := doRequest(d.engine(), http.MethodPost, provisionDirectPath, headers, body)
	assertStatusAndCode(t, rec, http.StatusForbidden, "TENANT_MISMATCH")
}

func TestRequireTenantMatch_MatchingTenant_Passes(t *testing.T) {
	d := newServiceDeps()
	tenantID := uuid.New()
	headers := withHeader(sysHeaders(), "x-tenant-id", tenantID.String())

	rec := doRequest(d.engine(), http.MethodGet, "/api/v1/internal/tenants/"+tenantID.String()+"/realm", headers, "")
	if rec.Code == http.StatusForbidden {
		t.Fatalf("a matching tenant must not be rejected: %s", rec.Body.String())
	}
}

func TestRequireTenantMatch_ProvisionTrialBodyTenantMismatch_Forbidden(t *testing.T) {
	d := newServiceDeps()
	headers := withHeader(withHeader(sysHeaders(), "Idempotency-Key", "mismatch-3"), "x-tenant-id", uuid.NewString())

	rec := doRequest(d.engine(), http.MethodPost, provisionTrialPath, headers, trialBody(uuid.New()))
	assertStatusAndCode(t, rec, http.StatusForbidden, "TENANT_MISMATCH")
}

func TestTruncateReason(t *testing.T) {
	cases := []struct{ in, want string }{
		{"short", "short"},
		{"exactly10!", "exactly10!"},
		{"this reason is too long", "this reaso"},
		{"ééééééééééé", "éééééééééé"}, // counts runes, never splits a UTF-8 sequence
	}
	for _, tc := range cases {
		if got := truncateReason(tc.in, 10); got != tc.want {
			t.Errorf("truncateReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
