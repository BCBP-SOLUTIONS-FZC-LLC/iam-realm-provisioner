package metrics

// Platform Observability Registry — ownership note
//
// All Tier 1 platform_* metrics (platform_messages_received_total,
// platform_messages_processed_total, platform_messages_failed_total,
// platform_dlq_messages_total, platform_retry_total,
// platform_duplicate_messages_total, platform_dependency_request_seconds,
// platform_event_propagation_seconds, platform_build_info, and the full
// outbox/queue-depth family) are owned by platform-gincommon and
// platform-events. They are registered automatically by:
//
//   - gincommon.ObservabilityMiddlewares (platform_build_info, HTTP metrics)
//   - events.InitMetrics (all messaging and dependency metrics)
//
// This service MUST NOT register any platform_* collector independently.
// Duplicate registration with a mismatched label schema causes a
// RegistrationWarning that silences the library's authoritative copy
// (fail-soft), corrupting cross-service aggregation.
//
// The canonical registry definition — name, help text, label vocabulary,
// bucket presets, status, governance notes — lives in platform-gincommon's
// internal/obsregistry/registry.json (embedded, versioned with the library).
// Use gincommon.NewSharedCounterVec / NewSharedHistogramVec / NewSharedGaugeVec
// to build Tier 1 or Tier 2 collectors that are in that registry, or
// gincommon.NewCounterVec / NewGaugeVec / NewHistogramVec for Tier 3
// (iam_realm_provisioner_*) service-specific metrics.
//
// Tier 2 (iam_*) domain-shared metrics: none currently apply for this service.
// RP-INV-1 makes this the sole Keycloak Admin API caller; every remaining
// metric represents a behavior no other IAM service performs.
