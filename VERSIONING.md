# Versioning and releases

This repository is a **deployed Go microservice**, not a library other services `go get` — `pkg/requestctx` is an internal helper for this process, and the `go.mod` module path exists so this repo's own code compiles, not for import by sibling repos (contrast [`platform-events`](https://github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events)'s `VERSIONING.md`, which this document is modeled on but differs from for that reason). What gets versioned here is the **container image** (`ghcr.io/bcbp-solutions-fzc-llc/iam-realm-provisioner`) and the **Helm chart** (`deploy/helm/`) that deploys it, plus the runtime contract Signup, Billing, Org & Membership, AuthZ Enrichment, and operators depend on. Versions are published with **Git tags** and described in [CHANGELOG.md](./CHANGELOG.md).

This document follows the same SemVer / image-tag / maintainer-process layout as [`iam-authz-enrichment/VERSIONING.md`](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-authz-enrichment/blob/main/VERSIONING.md).

## Semantic versioning (SemVer)

We use [SemVer 2.0.0](https://semver.org/): `MAJOR.MINOR.PATCH` (e.g. `v1.2.3`).

| Bump | When you change | Examples |
|------|-----------------|----------|
| **MAJOR** | Breaking change in the runtime contract (§ below) | Removing/renaming an RP-1..RP-16 path, a published event `type` or frozen payload field, a `processed_events.consumer` value, a required env var, the OpenBao IdP secret path, or an incompatible `values.yaml` restructure |
| **MINOR** | New backward-compatible capability | A new RP endpoint (new number, never reuse a retired one), a new optional env var / Helm value, a new produced or consumed event type, a new `iam_realm_provisioner_*` metric, an additive payload field |
| **PATCH** | Backward-compatible fix | Bug fix, performance improvement, dependency bump with no observable behavior change, documentation-only correction |

### What counts as the runtime contract

This service has no Go package for another service to import — its "public API" is the wire/deployment contract every caller and operator depends on. Names frozen at LLD v1.0 live in [LLD §25](docs/lld/iam-lld-realm-provisioner-service.md) — a freeze-time rename is a MAJOR (or a new versioned event type); it is not a silent edit.

| In scope (SemVer applies) | Out of scope (may change without MAJOR) |
|---------------------------|------------------------------------------|
| The 16 mesh-only paths under `/api/v1/internal` (RP-1..RP-16) — method, path, and frozen request/response field names (LLD §5, §25) | `internal/*` package structure, exported Go identifiers, file layout — nothing here is imported by another module. Gin's internal capture name (`:keycloak_user_id` vs the LLD's `:uid` on RP-9) is not part of the wire contract |
| The 9 produced event `type` names and frozen payload fields on `iam.tenant.events`; the three inbound queue names and their `EventType` filters (LLD §7, §25) | Envelope field order / JSON key sort; Glue wire-format header internals |
| `processed_events.consumer` ∈ `{membership, tenant, billing}` and `TenantSuspended.source` / `TenantReactivated.source` enums | Additive `provisioning_operations.operation` discriminator values (new endpoint names) |
| Domain error **codes** in LLD §17 (`keycloak_unavailable`, `operation_in_progress`, `idp_region_portability_failed`, …) and their HTTP statuses | Exact `ErrorResponse.message` wording |
| Required env var **names and semantics** (LLD §12, `.env-example`) | Env var **defaults** — tunable without a MAJOR bump unless the new default itself breaks a documented invariant (`cmd/server` fail-fast, CONFIG-1..6) |
| `deploy/helm/values.yaml` top-level key names and shapes consumers actually set (`image.*`, `env`, `envFromSecret`, `cronjobs`, `autoscaling`, `service.*`, replica/HPA fields) | Chart internals (`_helpers.tpl`, template structure) not exposed as a `values.yaml` key |
| Prometheus metric **names** listed in LLD §11.2 / `internal/adapter/outbound/metrics/metrics.go` (`iam_realm_provisioner_provision_total`, `users_migrated_total`, `realm_active_gauge`, `keycloak_call_duration_seconds`, `keycloak_call_failures_total`, `admin_token_refresh_total`, `idp_validation_total`, `mfa_role_sync_total`, `realm_config_pending_gauge`, `trial_expiry_processed_total`, `trial_expiry_deferred_total`, `lapse_processed_total`, `realm_export_total`, `user_delete_total`, `session_revoke_total`, `mfa_reset_total`, `template_drift`, `rls_violations_total`, `unknown_event_acknowledged_total`, `processed_events_duplicates_total`) — dashboards and alert rules key off these exact names | Metric **label cardinality** beyond what's documented, histogram bucket boundaries; passthrough `http_*` / `events_*` / `sqs_*` names owned by `platform-gincommon` / `platform-events` |
| OpenBao KV path `secret/data/iam/realm-provisioner/idp/<tenant_id>/<idp_alias>` and the `Idempotency-Key` header on the mutating routes that require it | In-process vs Valkey admin-token cache implementation (LLD §6.2 fallback is not a caller-visible contract) |
| `GET /healthz` / `/readyz` (HTTP) — existence and meaning (`/readyz` checks Postgres, sys-Postgres, outbox, Keycloak token bootstrap) | `GET /asyncapi` / `/asyncapi.yaml` content shape — documentation, not a contract a caller must hold stable. Dedicated `/metrics` port number is a Helm `service.metricsPort` default, not a SemVer surface |

Keycloak's Admin API and gocloak's client types are **not** owned by this repo — gocloak's version is tracked in the compatibility matrix below, not by this service's own SemVer. Callers never import gocloak; they hit RP-1..RP-16.

### Guarantees

- **Pre-`v1.0.0` (current status):** per [SemVer §4](https://semver.org/#spec-item-4), anything may change at any time while the major version is `0` — this service has not yet committed to a stable *service* contract via a Git tag. The LLD name freeze (§25) still applies inside the repo: do not silently rename a frozen event or path in `[Unreleased]`. The table above still indicates what's *more* disruptive than what within `v0.x` (a MINOR bump is still meant to signal "safer than a MAJOR bump would have been"), but a downstream caller should not yet assume `v0.x` compatibility across a MINOR bump the way it could once `v1.0.0` ships.
- **MAJOR (once `v1` ships):** we avoid breaking changes to the runtime contract within `v1.x`. A breaking change ships as `v2.0.0` with migration notes in the CHANGELOG.
- **MINOR:** safe to redeploy without changing caller URLs, event filters, or Helm values, unless you opt into a new capability.
- **PATCH:** drop-in image replacement; upgrade recommended for security fixes (every image is CVE-scanned, see below).

## Supported releases

| Version | Status | Image tag | Notes |
|---------|--------|-----------|-------|
| *(none tagged)* | **Unreleased** | — | [CHANGELOG.md](CHANGELOG.md) lives entirely under `[Unreleased]`. Helm chart `version` is `0.1.0` (`appVersion` currently `"1.0.0"` — bump both in lockstep with the first Git tag; do not treat `appVersion` as a published release). No live deployment yet, so no support window has started |
| `< v0.1.0` | — | — | No tagged Git releases |

This service has no formal support-window policy yet, since nothing is running in production against a tagged release. Once a `v1.0.0` ships to a real environment, this section will define how long a superseded major line receives security-only fixes (expect the same platform convention `platform-events` uses: security fixes only, for a period the platform team sets, typically ~6 months after the next major).

## Consume a release

This service is **not** consumed via `go get` — do not add this module as a dependency of another Go service. It is consumed as a **container image** on the mesh (`/api/v1/internal`), via the **Helm chart** in `deploy/helm/`. Sibling services call it over HTTP (`RealmProvisionerClient` in Org & Membership; Signup / Billing for RP-1..RP-4).

### Pull and verify the image

Once a tag exists:

```bash
docker pull ghcr.io/bcbp-solutions-fzc-llc/iam-realm-provisioner:v0.1.0
```

**Today** only `main`-branch images are published (see CI below). Every image pushed from `ci.yml` is signed keylessly via Sigstore/Cosign (no long-lived key) — verify before deploying:

```bash
# main-branch builds (current — signed by ci.yml's push job)
cosign verify \
  --certificate-identity-regexp "^https://github\.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/.github/workflows/.*@refs/heads/(main|master)$" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/bcbp-solutions-fzc-llc/iam-realm-provisioner@<digest>
```

Tagged releases, published by `.github/workflows/release.yml` (LLD §13.5), use `@refs/tags/` in the identity regexp instead of `@refs/heads/(main|master)`:

```bash
# tagged releases (signed by release.yml's docker job)
cosign verify \
  --certificate-identity-regexp "^https://github\.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/.github/workflows/.*@refs/tags/.*$" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/bcbp-solutions-fzc-llc/iam-realm-provisioner@<digest>
```

See [README.md § CI](README.md#ci).

### Image tag scheme

`release.yml`'s `docker/metadata-action` step produces, per tag push (same scheme as `iam-authz-enrichment`):

| Tag pattern | Produced for | Example (from `v1.2.3`) |
|-------------|--------------|--------------------------|
| `vMAJOR.MINOR.PATCH` | Every release | `v1.2.3` |
| `vMAJOR.MINOR` | Every release | `v1.2` |
| `vMAJOR` | Every release | `v1` |
| `latest` | Stable releases only (no pre-release suffix) | `latest` |

Separately, `ci.yml` pushes the image on every merge to `main` with git SHA / branch tags — pin production by a **release tag or digest**, not `latest` or an untagged `main` build.

| Pin style | Use when |
|-----------|----------|
| `vMAJOR.MINOR.PATCH` | Production; exact reproducibility (recommended — also enables digest verification) |
| `vMAJOR.MINOR` | Accept PATCH updates automatically |
| `vMAJOR` | Accept MINOR/PATCH updates automatically — not recommended before `v1.0.0` |
| `latest` / untagged `main` | Local/dev experiments only; never production |

### Deploy via Helm

```bash
helm upgrade iam-realm-provisioner ./deploy/helm \
  --install \
  --namespace iam \
  --set image.tag=v0.1.0
```

`image.tag` (`deploy/helm/values.yaml`) defaults to the chart's own `appVersion` (`Chart.yaml`) when unset. Chart `version`/`appVersion` are bumped manually alongside the Git tag (see the maintainer process below); they are **not** derived automatically from the tag by any workflow today.

The chart deploys **one** `Deployment` (`/iam-realm-provisioner-server` — HTTP API + three SQS consumers + outbox runner + AL-5 audit drainer), a pre-install/pre-upgrade migrate hook `Job` (`/migrate`), and **seven** `CronJob`s (`/iam-realm-provisioner-reconciler --job=<name>`), all from the same image.

## Maintainer release process

`.github/workflows/release.yml` implements LLD §13.5 (validate → build → image scan/sign → GitHub Release), modeled on sibling `iam-authz-enrichment`'s release workflow. It triggers on a pushed `vMAJOR.MINOR.PATCH` (or pre-release) tag, or via `workflow_dispatch` with a `tag` input. `ci.yml`'s push-to-`main` path (image to GHCR + Cosign, unversioned SHA/branch tags) keeps running independently of tagging — it is not replaced by `release.yml`.

When cutting a tagged release:

1. **Merge** all changes for the release to `main`.

2. **Update `CHANGELOG.md`:** move `[Unreleased]` entries into a new `## [X.Y.Z] - YYYY-MM-DD` section. `release.yml`'s `build` job runs `.github/scripts/verify-changelog-entry.sh`, which fails the release if this section is missing — cut it *before* tagging.

3. **Bump `deploy/helm/Chart.yaml`'s `version`/`appVersion`** to match `X.Y.Z`. Nothing does this automatically — a consumer who deploys the chart without setting `image.tag` explicitly gets whatever `appVersion` was last committed, so a forgotten bump here silently ships a stale image.

4. **Run `make ci` locally** to confirm everything is green before tagging:
   ```bash
   make ci   # tidy + fmt-check + vet + lint + arch-lint + test-ci + build
   ```

5. **Create and push an annotated tag** — this is what triggers `release.yml`:
   ```bash
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

6. **The release workflow** (`.github/workflows/release.yml`, LLD §13.5, modeled on sibling `iam-authz-enrichment`'s release workflow) runs:
   - `validate-test` / `validate-quality` — the same reusable gates `ci.yml` uses, re-run at the exact tagged commit.
   - `build` — verifies the tag matches HEAD (`verify-release-tag.sh`), verifies the CHANGELOG entry exists (`verify-changelog-entry.sh`), cross-compiles **both** binaries (`iam-realm-provisioner-server` and `iam-realm-provisioner-reconciler`) for 5 platforms each — a convenience for non-Docker runs, not the primary distribution artifact (this service is deployed as a container carrying both entrypoints).
   - `docker` — builds and pushes the semver-tagged image (see tag scheme above), CVE-scans it (fails the release on CRITICAL/HIGH), generates a CycloneDX SBOM and SLSA provenance, signs with Cosign, then re-pulls the pushed image and runs `.github/scripts/smoke-tests.sh` against **both** binaries — the same script and pattern `ci.yml`'s `smoke` job uses.
   - `deploy-gate` (skip unless `vars.DEPLOY_GATE_ENABLED == 'true'` — no cluster configured yet) — live Helm deploy, digest verification, health check against `platform_http_requests_total{status_class="5xx"}` (this service's own `platform-gincommon`-emitted HTTP metric — there is no gRPC surface here), automatic rollback on failure.
   - `publish` — creates the GitHub Release with the CHANGELOG section as notes, plus binaries/checksums/SBOM/provenance attached; notes when `deploy-gate` was skipped.

7. **Notify consumers** — Signup, Billing, Org & Membership (HTTP clients), AuthZ Enrichment (`TenantConverted` subscriber), and whoever owns the Helm deployment — with upgrade notes if MINOR or MAJOR. Coordinate anything that touches the OpenBao path convention or the Keycloak admin credential.

### Pre-release tags (optional)

| Tag pattern | Meaning |
|-------------|---------|
| `v1.1.0-rc.1` | Release candidate; not for production unless approved |
| `v1.1.0-beta.1` | Early integration testing |

Both should match a `v[0-9]*.[0-9]*.[0-9]*-*` tag trigger and produce a signed, scanned image — but never a `latest` tag (see the tag scheme table above).

## Compatibility matrix

| iam-realm-provisioner | Go (`go.mod`) | Keycloak Admin API client | Shared platform libraries | `platform-schemagov` (`schema-gov` CLI) |
|---|---|---|---|---|
| Unreleased (`main`) | `1.26.10` | `github.com/Nerzal/gocloak/v13` `v13.9.0` (Keycloak 26 in local compose / integration tests) | `platform-gincommon/v2` v2.1.0, `platform-events/v2` v2.1.0, `platform-pgcommon/v2` v2.0.2 | `0.4` (`SCHEMA_GOV_IMAGE` in `.env-example`) |

None of those libraries is re-exported — a consumer of this *service* never needs them as a direct dependency of *this* module. Callers depend on the HTTP/event contract, not on gocloak.

Keycloak server version in production (`auth.bcbpsolutions.com`) is owned by Keycloak ops, not this repo. A gocloak major bump that changes Admin API call shapes this adapter relies on is a **MINOR** for this service if RP-1..RP-16 stay compatible, and a **MAJOR** if a frozen response field or error code changes.

## Related files

| File | Purpose |
|------|---------|
| [CHANGELOG.md](./CHANGELOG.md) | User-facing history per version |
| [README.md](./README.md) | Mental model, API, local dev, CI/CD summary |
| [CONTRIBUTING.md](./CONTRIBUTING.md#deployment) | Deployment pipeline as it exists today (`ci.yml`) |
| [docs/lld/iam-lld-realm-provisioner-service.md](./docs/lld/iam-lld-realm-provisioner-service.md) | The runtime contract in full — §5 endpoints, §7 events, §11.2 metrics, §12 configuration, §17 errors, §25 frozen names |
| [docs/runbook-schema-registry.md](./docs/runbook-schema-registry.md) | Operator runbook for the shared Glue registry — drift, pruning, IAM, rollback |
| [ARCHITECTURE.md](./ARCHITECTURE.md) | Layer model, ports, degradation matrix, key invariants |
| [.github/workflows/ci.yml](./.github/workflows/ci.yml) | Current image build / Trivy / Cosign-on-`main` pipeline |
| [.github/workflows/release.yml](./.github/workflows/release.yml) | Tag-triggered release pipeline (validate → build → docker → deploy-gate → publish) |
| [deploy/helm/Chart.yaml](./deploy/helm/Chart.yaml) | Helm chart version / app version |
| [go.mod](./go.mod) | Module path and minimum Go version — not a consumable package |
