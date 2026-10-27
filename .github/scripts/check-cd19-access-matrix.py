#!/usr/bin/env python3
"""Assert the CD-19 caller access matrix in a rendered chart.

CI renders and kubeconform-validates the chart (helm-check.sh), but nothing
checked WHICH routes each caller can reach. This script does, for one
rendered environment (helm-check.sh runs it for base+staging and base+prod):

  Istio AuthorizationPolicy (L7)
    * Billing  <trustDomain>/ns/billing/sa/commercial-billing
        exactly POST */convert, POST */revert-conversion, GET */realm
        (RP-3, RP-4, RP-11)
    * Signup BFF  <trustDomain>/ns/bff/sa/bff-signup
        exactly POST on the two exact provision paths (RP-1, RP-2)
    * namespace rules: exactly [iam, observability] — never gateway,
      billing, commercial or bff
    * no other rule
  NetworkPolicy (L4), every NetworkPolicy in the render
    * billing admitted only together with pod label commercial-billing
    * bff admitted only together with pod label bff-signup
    * nothing admits the commercial namespace (nor every namespace)
    * no namespace-only rule for bff or billing
  App layer (router.go)
    * the commercial-system group (RequireSystemOrCommercial) registers
      exactly RP-3/RP-4/RP-11
    * the Istio Billing / BFF rules, evaluated with Istio's path semantics
      against every route router.go registers, match exactly those routes
      (a new route ending in /realm would be caught here)
  The middleware itself is covered by Go tests:
    TestCD19_CommercialSystem_RouteMatrix (cd19_route_matrix_test.go — every
    internal route called as commercial-system), TestRequireSystemOrCommercial_*
    (middleware_test.go).

Usage:
  helm template ... | check-cd19-access-matrix.py --label base+prod \\
      --values deploy/helm/values.yaml --values deploy/helm/values-prod.yaml \\
      [--router internal/adapter/inbound/http/router.go]

The rendered manifest is read from stdin. --values files are merged in
order (as helm does) to read commercialCallers.istio.trustDomain and the
service port. Exit 1 with one line per violation.
"""
import argparse
import re
import sys

import yaml

TRUST_DOMAIN_KEY = ("commercialCallers", "istio", "trustDomain")

BILLING_NS, BILLING_SA, BILLING_POD = "billing", "commercial-billing", "commercial-billing"
BFF_NS, BFF_SA, BFF_POD = "bff", "bff-signup", "bff-signup"
ALLOWED_CALLER_NAMESPACES = {"iam", "observability"}
FORBIDDEN_CALLER_NAMESPACES = {"gateway", "billing", "commercial", "bff"}
NS_LABEL = "kubernetes.io/metadata.name"
POD_LABEL = "app.kubernetes.io/name"

BILLING_OPS = {
    ("POST", "*/convert"),
    ("POST", "*/revert-conversion"),
    ("GET", "*/realm"),
}
BFF_OPS = {
    ("POST", "/api/v1/internal/tenants/provision-trial"),
    ("POST", "/api/v1/internal/tenants/provision-direct"),
}
# router.go routes each Istio rule must reach — no more, no fewer.
BILLING_ROUTES = {
    ("POST", "/api/v1/internal/tenants/:id/convert"),            # RP-3
    ("POST", "/api/v1/internal/tenants/:id/revert-conversion"),  # RP-4
    ("GET", "/api/v1/internal/tenants/:id/realm"),               # RP-11
}
BFF_ROUTES = {
    ("POST", "/api/v1/internal/tenants/provision-trial"),   # RP-1
    ("POST", "/api/v1/internal/tenants/provision-direct"),  # RP-2
}
COMMERCIAL_GROUP_MIDDLEWARE = "RequireSystemOrCommercial"


def merge(base, overlay):
    out = dict(base)
    for k, v in (overlay or {}).items():
        if isinstance(v, dict) and isinstance(out.get(k), dict):
            out[k] = merge(out[k], v)
        else:
            out[k] = v
    return out


def dig(d, keys):
    for k in keys:
        if not isinstance(d, dict) or k not in d:
            return None
        d = d[k]
    return d


def istio_path_match(pattern, path):
    """Istio's string match for request paths: exact, prefix "abc*",
    suffix "*abc", or presence "*". A "*" anywhere else is literal."""
    if pattern == "*":
        return True
    if pattern.endswith("*"):
        return path.startswith(pattern[:-1])
    if pattern.startswith("*"):
        return path.endswith(pattern[1:])
    return path == pattern


def ops_of(rule, errors, who):
    """Flatten a rule's `to` into {(method, path)}; reject any operation
    field other than methods/paths (notPaths, hosts, ports ... would make
    the comparison meaningless)."""
    ops = set()
    for entry in rule.get("to") or []:
        op = entry.get("operation") or {}
        extra = set(op) - {"methods", "paths"}
        if extra:
            errors.append(f"{who}: unexpected operation fields {sorted(extra)}")
        methods, paths = op.get("methods"), op.get("paths")
        if not methods or not paths:
            errors.append(f"{who}: an operation without both methods and paths admits too much: {op}")
            continue
        for m in methods:
            for p in paths:
                ops.add((m, p))
    return ops


def check_authorization_policy(docs, trust_domain, errors):
    pols = [d for d in docs if d.get("kind") == "AuthorizationPolicy"]
    if len(pols) != 1:
        errors.append(f"AuthorizationPolicy: expected exactly 1 (Istio must be on), found {len(pols)}")
        return None
    spec = pols[0].get("spec") or {}
    if spec.get("action", "ALLOW") != "ALLOW":
        errors.append(f"AuthorizationPolicy: action must be ALLOW, got {spec.get('action')}")

    billing_principal = f"{trust_domain}/ns/{BILLING_NS}/sa/{BILLING_SA}"
    bff_principal = f"{trust_domain}/ns/{BFF_NS}/sa/{BFF_SA}"
    namespaces_seen = set()
    by_principal = {}

    for i, rule in enumerate(spec.get("rules") or []):
        who = f"AuthorizationPolicy rule[{i}]"
        if rule.get("when"):
            errors.append(f"{who}: unexpected `when` condition")
        sources = [f.get("source") or {} for f in rule.get("from") or []]
        if not sources:
            errors.append(f"{who}: no `from` — it would admit every caller")
            continue
        for src in sources:
            extra = set(src) - {"namespaces", "principals"}
            if extra:
                errors.append(f"{who}: unexpected source fields {sorted(extra)}")
            if src.get("namespaces") and src.get("principals"):
                errors.append(f"{who}: mixes namespaces and principals in one source")
            for ns in src.get("namespaces") or []:
                namespaces_seen.add(ns)
                if rule.get("to"):
                    errors.append(f"{who}: namespace rule for {ns!r} is path-restricted; expected a plain namespace rule")
            for p in src.get("principals") or []:
                if p in by_principal:
                    errors.append(f"{who}: principal {p} appears in more than one rule")
                by_principal[p] = ops_of(rule, errors, f"{who} ({p})")
            if not src.get("namespaces") and not src.get("principals"):
                errors.append(f"{who}: source with neither namespaces nor principals admits every caller")

    bad_ns = namespaces_seen & FORBIDDEN_CALLER_NAMESPACES
    if bad_ns:
        errors.append(f"AuthorizationPolicy: namespace rule admits {sorted(bad_ns)} (never a namespace-wide caller)")
    if namespaces_seen != ALLOWED_CALLER_NAMESPACES:
        errors.append(f"AuthorizationPolicy: namespace rules are {sorted(namespaces_seen)}, expected {sorted(ALLOWED_CALLER_NAMESPACES)}")

    rendered = {"billing": by_principal.get(billing_principal, set()),
                "bff": by_principal.get(bff_principal, set())}
    for principal, want, label in ((billing_principal, BILLING_OPS, "Billing"),
                                   (bff_principal, BFF_OPS, "Signup BFF")):
        got = by_principal.pop(principal, None)
        if got is None:
            errors.append(f"AuthorizationPolicy: no rule for the {label} principal {principal}")
        elif got != want:
            errors.append(f"AuthorizationPolicy: {label} ({principal}) gets {sorted(got)}, expected exactly {sorted(want)}")
    for principal in sorted(by_principal):
        errors.append(f"AuthorizationPolicy: unexpected principal rule {principal}")
    return rendered


def peer_namespace(peer, who, errors):
    """The single namespace a NetworkPolicy peer's namespaceSelector names,
    or "*" for a selector that matches every namespace."""
    sel = peer.get("namespaceSelector")
    if sel is None:
        return None
    labels = sel.get("matchLabels") or {}
    if sel.get("matchExpressions") or set(labels) - {NS_LABEL}:
        errors.append(f"{who}: namespaceSelector {sel} is not a single {NS_LABEL} match — cannot verify it")
        return "?"
    return labels.get(NS_LABEL, "*")


def check_network_policies(docs, service_port, errors):
    nps = [d for d in docs if d.get("kind") == "NetworkPolicy"]
    if not nps:
        errors.append("NetworkPolicy: none rendered")
        return
    admitted = {BILLING_NS: False, BFF_NS: False}
    for np in nps:
        name = np["metadata"]["name"]
        for i, rule in enumerate((np.get("spec") or {}).get("ingress") or []):
            for j, peer in enumerate(rule.get("from") or []):
                who = f"NetworkPolicy {name} ingress[{i}].from[{j}]"
                if "ipBlock" in peer:
                    errors.append(f"{who}: ipBlock peer can admit any namespace, including commercial")
                ns = peer_namespace(peer, who, errors)
                if ns is None:
                    continue  # podSelector only: same namespace (iam)
                if ns == "*":
                    errors.append(f"{who}: namespaceSelector matches EVERY namespace (admits commercial)")
                    continue
                if ns == "commercial":
                    errors.append(f"{who}: admits the commercial namespace")
                pods = (peer.get("podSelector") or {}).get("matchLabels") or {}
                for target_ns, pod in ((BILLING_NS, BILLING_POD), (BFF_NS, BFF_POD)):
                    if ns != target_ns:
                        continue
                    if not pods:
                        errors.append(f"{who}: namespace-only rule for {target_ns} (must also select {POD_LABEL}={pod})")
                    elif pods != {POD_LABEL: pod} or (peer["podSelector"].get("matchExpressions")):
                        errors.append(f"{who}: {target_ns} admitted with pod selector {peer['podSelector']}, expected {POD_LABEL}={pod} only")
                    else:
                        ports = {p.get("port") for p in rule.get("ports") or []}
                        if ports != {service_port}:
                            errors.append(f"{who}: {target_ns} admitted on ports {sorted(ports, key=str)}, expected [{service_port}]")
                        admitted[target_ns] = True
    for ns, pod in ((BILLING_NS, BILLING_POD), (BFF_NS, BFF_POD)):
        if not admitted[ns]:
            errors.append(f"NetworkPolicy: {ns}/{pod} is not admitted to the API port (expected by CD-19 / BFF-1)")


ROUTE_RE = re.compile(r'^\s*(\w+)\.(GET|POST|PUT|PATCH|DELETE)\("([^"]+)"', re.M)
GROUP_RE = re.compile(r'^\s*(\w+)\s*:=\s*r\.Group\("([^"]+)"(.*)$', re.M)


def parse_router(path, errors):
    src = open(path, encoding="utf-8").read()
    groups = {"r": ("", "")}
    for var, prefix, rest in GROUP_RE.findall(src):
        groups[var] = (prefix, rest)
    routes, commercial = set(), set()
    for var, method, sub in ROUTE_RE.findall(src):
        if var not in groups:
            errors.append(f"router.go: route {method} {sub} on unknown group {var!r}")
            continue
        prefix, mw = groups[var]
        full = prefix + sub
        routes.add((method, full))
        if COMMERCIAL_GROUP_MIDDLEWARE in mw:
            commercial.add((method, full))
    if not routes:
        errors.append("router.go: no routes parsed — the parser is out of date")
    return routes, commercial


def check_app_layer(router_path, rendered_ops, errors):
    """rendered_ops: the (method, path) pairs each Istio principal rule
    actually grants in this render (None when no policy rendered)."""
    routes, commercial = parse_router(router_path, errors)
    if commercial != BILLING_ROUTES:
        errors.append(f"router.go: commercial-system group registers {sorted(commercial)}, expected exactly {sorted(BILLING_ROUTES)}")
    if rendered_ops is None:
        return
    for label, ops, want in (("Billing", rendered_ops["billing"], BILLING_ROUTES),
                             ("Signup BFF", rendered_ops["bff"], BFF_ROUTES)):
        matched = set()
        for method, route in routes:
            concrete = re.sub(r"/[:*][^/]+", "/00000000-0000-0000-0000-000000000000", route)
            if any(m == method and istio_path_match(p, concrete) for m, p in ops):
                matched.add((method, route))
        if matched != want:
            errors.append(f"Istio {label} rule reaches {sorted(matched)} in router.go, expected exactly {sorted(want)}")


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--label", required=True)
    ap.add_argument("--values", action="append", required=True)
    ap.add_argument("--router", default="internal/adapter/inbound/http/router.go")
    args = ap.parse_args()

    values = {}
    for f in args.values:
        values = merge(values, yaml.safe_load(open(f, encoding="utf-8")) or {})
    trust_domain = dig(values, TRUST_DOMAIN_KEY) or "cluster.local"
    service_port = dig(values, ("service", "port")) or 8080

    docs = [d for d in yaml.safe_load_all(sys.stdin) if isinstance(d, dict)]
    errors = []
    ops = check_authorization_policy(docs, trust_domain, errors)
    check_network_policies(docs, service_port, errors)
    check_app_layer(args.router, ops, errors)

    if errors:
        for e in errors:
            print(f"::error title=CD-19 access matrix [{args.label}]::{e}")
        print(f"  ✘  CD-19 access matrix [{args.label}]: {len(errors)} violation(s)")
        return 1
    print(f"  ✔  CD-19 access matrix [{args.label}]: Billing RP-3/RP-4/RP-11, "
          f"BFF RP-1/RP-2, namespaces {sorted(ALLOWED_CALLER_NAMESPACES)}, NetworkPolicy, router.go")
    return 0


if __name__ == "__main__":
    sys.exit(main())
