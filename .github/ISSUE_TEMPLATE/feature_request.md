---
name: Feature request
about: Propose a new endpoint, domain field, event type, IdP capability, or reconciler behaviour
title: '[FEAT] '
labels: enhancement
assignees: ''
---

## Problem / motivation
What problem does this solve? Which consumers or use cases are affected?

## Proposed solution
Describe the API or behaviour change you'd like.

```go
// Example: new endpoint, payload struct, or domain type
```

## Affected areas
- [ ] HTTP API (new or changed endpoint)
- [ ] Domain model (new field or entity)
- [ ] Database schema (new migration required)
- [ ] Event contract (new event type or changed payload — asyncapi.yaml + internal/eventschema update needed)
- [ ] Keycloak admin adapter (new realm/user/IdP operation)
- [ ] OpenBao adapter (new secret path or KV shape)
- [ ] Reconciler / CronJob behaviour
- [ ] Helm / deployment config

## Event contract impact
If this adds or changes an event type, describe the downstream consumer impact (every subscriber of `iam.tenant.events`, and — for consumed events — the producing service's own contract).

## Alternatives considered
Other approaches you evaluated and why you ruled them out.

## Acceptance criteria
- [ ]
- [ ]
- [ ]

## Additional context
Links to related issues, LLD sections, ADRs, or prior art.
