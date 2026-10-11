# syntax=docker/dockerfile:1.7
#
# iam-realm-provisioner
#
# Multi-stage build producing a minimal, non-root, distroless runtime image
# carrying BOTH binaries this service ships (LLD §3.1, §13):
#   /iam-realm-provisioner-server       HTTP API (RP-1..RP-16) + the three
#                                        inbound SQS consumers + the
#                                        outbox.Runner, in one process. The
#                                        image's default ENTRYPOINT.
#   /iam-realm-provisioner-reconciler   the RP-C1..RP-C4 CronJob entry
#                                        points (cmd/reconciler/jobs/*.go),
#                                        selected at invocation time via
#                                        --job=<name>. No long-running
#                                        HTTP/metrics server, no ports
#                                        exposed for it.
#   /migrate                            schema migration (cmd/migrate), run
#                                        once per release by the Helm
#                                        pre-install/pre-upgrade hook Job.
#
# One image: the Deployment runs it unmodified; each CronJob template
# (deploy/helm/templates/cronjobs.yaml) overrides `command` to invoke
# /iam-realm-provisioner-reconciler --job=<name> against the SAME image
# reference.

########################################
# Stage: builder
########################################
FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS builder

ARG BUILD_VERSION=dev
ARG SOURCE_DATE_EPOCH
ENV SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}

WORKDIR /src

# Copy go.mod/go.sum first so dependency resolution is cached independently
# of application source changes.
COPY go.mod go.sum ./

# platform-gincommon, platform-pgcommon, and platform-events are private
# github.com/BCBP-SOLUTIONS-FZC-LLC modules (not vendored locally), fetched
# via git using a short-lived token — same secret-handling pattern as every
# sibling IAM service's Dockerfile.
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENV GOPRIVATE="github.com/BCBP-SOLUTIONS-FZC-LLC/*"
ENV GONOSUMDB="github.com/BCBP-SOLUTIONS-FZC-LLC/*"

RUN --mount=type=secret,id=go_private_token \
    TOKEN=$(cat /run/secrets/go_private_token 2>/dev/null || true) && \
    if [ -z "$TOKEN" ]; then echo "ERROR: go_private_token secret is missing or empty — pass --secret id=go_private_token,src=<token-file>"; exit 1; fi && \
    git config --global url."https://x-access-token:${TOKEN}@github.com/".insteadOf "https://github.com/" && \
    go mod download && \
    git config --global --unset url."https://x-access-token:${TOKEN}@github.com/".insteadOf

# Now copy the remainder of the source tree.
COPY . .

RUN CGO_ENABLED=0 GOOS=linux \
    GOPRIVATE="github.com/BCBP-SOLUTIONS-FZC-LLC/*" \
    go build \
    -trimpath \
    -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" \
    -o /out/iam-realm-provisioner-server \
    ./cmd/server && \
    go build \
    -trimpath \
    -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" \
    -o /out/iam-realm-provisioner-reconciler \
    ./cmd/reconciler && \
    go build \
    -trimpath \
    -ldflags="-s -w -X main.buildVersion=${BUILD_VERSION}" \
    -o /out/migrate \
    ./cmd/migrate

########################################
# Stage: runtime
########################################
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS runtime

ARG BUILD_VERSION=dev
ENV BUILD_VERSION=${BUILD_VERSION}

LABEL org.opencontainers.image.title="iam-realm-provisioner" \
      org.opencontainers.image.description="Realm Provisioner Service — the single writer to Keycloak's Admin API and the system of record for tenant identity lifecycle (RP-1..RP-16, RP-C1..RP-C4, three SQS consumers)" \
      org.opencontainers.image.source="https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner" \
      org.opencontainers.image.vendor="BCBP Solutions" \
      org.opencontainers.image.licenses="Proprietary" \
      org.opencontainers.image.base.name="gcr.io/distroless/static-debian12:nonroot" \
      org.opencontainers.image.revision="${BUILD_VERSION}"

WORKDIR /

COPY --from=builder /out/iam-realm-provisioner-server /iam-realm-provisioner-server
COPY --from=builder /out/iam-realm-provisioner-reconciler /iam-realm-provisioner-reconciler
COPY --from=builder /out/migrate /migrate
COPY --from=builder /src/deploy/keycloak/realm-template.json /deploy/keycloak/realm-template.json

USER nonroot:nonroot

EXPOSE 8080 9090

ENTRYPOINT ["/iam-realm-provisioner-server"]
