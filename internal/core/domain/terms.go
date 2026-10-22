package domain

import (
	"encoding/json"
	"net"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Terms-of-service acceptance (Signup BFF open gap A-4, LLD §8.1/§8.2,
// §10.6). The tenant owner accepts the terms on the signup form; RP-1/RP-2
// record that acceptance as an Audit Log direct-write entry
// (legal.terms.accepted, compliance_7y — kept 7 years and through a GDPR
// erasure), enqueued in pending_audit_entries in the same transaction as
// tenant_realms. The IP address and user agent travel point to point only
// (BFF → RP → Audit Log AL-5) and are never put on an event.
const (
	// AuditEntryTypeTermsAccepted is Audit Log's taxonomy value (Audit LLD
	// §7.1 rev 0.31). It is generic, not signup-specific: a later
	// re-acceptance of a newer version is another entry of the same type.
	AuditEntryTypeTermsAccepted = "legal.terms.accepted"
	// AuditSourceTermsAccepted is AL-5's source_event_type (provenance).
	AuditSourceTermsAccepted = "TermsAccepted"
	// AuditActionAccept is AL-5's action for an acceptance.
	AuditActionAccept = "accept"
	// AuditTargetTenant is AL-5's target.type for the tenant accepted for.
	AuditTargetTenant = "tenant"

	// TermsVersionMaxLen bounds terms_acceptance.version.
	TermsVersionMaxLen = 32
	// TermsUserAgentMaxLen bounds terms_acceptance.user_agent (bytes).
	TermsUserAgentMaxLen = 512
	// TermsAcceptedMaxAge is how old accepted_at may be when RP-1/RP-2
	// first runs: the BFF stamps it when it validates the signup form and
	// calls RP-1/RP-2 within seconds, and a retry with a fresh
	// Idempotency-Key comes well inside a day. An idempotent replay of an
	// already-completed key is answered from the ledger before this check.
	TermsAcceptedMaxAge = 24 * time.Hour
	// TermsAcceptedMaxFutureSkew tolerates clock skew between the BFF and
	// this service.
	TermsAcceptedMaxFutureSkew = 10 * time.Minute
)

// termsVersionShape is a conservative version identifier ("2026-09",
// "v3.1"): it becomes part of the AL-5 idempotency key, so no separators,
// whitespace or control characters.
var termsVersionShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// TermsAcceptance is the RP-1/RP-2 terms_acceptance object: which terms
// version the tenant owner accepted, when, and from which client.
type TermsAcceptance struct {
	Version    string
	AcceptedAt time.Time
	IPAddress  string
	UserAgent  string
}

// Validate checks a terms acceptance against now: ValidateShape, then the
// accepted_at window [now-24h, now+10m]. Every failure is a 400
// invalid_request.
func (t *TermsAcceptance) Validate(now time.Time) error {
	if err := t.ValidateShape(); err != nil {
		return err
	}
	if t.AcceptedAt.After(now.Add(TermsAcceptedMaxFutureSkew)) {
		return NewError(ErrInvalidRequest, "terms_acceptance.accepted_at is more than 10 minutes in the future")
	}
	if t.AcceptedAt.Before(now.Add(-TermsAcceptedMaxAge)) {
		return NewError(ErrInvalidRequest, "terms_acceptance.accepted_at is more than 24 hours old")
	}
	return nil
}

// ValidateShape checks everything but the time window: a missing
// acceptance, a bad version, a missing accepted_at, an invalid IP address,
// or a user agent that is empty, too long, not UTF-8 or carries control
// characters. Every failure is a 400 invalid_request.
func (t *TermsAcceptance) ValidateShape() error {
	if t == nil {
		return NewError(ErrInvalidRequest, "terms_acceptance is required")
	}
	if t.Version == "" || len(t.Version) > TermsVersionMaxLen || !termsVersionShape.MatchString(t.Version) {
		return NewError(ErrInvalidRequest, "terms_acceptance.version must be 1-32 characters of letters, digits, '.', '_' or '-'")
	}
	if t.AcceptedAt.IsZero() {
		return NewError(ErrInvalidRequest, "terms_acceptance.accepted_at is required (RFC 3339)")
	}
	if net.ParseIP(t.IPAddress) == nil {
		return NewError(ErrInvalidRequest, "terms_acceptance.ip_address must be an IPv4 or IPv6 address")
	}
	if strings.TrimSpace(t.UserAgent) == "" || len(t.UserAgent) > TermsUserAgentMaxLen {
		return NewError(ErrInvalidRequest, "terms_acceptance.user_agent is required (at most 512 bytes)")
	}
	if !utf8.ValidString(t.UserAgent) || strings.IndexFunc(t.UserAgent, unicode.IsControl) >= 0 {
		return NewError(ErrInvalidRequest, "terms_acceptance.user_agent must not contain control characters")
	}
	return nil
}

// TermsAcceptedIdempotencyKey is the deterministic AL-5 Idempotency-Key of
// one acceptance: one record per tenant, user and terms version. An RP-1
// replay, the user-exists conflict resume path or a retry with a new
// Idempotency-Key all produce the same key, so pending_audit_entries holds
// one row and Audit Log one entry — a different accepted_at for the same
// version still dedupes (the first one recorded wins).
func TermsAcceptedIdempotencyKey(tenantID, userID uuid.UUID, version string) string {
	return AuditSourceTermsAccepted + ":" + tenantID.String() + ":" + userID.String() + ":" + version
}

// TermsAcceptedAuditEntry builds the legal.terms.accepted AL-5 entry for
// the tenant owner (userID, the Keycloak sub). signupType and plan are
// context for the record; traceID ties it to the signup request, since
// delivery happens later from the drainer.
func TermsAcceptedAuditEntry(tenantID, userID uuid.UUID, t TermsAcceptance, signupType, plan, traceID string) AuditEntry {
	acceptedAt := t.AcceptedAt.UTC()
	metadata, _ := json.Marshal(map[string]string{ //nolint:errcheck // a map of strings always marshals
		"terms_version": t.Version,
		"accepted_at":   acceptedAt.Format(time.RFC3339Nano),
		"signup_type":   signupType,
		"plan":          plan,
	})
	return AuditEntry{
		TenantID:        tenantID,
		EntryType:       AuditEntryTypeTermsAccepted,
		Action:          AuditActionAccept,
		Actor:           AuditActor{Type: AuditActorUser, ID: userID.String()},
		Target:          &AuditTarget{Type: AuditTargetTenant, ID: tenantID.String()},
		OccurredAt:      acceptedAt,
		SourceService:   Source,
		SourceEventType: AuditSourceTermsAccepted,
		IPAddress:       t.IPAddress,
		UserAgent:       t.UserAgent,
		TraceID:         traceID,
		Metadata:        metadata,
	}
}
