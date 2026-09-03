# Runbook — AWS Glue Schema Registry state

## The shared registry — read this first

`iam-tenant-events` is **not** this service's own registry. It is **shared**
with `iam-org-membership` (O&M), the topic's other producer (LLD §7.3.1):

> Because `iam.tenant.events` is a two-producer topic, this service publishes
> into the same Glue registry Org & Membership already uses for the topic —
> `iam-tenant-events` (`GLUE_REGISTRY_TENANT_NAME`, ap-south-1, JSON data
> format). O&M registers two schemas there (`TenantCreated`, `TrialStarted`);
> this service registers the other nine. The two producers share the
> registry and the topic but never the schema names, so there is no
> collision (RP-D5, §22 — the nine event-type names are this service's alone
> within this registry).

Every operational tool in this repo (`make schema-*`, `.github/workflows/schema-*.yml`)
is scoped to this service's own 9 schemas — the files under
`internal/eventschema/`. None of them ever registers, diffs, verifies, or
deletes `TenantCreated` / `TrialStarted`. Because those two names have no
file in this repo and never will, `schema-gov prune`'s orphan detection
(Glue schema names minus local file names) **will always list them as
"orphaned."** They are not — treat any dry-run report line naming either of
them as an expected false positive, and never pass `--execute` in a way that
would touch them. `schema-prune.yml` enforces this with a hard guard step
that inspects the live registry and refuses to run `--execute` at all while
either name is present — which, given the paragraph above, is effectively
always, until `platform-schemagov` ships a schema-name exclusion flag or O&M
moves those two schemas elsewhere. See "Pruning orphaned schemas" below.

## The contract

The service reads its 9 event schemas from AWS Glue at startup
(`eventbus.NewGlueCodec` / the `ValidatingCodec`, `internal/adapter/outbound/eventbus/glue_codec.go`,
pre-fetches every schema-version UUID it needs to encode outbound events).
Startup fails readiness if any of the 9 is missing — the pod will not accept
traffic. This is intentional; publishing an event whose header points at a
non-existent Glue schema version would silently poison every consumer
downstream (LLD §7.3.1).

The 9 expected schema names in the registry — **PascalCase**, per LLD §7.3.1
and the frozen name inventory (§25):

| Domain event `type` | Registered Glue schema name |
|---|---|
| `TrialTenantProvisioned` | `TrialTenantProvisioned` |
| `TenantRealmReady` | `TenantRealmReady` |
| `TenantConverted` | `TenantConverted` |
| `DirectPaidSignup` | `DirectPaidSignup` |
| `TrialExpired` | `TrialExpired` |
| `TrialReactivated` | `TrialReactivated` |
| `TenantSuspended` | `TenantSuspended` |
| `TenantOffboarded` | `TenantOffboarded` |
| `TenantReactivated` | `TenantReactivated` |

Unlike some sibling services, this service's event `type` names and Glue
schema names are identical (no dot-notation-to-PascalCase translation layer)
— see `internal/core/domain/event.go`'s `Event*` constants and
`internal/eventschema/schemas.go`'s `ByEventType` map, which is the
authoritative list this repo's tooling iterates over.

Note: the **JSON filenames** in `internal/eventschema/`
(`trial_tenant_provisioned.json`, etc.) are snake_case to satisfy
`schema-gov validate`'s Pass 7 coverage rule and this repo's Go file-naming
convention. That is a source-file convention only and does not dictate the
registered Glue schema name — `internal/eventschema/asyncapi_drift_test.go`
is the CI-enforced guarantee that the file stem's PascalCase form and the
`ByEventType` map key always agree.

## Pre-deploy checklist

Run this once, in the target AWS account, before every promotion to a new
environment. Fails loudly if any of this service's 9 schemas is missing —
does **not** check for or care about `TenantCreated`/`TrialStarted`.

```bash
export GLUE_REGISTRY_TENANT_NAME=iam-tenant-events   # or the env-specific name
export AWS_REGION=ap-south-1
make schema-verify
```

Expected output:

```
OK: all 9 iam-realm-provisioner schemas present in registry 'iam-tenant-events'
```

Failure modes and fixes:

- `FAIL: missing Glue schemas in registry 'iam-tenant-events': TrialTenantProvisioned ...`
  The registry does not yet have the schema(s). Register them:

  ```bash
  make schema-register
  make schema-verify   # re-check
  ```

- The IAM role running `aws glue get-schema` returns `AccessDenied`.
  Ensure the caller can `glue:GetSchemaVersion` (see `deploy/iam/policy.json`
  sid `GlueSchemaRegistryReadOnly` for the pod's own runtime role; the CI
  pipeline's AWS credentials additionally need `glue:CreateSchema` /
  `glue:RegisterSchemaVersion` / `glue:DeleteSchema`, provisioned separately
  from the pod role — see "IAM policy" below).

## What happens if a schema is missing at pod startup

`NewGlueCodec` returns an error of the form:

```
prefetch glue schema "TrialTenantProvisioned" in registry "iam-tenant-events":
<underlying AWS error> — run `make schema-verify` to confirm the 9 expected
schemas exist
```

The pod fails readiness (`/readyz` — LLD §7.3.1) before opening the HTTP
port. Kubernetes will restart-loop it (CrashLoopBackOff). Nothing publishes
to `iam-tenant-events` during this state — the outbox continues to accumulate
rows.

**Mitigation:** roll back the Helm release with `helm rollback` and register
the missing schema, then re-deploy. Do NOT set `GLUE_REGISTRY_TENANT_NAME=""`
in an attempt to fall through to the `NoopCodec` in a real environment — that
publishes unversioned JSON and permanently corrupts the audit trail for the
duration. `NoopCodec` is a local-dev/test fallback only.

## Local development

Unlike some sibling services, `scripts/init-localstack.sh` does **not**
register any Glue schemas — it only provisions this service's SNS
topic/SQS-queue/DLQ messaging topology (see the script's own header
comment). AWS Glue Schema Registry requires **LocalStack Pro**; the script
never attempts it, so a plain `make docker-up` always leaves
`GLUE_REGISTRY_TENANT_NAME` effectively unusable locally and the app runs on
`NoopCodec` (plain JSON, no Glue wire-format header) unless you additionally:

```bash
export AWS_ENDPOINT_URL=http://localhost:4566   # LocalStack Pro only
export GLUE_REGISTRY_TENANT_NAME=iam-tenant-events
make schema-register     # registers this service's 9 schemas into LocalStack's Glue mock
```

This does not create O&M's `TenantCreated`/`TrialStarted` — those are not
this service's concern even in local dev; a developer testing full-topology
event flows locally should provision them separately (or accept that this
service's own `schema-prune`/`schema-verify` will report them as absent,
which is correct — this service never expects to own them).

## Adding a new event type

1. Add the payload struct and frozen `type` constant to
   `internal/core/domain/event.go`.
2. Build the event in the relevant service method and enqueue it via the
   outbox inside the same transaction as the business write.
3. Add the message to `api/asyncapi.yaml`'s `iamTenantEvents` channel and add
   the matching JSON Schema file under `internal/eventschema/` using the
   snake_case filename convention (e.g. `tenant_x_happened.json`), registering
   it in `ByEventType` (`internal/eventschema/schemas.go`).
4. Run `go test ./internal/eventschema/... -run TestAsyncAPIVsSchemaDrift` —
   this service's CI-enforced drift gate between the AsyncAPI spec and the
   embedded schemas.
5. Run `make schema-validate` locally — 8 passes, no AWS credentials needed.
6. §25 names are frozen at v1.0 sign-off — do not rename an existing event
   type or field; a post-freeze breaking change needs a new Glue schema name,
   a new AsyncAPI message, and a major version bump (LLD §7.3.1).
7. Merge, then let `schema-registry.yml` register the new schema in each
   environment (staging on push to `main`, production on release) before the
   first pod that would publish the new event starts there.

## CI/CD pipeline overview

`.github/workflows/schema-registry.yml` runs on schema-file changes
(pull request / push to `main`) and on release/`workflow_dispatch`:
validate (8 passes) → check `SCHEMA_FREEZE` → configure AWS credentials →
detect orphaned schemas (dry-run; expect `TenantCreated`/`TrialStarted` in
the list, see above) → assess event usage (dormant while `PROMETHEUS_URL` is
unset, see `.env-example`) → enforce usage→lifecycle (also dormant while
usage assessment is dormant) → diff against the registry (fails the run on a
breaking change) → register → update `docs/schema-changelog.md` → emit
metrics. The `pr-check` job runs a read-only subset (validate + diff) so a
pull request fails fast on a breaking change before merge.

`SCHEMA_FREEZE=true` (a repository/environment variable) blocks every AWS
step in a given environment — set it during an incident or a coordinated
migration window; override a single run with `workflow_dispatch`'s
`force: true` input.

`.github/workflows/schema-health-quarterly.yml` runs a read-only quarterly
report (lifecycle lint + Glue version-accumulation scan) as a GitHub Step
Summary — it never mutates the registry. Expect "no x-lifecycle annotation"
warnings for all 9 of this service's events in that report; `api/asyncapi.yaml`
deliberately carries no `x-lifecycle` annotations today (frozen event set,
see `ARCHITECTURE.md`) and Pass 4 only warns, never fails, on that condition.

## Pruning orphaned schemas

`.github/workflows/schema-prune.yml` runs a monthly staging dry-run report
plus an operator-triggered `workflow_dispatch` (staging or production,
dry-run or execute). Read the file header before triggering an execute run —
it hard-fails with `Guard — exclude O&M-owned shared-registry schemas` if
`TenantCreated` or `TrialStarted` is present in the target registry, which
in practice means **execute mode is currently unavailable for this service's
automated pipeline** (those two names are permanent residents of the shared
registry). If this service ever accumulates a real orphan of its own
(a retired schema whose file was removed from `internal/eventschema/`), it
must currently be pruned by an operator directly via `aws glue delete-schema`
against that one schema name, after manually confirming it is not
`TenantCreated`/`TrialStarted` and archiving its version history by hand —
or by asking the `platform-schemagov` maintainers for a name-exclusion flag
so the automated workflow can resume safely.

## IAM policy — required SIDs

The pod's IAM role must include the following SIDs (see
`deploy/iam/policy.json`). Missing any of these produces `AccessDenied` at
startup.

**Glue Schema Registry (read-only, pod runtime):**
- `GlueSchemaRegistryReadOnly` — `glue:GetRegistry`, `glue:GetSchema`,
  `glue:GetSchemaVersion`, `glue:GetSchemaByDefinition`, `glue:ListSchemas`,
  `glue:QuerySchemaVersionMetadata`.

**Glue Schema Registry (write, CI pipeline only — a separate AWS principal
from the pod role, configured as the `staging`/`production` GitHub
Environment secrets `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`):**
`glue:CreateRegistry`, `glue:CreateSchema`, `glue:RegisterSchemaVersion`,
plus the read-only actions above for diff/verify, plus `glue:DeleteSchema`
for `schema-prune.yml`'s execute mode (currently unreachable in automation —
see "Pruning orphaned schemas" above). The pod role is never granted write
access to the registry.

## Required env vars

Set in `deploy/helm/iam-realm-provisioner/values.yaml` (per environment) or
`.env-example` (dev):

| Variable | Purpose | Notes |
|---|---|---|
| `GLUE_REGISTRY_TENANT_NAME` | Glue registry name | `iam-tenant-events` in every environment that shares it with O&M. Leave empty in dev (`NoopCodec`) — see "Local development" above |
| `SCHEMA_GOV_IMAGE` | `platform-schemagov` Docker image tag used by `make schema-*` and `.github/workflows/schema-*.yml` | Currently `ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4` |
| `PROMETHEUS_URL` | Event-usage data source for the usage-check/enforce-lifecycle CI steps | Deliberately unset — this service is pre-production with no live Prometheus, and its 9 events are frozen at LLD v1.0; auto-deprecating a never-fired-in-pre-production event would be wrong. Leaving it unset runs usage-check with `--source disabled`, which makes lifecycle enforcement a deliberate no-op, not a deleted capability |
| `AWS_REGION` | Primary region | `ap-south-1` |

`GLUE_REGISTRY_TENANT_NAME` has no corresponding `*_ARN` variable in this
repo today. `GLUE_REGISTRY_ARN`-style IAM-policy-scoping variables exist in
some sibling services, but `platform-schemagov`'s actual CLI commands
(`register`, `diff`, `prune`, `verify` equivalents) take only `--registry`
(a name) and `--region` — no ARN argument — so there is nothing in this
service's own schema-governance tooling that would consume one. If a future
IAM policy needs ARN-based resource scoping for the pod's `GlueSchemaRegistryReadOnly`
SID, add it as a Helm value at that point rather than pre-emptively here.
