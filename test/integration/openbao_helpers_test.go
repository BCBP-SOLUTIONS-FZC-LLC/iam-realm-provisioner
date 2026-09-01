//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	openbaoadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-realm-provisioner/internal/adapter/outbound/openbao"
)

// These constants mirror deploy/openbao/policy.hcl and
// deploy/openbao/role.tf.example verbatim (real production ACL + role
// binding), and BAO_AUTH_ROLE / BAO_KV_MOUNT / BAO_IDP_PATH_PREFIX from
// .env-example (§12) — the same "seam" convention keycloak_helpers_test.go
// uses (kcClientID/kcClientSecret) for its own bootstrap constants.
const (
	baoRoleName    = "iam-realm-provisioner"
	baoSANamespace = "iam"
	baoSAName      = "iam-realm-provisioner"
	baoKVMount     = "secret"
	baoIdpPrefix   = "iam/realm-provisioner/idp"
)

// baoPolicyHCL is deploy/openbao/policy.hcl's body, used verbatim (per the
// task) rather than re-derived, so a drift between the real production ACL
// and this test's fixture would have to be introduced in both places at
// once. Kept inline (not read from disk) so the test package has no
// runtime dependency on the repo's working directory.
const baoPolicyHCL = `
path "secret/data/iam/realm-provisioner/idp/*" {
  capabilities = ["create", "read", "update"]
}

path "secret/metadata/iam/realm-provisioner/idp/*" {
  capabilities = ["read", "delete", "list"]
}

path "auth/token/renew-self" {
  capabilities = ["update"]
}
`

var rootTokenRe = regexp.MustCompile(`Root Token: (\S+)`)

// openbaoEnv is everything a test needs to construct one or more
// openbao.Client instances against one real, fully-bootstrapped OpenBao
// container: KV v2 policy + Kubernetes auth (role bound to baoSAName/
// baoSANamespace) are already configured, and tokenPath already holds a
// real signed ServiceAccount JWT satisfying that role's bindings.
type openbaoEnv struct {
	addr      string
	rootToken string
	tokenPath string
}

// newClient builds an openbao.Client against env using idpPrefix as
// BAO_IDP_PATH_PREFIX — a distinct IdpPathPrefix from the policy's allowed
// prefix is how TestOpenBao_PermissionDenied_SurfacesRealError constructs a
// client whose Kubernetes-auth login still succeeds (same role, same real
// JWT) but whose KV requests fall outside the policy's granted paths.
func newClient(env *openbaoEnv, idpPrefix string) *openbaoadapter.Client {
	return openbaoadapter.New(openbaoadapter.Config{
		Addr:                env.addr,
		AuthRole:            baoRoleName,
		KVMount:             baoKVMount,
		IdpPathPrefix:       idpPrefix,
		KubernetesTokenPath: env.tokenPath,
	}, noopLogger{})
}

// setupOpenBao starts a real OpenBao dev-mode testcontainer and fully
// bootstraps it against real Kubernetes-auth: no live Kubernetes cluster is
// ever contacted. This repo's port.OpenBaoClient adapter (§12) always
// authenticates via OpenBao's Kubernetes auth method — the standard way to
// exercise that for real without a cluster is OpenBao/Vault's pem_keys
// static-JWT-validation config
// (auth/kubernetes/config's "pem_keys" field, meant to let the plugin
// verify a ServiceAccount JWT's RS256 signature locally and skip the
// TokenReview API call entirely).
//
// That was tried first here and does NOT work against openbao/openbao:
// latest (core v2.6.2, kubernetes auth plugin "v2.6.2+builtin.bao") —
// confirmed empirically (trace-level container logs) that the login
// handler unconditionally calls POST
// .../apis/authentication.k8s.io/v1/tokenreviews regardless of whether
// pem_keys is configured; only the JWT *parsing* (namespace/name/uid
// extraction for role-matching) happens locally. So this helper instead
// runs a real TokenReview flow: a tiny in-test HTTP server plays the part
// of the Kubernetes API server's TokenReview endpoint (always answering
// "authenticated: true" for the one ServiceAccount identity baked into our
// hand-signed JWT), exposed to the OpenBao container via testcontainers'
// WithHostPortAccess-equivalent HostAccessPorts field (an SSHD-sidecar
// tunnel reachable at testcontainers.HostInternal — portable to Linux CI,
// unlike Docker Desktop's host.docker.internal). OpenBao's own
// kubernetes-auth code — real bound-service-account-name/namespace role
// matching, real JWT claim parsing, real token issuance — runs exactly as
// it would against a live cluster; only the cluster's TokenReview response
// is stubbed.
func setupOpenBao(t *testing.T) *openbaoEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping openbao integration test in short mode")
	}
	ctx := context.Background()

	priv := genRSAKeypair(t)
	saUID := "11111111-1111-1111-1111-111111111111"
	jwt := mintServiceAccountJWT(t, priv, baoSANamespace, baoSAName, saUID)
	tokenPath := writeSAToken(t, jwt)

	trPort := startFakeTokenReviewServer(t, baoSANamespace, baoSAName, saUID)

	req := testcontainers.ContainerRequest{
		Image:           "openbao/openbao:latest",
		ExposedPorts:    []string{"8200/tcp"},
		Cmd:             []string{"server", "-dev"},
		HostAccessPorts: []int{trPort},
		WaitingFor:      wait.ForLog("Development mode should NOT be used in production installations!").WithStartupTimeout(60 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	mappedPort, err := container.MappedPort(ctx, "8200/tcp")
	require.NoError(t, err)
	host, err := container.Host(ctx)
	require.NoError(t, err)
	addr := fmt.Sprintf("http://%s:%s", host, mappedPort.Port())

	logsReader, err := container.Logs(ctx)
	require.NoError(t, err)
	logs, err := io.ReadAll(logsReader)
	require.NoError(t, err)
	m := rootTokenRe.FindSubmatch(logs)
	require.NotNil(t, m, "root token not found in openbao startup logs:\n%s", string(logs))
	rootToken := string(m[1])

	// KV v2 is already mounted at "secret/" by default in dev mode
	// (verified empirically) — no sys/mounts call needed.

	// Write the real production policy verbatim.
	baoDo(t, http.MethodPut, addr, rootToken, "/v1/sys/policies/acl/"+baoRoleName,
		map[string]any{"policy": baoPolicyHCL}, nil)

	// Enable Kubernetes auth.
	baoDo(t, http.MethodPost, addr, rootToken, "/v1/sys/auth/kubernetes",
		map[string]any{"type": "kubernetes"}, nil)

	// Configure it against the fake TokenReview stub. kubernetes_ca_cert
	// must be a well-formed PEM even though it is never actually used to
	// dial TLS here (the stub is plain HTTP) — OpenBao errors out at
	// config-write time if it's empty and no local in-cluster CA file
	// exists, confirmed empirically.
	baoDo(t, http.MethodPost, addr, rootToken, "/v1/auth/kubernetes/config",
		map[string]any{
			"kubernetes_host":        fmt.Sprintf("http://%s:%d", testcontainers.HostInternal, trPort),
			"kubernetes_ca_cert":     generateDummyCACertPEM(t),
			"disable_iss_validation": true,
		}, nil)

	// Role bound to the same ServiceAccount identity baked into our JWT —
	// deploy/openbao/role.tf.example's real binding, verbatim.
	baoDo(t, http.MethodPost, addr, rootToken, "/v1/auth/kubernetes/role/"+baoRoleName,
		map[string]any{
			"bound_service_account_names":      []string{baoSAName},
			"bound_service_account_namespaces": []string{baoSANamespace},
			"policies":                         []string{baoRoleName},
			"ttl":                              "1h",
		}, nil)

	return &openbaoEnv{addr: addr, rootToken: rootToken, tokenPath: tokenPath}
}

// genRSAKeypair returns a fresh RSA keypair used to sign a realistic
// ServiceAccount JWT (mintServiceAccountJWT). The public half would be the
// pem_keys value in OpenBao's auth/kubernetes/config static-JWT-validation
// mode — attempted first here and found not to skip the TokenReview call
// against openbao/openbao:latest (see setupOpenBao's doc comment), so only
// the private key is used, to sign the JWT the fake TokenReview stub then
// vouches for.
func genRSAKeypair(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return priv
}

// mintServiceAccountJWT hand-signs a realistic (legacy-format, pre-Kubernetes
// 1.21 "flat claim") ServiceAccount JWT: iss/sub plus the
// "kubernetes.io/serviceaccount/*" claim set OpenBao's kubernetes-auth
// plugin parses to identify the caller and match it against a role's
// bound_service_account_names/namespaces. Confirmed empirically against a
// real openbao/openbao container (§ setupOpenBao's doc comment) — in
// particular the UID claim key is
// "kubernetes.io/serviceaccount/service-account.uid", not the shorter
// "kubernetes.io/serviceaccount/uid" one might guess from the sibling
// claims' naming.
func mintServiceAccountJWT(t *testing.T, priv *rsa.PrivateKey, namespace, saName, uid string) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "typ": "JWT"}
	now := time.Now()
	claims := map[string]any{
		"iss":                                    "kubernetes/serviceaccount",
		"sub":                                    fmt.Sprintf("system:serviceaccount:%s:%s", namespace, saName),
		"kubernetes.io/serviceaccount/namespace": namespace,
		"kubernetes.io/serviceaccount/service-account.name": saName,
		"kubernetes.io/serviceaccount/service-account.uid":  uid,
		"kubernetes.io/serviceaccount/secret.name":          saName + "-token",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
	hb, err := json.Marshal(header)
	require.NoError(t, err)
	cb, err := json.Marshal(claims)
	require.NoError(t, err)
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	require.NoError(t, err)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// writeSAToken writes jwt to a temp file, returning its path — this becomes
// Config.KubernetesTokenPath, standing in for the projected ServiceAccount
// token volume Client.token() reads in a real pod.
func writeSAToken(t *testing.T, jwt string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte(jwt), 0o600))
	return path
}

// startFakeTokenReviewServer plays the part of the Kubernetes API server's
// TokenReview endpoint (see setupOpenBao's doc comment for why this exists
// instead of pem_keys-based static validation): it always answers
// "authenticated: true" for the single ServiceAccount identity this test
// package's JWT is signed for. It does not check the bearer token's
// signature — that's fine, since it isn't standing in for the JWT's
// cryptographic trust anchor here, only for the live cluster call OpenBao's
// kubernetes-auth plugin makes unconditionally; the JWT itself is still a
// genuinely RS256-signed, realistically-shaped ServiceAccount token, and it
// is OpenBao's own role bound_service_account_names/namespaces check
// (evaluated against the JWT's claims and this response's status.user) that
// gates the login.
func startFakeTokenReviewServer(t *testing.T, namespace, saName, uid string) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		resp := map[string]any{
			"apiVersion": "authentication.k8s.io/v1",
			"kind":       "TokenReview",
			"status": map[string]any{
				"authenticated": true,
				"user": map[string]any{
					"username": fmt.Sprintf("system:serviceaccount:%s:%s", namespace, saName),
					"uid":      uid,
					"groups":   []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return port
}

// generateDummyCACertPEM returns a throwaway self-signed certificate PEM —
// OpenBao's auth/kubernetes/config requires a non-empty kubernetes_ca_cert
// (or a locally-readable in-cluster CA file, which doesn't exist in this
// container) even though the fake TokenReview server in this test suite is
// plain HTTP and never actually dials TLS with it.
func generateDummyCACertPEM(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "openbao-integration-test-fake-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// ── root-token-authenticated raw HTTP helpers ───────────────────────────
//
// These bypass the adapter under test entirely — used both for real-server
// bootstrap (policy/auth/role setup, no Vault-API-compatible Go SDK is
// vendored in this repo, see go.mod) and as an independent verification
// channel in test assertions (e.g. "the secret really exists at this exact
// KV v2 path"), mirroring keycloak_helpers_test.go's rawAdminClient serving
// the same "fixture setup / independent check the port doesn't expose" role.

// baoDo issues a root-token-authenticated request against addr+path and, if
// out is non-nil, JSON-decodes a 2xx response body into it. It fails the
// test on any non-2xx status.
func baoDo(t *testing.T, method, addr, token, path string, body any, out any) {
	t.Helper()
	resp := baoDoRaw(t, method, addr, token, path, body)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Truef(t, resp.StatusCode < 300, "%s %s: unexpected status %d: %s", method, path, resp.StatusCode, string(b))
	if out != nil && len(b) > 0 {
		require.NoError(t, json.Unmarshal(b, out))
	}
}

// baoDoRaw is baoDo without the success assertion — for tests asserting on
// the status code itself (e.g. "reading a deleted secret is a real 404").
func baoDoRaw(t *testing.T, method, addr, token, path string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, addr+path, reader)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}
