#!/usr/bin/env bash
# Installs go-arch-lint if missing, then enforces .go-arch-lint.yml rules
# (§3.4 — internal/core/* imports no adapter/Gin/pgx/gocloak/AWS SDK).
set -euo pipefail

if ! command -v go-arch-lint >/dev/null; then
  go install github.com/fe3dback/go-arch-lint@v1.15.0
fi
go-arch-lint check --project-path .

# RP-INV-1: no gocloak import outside internal/adapter/outbound/keycloak.
echo "Checking RP-INV-1 (single Keycloak writer)..."
offenders=$(grep -rl "Nerzal/gocloak" --include="*.go" . 2>/dev/null | grep -v "^\./internal/adapter/outbound/keycloak/" | grep -v "_test.go" || true)
if [ -n "$offenders" ]; then
  echo "FAIL: gocloak imported outside internal/adapter/outbound/keycloak (RP-INV-1 violation):"
  echo "$offenders"
  exit 1
fi
echo "OK: gocloak is imported only by internal/adapter/outbound/keycloak"

# Observability invariant: logs, metrics, and traces must flow through
# platform-gincommon only — no independently-configured logging/metrics/
# tracing library. Three checks, each excluding _test.go (test doubles that
# read back metrics/spans for assertions, e.g. prometheus/testutil or an
# in-memory span recorder, are not a production bypass):
#
#   1. Logging — the only logger built anywhere in this repo is
#      platform-gincommon/pkg/logger.NewLogger (cmd/server/main.go,
#      cmd/reconciler/main.go); every call site takes port.Logger, which
#      that logger satisfies directly. zap/logrus/stdlib "log" imported
#      elsewhere would mean a second, ungoverned logging path.
#   2. Metrics — every custom Prometheus collector in this repo registers
#      onto gincommon.MetricsRegisterer() (internal/adapter/outbound/metrics/
#      metrics.go), never prometheus.DefaultRegisterer/MustRegister
#      directly, so a single /metrics scrape and one set of {service,
#      version} const labels covers HTTP + business + outbox + pg
#      collectors together.
#   3. Tracing — both binaries install the process TracerProvider exactly
#      once via gincommon.InitTracingFromEnv() (cmd/server/main.go,
#      cmd/reconciler/main.go); every span elsewhere (Keycloak adapter,
#      pgcommon query tracer, reconciler job spans) is created against that
#      same provider via the plain otel.Tracer(...) accessor. A second
#      SetTracerProvider/NewTracerProvider call would silently fork spans
#      onto an unconfigured or duplicate provider.
echo "Checking observability invariant (logs/metrics/traces via platform-gincommon only)..."

offenders=$(grep -rlE '"go\.uber\.org/zap"|"github\.com/sirupsen/logrus"|^[[:space:]]*"log"$' --include="*.go" . 2>/dev/null | grep -v "_test.go" || true)
if [ -n "$offenders" ]; then
  echo "FAIL: a logging library other than platform-gincommon's port.Logger is imported:"
  echo "$offenders"
  exit 1
fi

offenders=$(grep -rlE 'prometheus\.MustRegister\(|prometheus\.DefaultRegisterer\b' --include="*.go" . 2>/dev/null | grep -v "_test.go" || true)
if [ -n "$offenders" ]; then
  echo "FAIL: a Prometheus collector is registered outside gincommon.MetricsRegisterer():"
  echo "$offenders"
  exit 1
fi

offenders=$(grep -rlE 'otel\.SetTracerProvider\(|sdktrace\.NewTracerProvider\(|"go\.opentelemetry\.io/otel/sdk/trace"' --include="*.go" . 2>/dev/null | grep -v "_test.go" || true)
if [ -n "$offenders" ]; then
  echo "FAIL: an OpenTelemetry TracerProvider is configured outside gincommon.InitTracingFromEnv()/InitTracing():"
  echo "$offenders"
  exit 1
fi

echo "OK: logs/metrics/traces route through platform-gincommon only"

# Database invariant: every connection, config, and operation must flow
# through platform-pgcommon only — no second, ungoverned way to reach
# Postgres. Unlike the observability checks above, _test.go is NOT
# excluded here: test/dbseed/pool.go and every test/postgres/*_test.go
# (including the Case 5 pinned-connection RLS test, which needs
# MaxConns=1) already construct pools via pgcommon.NewPool precisely so
# test pools share the same connection/GUC/drain path as production —
# there is no legitimate reason, test or production, to open a second one.
#
#   1. Pool/connection construction — pgcommon.NewPool is the only pool
#      constructor anywhere in this repo (cmd/server/main.go,
#      cmd/reconciler/main.go, test/dbseed/pool.go); a bare
#      pgxpool.New/NewWithConfig or pgx.Connect/ConnectConfig would open a
#      pool pgcommon never configured (no GUCProvider, no pgcommon.Logger/
#      Tracer hook, no retry/pooling semantics).
#   2. Driver — no database/sql or lib/pq import: this repo talks to
#      Postgres exclusively through pgx (which pgcommon wraps), never the
#      stdlib database/sql interface (including pgx's own stdlib bridge).
#   3. Migrations — platform-pgcommon/pkg/migrate.Runner
#      (internal/adapter/outbound/postgres/migrate.go) is the only
#      migration runner; a direct golang-migrate/migrate import would be a
#      second, undocumented migration path alongside it.
echo "Checking database invariant (connections/config/operations via platform-pgcommon only)..."

offenders=$(grep -rlE '"database/sql"|"github\.com/lib/pq"|"github\.com/jackc/pgx/v5/stdlib"' --include="*.go" . 2>/dev/null || true)
if [ -n "$offenders" ]; then
  echo "FAIL: a Postgres driver other than pgx-via-pgcommon is imported:"
  echo "$offenders"
  exit 1
fi

offenders=$(grep -rlE 'pgxpool\.New\(|pgxpool\.NewWithConfig\(|pgx\.Connect\(|pgx\.ConnectConfig\(' --include="*.go" . 2>/dev/null || true)
if [ -n "$offenders" ]; then
  echo "FAIL: a connection pool/connection is constructed outside pgcommon.NewPool:"
  echo "$offenders"
  exit 1
fi

offenders=$(grep -rlE '"github\.com/golang-migrate/migrate' --include="*.go" . 2>/dev/null || true)
if [ -n "$offenders" ]; then
  echo "FAIL: golang-migrate imported directly instead of platform-pgcommon/pkg/migrate:"
  echo "$offenders"
  exit 1
fi

echo "OK: database connections/config/operations route through platform-pgcommon only"

# Events invariant: every SNS publish, SQS consume, and outbox write must
# flow through platform-events only — no independent messaging path.
# Dedup included: processed_events is read and written only through
# platform-events' pkg/inbox (inbox.Store, wrapped by
# internal/adapter/outbound/postgres/inbox_repository.go) — check 5 below
# forbids a second, hand-rolled writer. The table's DDL stays in this
# service's migration (in inbox's own shape) so its grants live with it.
#
#   1. Publish — events.NewSNSPublisher (cmd/server/wiring.go) is the only
#      way this repo puts a message on iam-tenant-events; no file may
#      import the raw aws-sdk-go-v2/service/sns package (a direct
#      sns.Client.Publish call would bypass the outbox entirely, breaking
#      RP-INV-3's atomic-commit guarantee).
#   2. Consume — events.NewSQSConsumerWithClient (cmd/server/main.go) is
#      the only SQS consumer construction site; the raw
#      aws-sdk-go-v2/service/sqs import there exists solely to build the
#      *sqs.Client platform-events' SQSClientLike interface requires, so
#      it is the one file allowed to import it — everywhere else, a direct
#      import would mean hand-rolled polling/delete/DLQ logic duplicating
#      platform-events' own.
#   3. Outbox writes — outbox.Enqueue (internal/adapter/outbound/eventbus/
#      publisher.go) is the only inserter of outbox_events rows; a literal
#      "INSERT INTO outbox_events" elsewhere would be a second, ungoverned
#      writer bypassing schema validation and the Codec pipeline
#      (_test.go excluded — publisher_test.go asserts against
#      outbox.Enqueue's own generated SQL as a string literal, which is
#      not a competing writer).
#   4. Envelope — events.Envelope (platform-events) is the only envelope
#      type; a local "type Envelope struct" would be a second, incompatible
#      wire shape.
#   5. Inbox — inbox.Store (platform-events' pkg/inbox) is the only
#      processed_events writer; a hand-rolled INSERT would be a second dedup
#      ledger outside the library's claim-in-the-same-transaction contract.
echo "Checking events invariant (SNS/SQS/outbox/inbox via platform-events only)..."

offenders=$(grep -rlE '"github\.com/aws/aws-sdk-go-v2/service/sns"' --include="*.go" . 2>/dev/null || true)
if [ -n "$offenders" ]; then
  echo "FAIL: raw aws-sdk-go-v2/service/sns imported — publish only via events.NewSNSPublisher:"
  echo "$offenders"
  exit 1
fi

# Test files are exempt (matching iam-org-membership's
# check-forbidden-events-bypass.sh): a fake SQS transport implementing
# events.SQSClientLike legitimately uses the SDK's request/response types to
# simulate the external queue.
offenders=$(grep -rlE '"github\.com/aws/aws-sdk-go-v2/service/sqs(/[a-z0-9]+)*"' --include="*.go" . 2>/dev/null | grep -v "^\./cmd/server/main\.go$" | grep -v "_test\.go$" || true)
if [ -n "$offenders" ]; then
  echo "FAIL: raw aws-sdk-go-v2/service/sqs imported outside cmd/server/main.go (the events.NewSQSConsumerWithClient wiring site):"
  echo "$offenders"
  exit 1
fi

offenders=$(grep -rlE 'INSERT INTO outbox_events' --include="*.go" . 2>/dev/null | grep -v "_test.go" || true)
if [ -n "$offenders" ]; then
  echo "FAIL: a second outbox_events writer exists outside platform-events' outbox.Enqueue:"
  echo "$offenders"
  exit 1
fi

offenders=$(grep -rlE '^[[:space:]]*type[[:space:]]+Envelope[[:space:]]' --include="*.go" . 2>/dev/null | grep -v "_test.go" || true)
if [ -n "$offenders" ]; then
  echo "FAIL: a local Envelope type competes with platform-events' events.Envelope:"
  echo "$offenders"
  exit 1
fi

offenders=$(grep -rlE 'INSERT INTO (public\.)?processed_events' --include="*.go" . 2>/dev/null | grep -v "_test\.go$" || true)
if [ -n "$offenders" ]; then
  echo "FAIL: a hand-rolled processed_events writer exists outside platform-events' inbox.Store:"
  echo "$offenders"
  exit 1
fi

echo "OK: events/outbox/inbox route through platform-events only"

# RLS-6 (no session-scoped tenant GUC) is checked once, inline in
# validate-quality.yml's "Reject non-transaction-local SET app.tenant_id"
# step — matching the sibling IAM services' convention (org-membership's
# own arch-lint.sh does only the go-arch-lint check above, with its RLS-6
# gate likewise inline in validate-quality.yml, not duplicated here).
