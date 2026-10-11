# Architecture diagrams

Standalone Mermaid source files. `ARCHITECTURE.md` carries its own independent (and more detailed) version of the layer-model diagram inline as a fenced code block — these `.mmd` files are a separate, narrower diagram set covering specific flows, not embedded in or generated from `ARCHITECTURE.md`. Keep both in sync by hand when either changes.

| File | Diagram | Related section |
|------|---------|------------|
| [`layer-model.mmd`](mermaid/layer-model.mmd) | Clean Architecture layer graph | ARCHITECTURE.md § Layer model |
| [`package-dependencies.mmd`](mermaid/package-dependencies.mmd) | Go import graph (mirrors `.go-arch-lint.yml`) | `.go-arch-lint.yml` |
| [`request-flow.mmd`](mermaid/request-flow.mmd) | Full HTTP request lifecycle (RP-1 trial provision as the representative endpoint) | `.claude/CLAUDE.md` § Key Files to Know (router.go), `internal/adapter/inbound/http/router.go` |
| [`write-flow.mmd`](mermaid/write-flow.mmd) | Keycloak-effect-first, then local commit + outbox enqueue (§9.1) | LLD §9.1, `internal/core/service/provisioning_service.go` (`ProvisionTrial`) |
| [`provisioning-flow.mmd`](mermaid/provisioning-flow.mmd) | RP-1 trial provisioning end to end — HTTP call → Keycloak realm/user creation → `TrialTenantProvisioned` in the outbox | LLD §8.1 |
| [`cache-strategy.mmd`](mermaid/cache-strategy.mmd) | Keycloak admin-token cache (RP-D4, in-process single-flight tier) + the idempotency-ledger fast path and failure mode | LLD §6 Caching Design, `internal/adapter/outbound/keycloak/admin_token.go` |
| [`event-outbox-flow.mmd`](mermaid/event-outbox-flow.mmd) | Enqueue validation → outbox → single-topic `iam.tenant.events` SNS publish (GlueCodec version resolved by definition), plus the 3 inbound SQS consumer queues and their shared pipeline (`sqspipeline`: library GlueDecodeCodec → metrics → consumed-schema validation, violations straight to `-dlq` → handler with inbox dedup; platform-events forwards the 5th failed delivery to `-dlq`) | LLD §3.3.3, §7 Event Architecture |
| [`observability-stack.mmd`](mermaid/observability-stack.mmd) | OTel + Prometheus + Zap, all wired once through `platform-gincommon` | `.claude/CLAUDE.md` § Operations (Observability), `internal/adapter/outbound/metrics/metrics.go` |
| [`rls-guc-flow.mmd`](mermaid/rls-guc-flow.mmd) | `app.tenant_id` GUC injection per request (RLS-6) | `.claude/CLAUDE.md` § Key Files to Know (RLS-6), `test/postgres/rls_test.go` Case 5 |

To render locally, open any `.mmd` file in a Mermaid-aware IDE (VS Code + Mermaid Preview, IntelliJ + Mermaid plugin) or paste into [mermaid.live](https://mermaid.live).
