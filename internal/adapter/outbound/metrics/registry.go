package metrics

// RegistryStatus is this service's local mirror of a metric's status in
// the Platform Observability Registry — the Enterprise Platform
// Observability Standard's governance body for every platform_* and
// domain-shared metric name and label vocabulary (rules 9-12: shared
// names/labels are part of the observability contract; a service SHALL
// NOT invent or adopt one unilaterally). There is no live registry
// service this repo can call, so PlatformRegistry below — including each
// entry's full ratification packet (semantic definition, label
// vocabulary and allowed values, cardinality justification, aggregation
// expectations) — is the checked-in source of truth this service's own
// CI enforces itself against.
//
// Ratification checklist (for the reviewing body, once one exists):
//   - Semantic definition holds identically across at least two domains
//     (not just IAM) for the metric under review.
//   - The label vocabulary (RegistryEntry.RequiredLabels/ApprovedLabels/
//     LabelValues) is acceptable as canonical, or amended with a
//     documented reason.
//   - No approved label is high-cardinality (user_id/email/tenant_id/
//     request_id/event_id/session_id or an equivalent unbounded value) —
//     every entry below is reviewed against this before being added.
//   - Aggregation expectations are validated against at least one other
//     service's real usage before Canonical status is granted — a single
//     submitter's shape may not generalize.
//   - Once approved: flip the entry's Status to StatusCanonical, then
//     follow the standard's Backward Compatibility migration order
//     (dashboards → alerts → recording rules → SLOs → HPA → deprecate the
//     Tier 3 predecessor → remove after the approved sunset period).
type RegistryStatus string

const (
	// StatusCanonical marks a metric already named as a worked Tier 1/2
	// example in the standard itself, or one the registry has ratified.
	// Safe to adopt as an alerting/dashboard/recording-rule/SLO/HPA
	// source.
	StatusCanonical RegistryStatus = "canonical"

	// StatusProposed marks a metric submitted for ratification but not
	// yet approved. Per rule 12, a Proposed metric MUST be shadow-emitted
	// only — no alert, dashboard, recording rule, SLO, or HPA reference
	// in this repo may treat it as authoritative until its status here
	// flips to Canonical following real governance approval.
	StatusProposed RegistryStatus = "proposed"
)

// RegistryEntry documents one platform_*/domain-shared metric this service
// emits or proposes, carrying the exact information the standard's
// Registry Ratification Requirement calls for (semantic definition,
// required labels, allowed label values, aggregation expectations) so a
// reviewer — human or CI — can check actual instrumentation against it
// without re-deriving intent from the Go source.
type RegistryEntry struct {
	// Name is the exact metric name as registered with Prometheus.
	Name string

	// Tier is "platform" (Tier 1) or "domain" (Tier 2). Tier 3
	// (service-specific) metrics carry no registry entry — the standard
	// only governs shared names.
	Tier string

	Status RegistryStatus

	// SemanticDefinition is what this metric means, in domain-neutral
	// terms — must hold regardless of which service or domain emits it.
	SemanticDefinition string

	// RequiredLabels are the standard's mandated const labels for this
	// tier: {domain, service, environment} for Tier 1, {service,
	// environment} for Tier 2.
	RequiredLabels []string

	// ApprovedLabels are the additional dynamic labels this entry's
	// packet requests approval for, beyond RequiredLabels. A label not
	// listed here or in RequiredLabels is not part of this metric's
	// approved vocabulary (label governance: labels are part of the API
	// contract).
	ApprovedLabels []string

	// LabelValues documents, for each ApprovedLabel, the allowed value
	// set this submission proposes (label governance: allowed values are
	// part of the contract, not just the label name). Keyed by label
	// name; a label with an open-ended value set (still bounded, just
	// not fully enumerable here) says so in its own entry instead of
	// listing every value.
	LabelValues map[string][]string

	// Cardinality justifies why this metric's label set is safe under
	// the standard's high-cardinality prohibition (no user_id/email/
	// tenant_id/request_id/event_id/session_id or equivalent unbounded
	// value as a label).
	Cardinality string

	// AggregationNotes states how the metric is expected to be queried
	// across services/domains, so cross-service aggregation stays valid.
	AggregationNotes string

	// SupersedesTier3 names the pre-existing Tier 3 metric(s) this
	// service already emits that cover the same signal today and that
	// remain the authoritative alerting/SLO source until Status flips to
	// Canonical. Empty when there is no Tier 3 predecessor.
	SupersedesTier3 []string
}

// PlatformRegistry lists every platform_*/domain-shared metric this
// service emits or has proposed. All three entries here are this
// service's own Registry Ratification Requirement submissions — none is
// yet Canonical, so none is wired into this repo's alerts, recording
// rules, SLOs, or HPA references (see each metric's doc comment in
// metrics.go and every alert/SLO file's header comment for the same
// note). Flip an entry's Status to StatusCanonical only once real
// observability governance has actually approved it, and only then
// proceed with the standard's Backward Compatibility migration steps
// (dashboards → alerts → recording rules → SLOs → HPA → deprecate →
// remove after sunset).
var PlatformRegistry = []RegistryEntry{
	{
		Name:               "platform_dependency_request_seconds",
		Tier:               "platform",
		Status:             StatusProposed,
		SemanticDefinition: "Latency of one outbound call this service makes to a dependency it calls synchronously (a REST/gRPC/admin-API call — not a queue send/receive, which platform-events' own events_* metrics already cover). `outcome` distinguishes a failed call from a successful one, so failure rate is derivable from this histogram's sample counts alone; no separate failure counter is needed. Named explicitly in the standard's own Registry-Proposed Examples.",
		RequiredLabels:     []string{"domain", "service", "environment"},
		ApprovedLabels:     []string{"dependency", "operation", "outcome"},
		LabelValues: map[string][]string{
			"dependency": {"keycloak"}, // this service currently calls only Keycloak's Admin API through this port
			"operation":  {"open — the frozen (§25) Keycloak Admin API operation names this service already uses as its `op` label today, e.g. create_realm, migrate_user, token"},
			"outcome":    {"success", "failure"},
		},
		Cardinality:      "Bounded — dependency × operation × outcome is a small, enumerable set per service; no request/session/tenant identifier is ever a label value.",
		AggregationNotes: "Per-dependency failure rate: sum(rate(platform_dependency_request_seconds_count{dependency=\"X\",outcome=\"failure\"}[w])) / sum(rate(platform_dependency_request_seconds_count{dependency=\"X\"}[w])). Cross-domain dependency-health board: sum by (domain, dependency) rate(..._count[w]). p99 latency per dependency: histogram_quantile(0.99, sum by (le, dependency) (rate(..._bucket[w]))).",
		SupersedesTier3:  []string{"iam_realm_provisioner_keycloak_call_duration_seconds", "iam_realm_provisioner_keycloak_call_failures_total"},
	},
	{
		Name:               "platform_duplicate_messages_total",
		Tier:               "platform",
		Status:             StatusProposed,
		SemanticDefinition: "A message a consumer received that its own dedup store (however implemented — a processed-events table, an idempotency cache, etc.) had already marked processed: a redelivery, not a new logical message. Independent of transport (SQS/SNS/Kafka) or domain. Named explicitly in the standard's own Registry-Proposed Examples.",
		RequiredLabels:     []string{"domain", "service", "environment"},
		ApprovedLabels:     []string{"consumer"},
		LabelValues: map[string][]string{
			"consumer": {"membership", "tenant", "billing"}, // this service's frozen (§25) processed_events.consumer values
		},
		Cardinality:      "Bounded — one value per consumer this service registers (currently 3); no message/event identifier is ever a label value.",
		AggregationNotes: "Platform-wide redelivery-rate board: sum by (domain, service) rate(platform_duplicate_messages_total[w]). A sustained nonzero rate across many unrelated services signals a shared upstream cause (e.g. a producer retry storm or a broker redelivery-timeout misconfiguration), not a per-service bug — the reason this is worth a platform_* name rather than staying per-service.",
		SupersedesTier3:  []string{"iam_realm_provisioner_processed_events_duplicates_total"},
	},
	{
		Name:               "platform_rls_violations_total",
		Tier:               "platform",
		Status:             StatusProposed,
		SemanticDefinition: "A Postgres query that a Row-Level-Security policy blocked or that ran with a missing/invalid tenant-scoping session GUC bound, for any service enforcing multi-tenant isolation via Postgres RLS — independent of domain. NOT one of the standard's own named canonical/registry-proposed examples; submitted by iam-realm-provisioner as an additional proposal on the argument that RLS-based tenant isolation is a platform-wide pattern (any FORCE ROW LEVEL SECURITY service could emit this identically), not an IAM-specific concept, and therefore warrants the same governance review as the standard's own pre-named examples before adoption — this is why it needs its own justification distinct from the two metrics above, which the standard already names as anticipated candidates.",
		RequiredLabels:     []string{"domain", "service", "environment"},
		ApprovedLabels:     []string{"violation_type"},
		LabelValues: map[string][]string{
			"violation_type": {"cross_tenant_access", "missing_or_invalid_guc"}, // cross_tenant_access: a valid tenant GUC pointed at a different tenant's row (critical, page-worthy); missing_or_invalid_guc: no/invalid tenant GUC bound before a tenant-scoped query (an application bug)
		},
		Cardinality:      "Bounded — a fixed, small enum (this submission proposes exactly the two values above); a service proposing additional violation_type values in the future should extend this entry rather than inventing a parallel metric.",
		AggregationNotes: "increase(platform_rls_violations_total{violation_type=\"cross_tenant_access\"}[5m]) > 0 pages regardless of which service emits it — the intended cross-service alert shape once ratified. Platform-wide tenant-isolation board: sum by (domain, service, violation_type) rate(...[w]).",
		SupersedesTier3:  []string{"iam_realm_provisioner_rls_violations_total"},
	},
	{
		Name:               "platform_messages_received_total",
		Tier:               "platform",
		Status:             StatusProposed,
		SemanticDefinition: "A message an inbound consumer received off a queue, counted once per delivery attempt (including a redelivery) before dedup/decode/business logic runs. Named verbatim as one of the standard's own Canonical Tier 1 examples — proposed here (not adopted directly as Canonical) because this service's shared messaging library (platform-events) emits its own differently-named events_consumed_total/etc. instead of this standard name, and this entry documents an independent, parallel emission at this service's own consumer call sites rather than a rename of the shared library.",
		RequiredLabels:     []string{"domain", "service", "environment"},
		ApprovedLabels:     []string{"consumer"},
		LabelValues: map[string][]string{
			"consumer": {"membership", "tenant", "billing"}, // this service's frozen (§25) processed_events.consumer values
		},
		Cardinality:      "Bounded — one value per consumer this service registers (currently 3); no message/event identifier is ever a label value.",
		AggregationNotes: "Platform-wide inbound-volume board: sum by (domain, service, consumer) rate(platform_messages_received_total[w]). received - processed - failed for the same window and consumer should reconcile to ~0 outside of in-flight messages at the window boundary.",
		SupersedesTier3:  nil,
	},
	{
		Name:               "platform_messages_processed_total",
		Tier:               "platform",
		Status:             StatusProposed,
		SemanticDefinition: "A message an inbound consumer's Handle() returned nil for — including a deliberate no-op acknowledgement of an unknown event type — meaning SQS deletes it and it will not be redelivered. Named verbatim as one of the standard's own Canonical Tier 1 examples; same parallel-emission rationale as platform_messages_received_total above.",
		RequiredLabels:     []string{"domain", "service", "environment"},
		ApprovedLabels:     []string{"consumer"},
		LabelValues: map[string][]string{
			"consumer": {"membership", "tenant", "billing"},
		},
		Cardinality:      "Bounded — one value per consumer this service registers (currently 3); no message/event identifier is ever a label value.",
		AggregationNotes: "Per-consumer success rate: sum(rate(platform_messages_processed_total[w])) / sum(rate(platform_messages_received_total[w])) by (consumer).",
		SupersedesTier3:  nil,
	},
	{
		Name:               "platform_messages_failed_total",
		Tier:               "platform",
		Status:             StatusProposed,
		SemanticDefinition: "A message an inbound consumer's Handle() returned a non-nil error for, leaving it visible on the queue for SQS's own redelivery. Named verbatim as one of the standard's own Canonical Tier 1 examples; same parallel-emission rationale as platform_messages_received_total above.",
		RequiredLabels:     []string{"domain", "service", "environment"},
		ApprovedLabels:     []string{"consumer"},
		LabelValues: map[string][]string{
			"consumer": {"membership", "tenant", "billing"},
		},
		Cardinality:      "Bounded — one value per consumer this service registers (currently 3); no message/event identifier is ever a label value.",
		AggregationNotes: "Per-consumer failure rate: sum(rate(platform_messages_failed_total[w])) / sum(rate(platform_messages_received_total[w])) by (consumer). A sustained nonzero rate on one consumer, with platform_dlq_messages_total flat, means the handler is failing but recovering before the dead-letter threshold — still worth investigating, just not yet a poison message.",
		SupersedesTier3:  nil,
	},
	{
		Name:               "platform_dlq_messages_total",
		Tier:               "platform",
		Status:             StatusProposed,
		SemanticDefinition: "A message observed at the dead-letter threshold — one delivery before SQS's own redrive policy would move it to the queue's -dlq sibling — counted once per message, purely as an observation of that existing, unaltered outcome (the dead-letter handler that increments this always returns a non-nil error so the message's actual fate is unchanged: SQS's redrive policy still moves it on the next attempt). Named verbatim as one of the standard's own Canonical Tier 1 examples; same parallel-emission rationale as platform_messages_received_total above.",
		RequiredLabels:     []string{"domain", "service", "environment"},
		ApprovedLabels:     []string{"consumer"},
		LabelValues: map[string][]string{
			"consumer": {"membership", "tenant", "billing"},
		},
		Cardinality:      "Bounded — one value per consumer this service registers (currently 3); no message/event identifier is ever a label value.",
		AggregationNotes: "increase(platform_dlq_messages_total[1h]) > 0 is worth a WARN-severity alert once ratified — a poison message needs operator triage (inspect the real -dlq queue) regardless of which service or consumer produced it.",
		SupersedesTier3:  nil,
	},
}
