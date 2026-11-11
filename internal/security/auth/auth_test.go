package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

const (
	testIssuer   = "https://issuer.patchproof.test"
	testAudience = "patchproof-test"
)

type testKey struct {
	kid string
	rsa *rsa.PrivateKey
	ec  *ecdsa.PrivateKey
}

func (k testKey) algorithm() string {
	if k.rsa != nil {
		return "RS256"
	}
	return "ES256"
}

func newRSAKey(t *testing.T, kid string) testKey {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return testKey{kid: kid, rsa: private}
}

func newECKey(t *testing.T, kid string) testKey {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	return testKey{kid: kid, ec: private}
}

func padCoordinate(value *big.Int) []byte {
	raw := value.Bytes()
	out := make([]byte, 32)
	if len(raw) > len(out) {
		return raw
	}
	copy(out[len(out)-len(raw):], raw)
	return out
}

func jwksJSON(t *testing.T, keys ...testKey) []byte {
	t.Helper()
	type jwk struct {
		KeyType   string `json:"kty"`
		KeyID     string `json:"kid"`
		Use       string `json:"use"`
		Algorithm string `json:"alg"`
		N         string `json:"n,omitempty"`
		E         string `json:"e,omitempty"`
		Curve     string `json:"crv,omitempty"`
		X         string `json:"x,omitempty"`
		Y         string `json:"y,omitempty"`
	}
	document := struct {
		Keys []jwk `json:"keys"`
	}{Keys: make([]jwk, 0, len(keys))}
	for _, key := range keys {
		entry := jwk{KeyID: key.kid, Use: "sig", Algorithm: key.algorithm()}
		if key.rsa != nil {
			entry.KeyType = "RSA"
			entry.N = base64.RawURLEncoding.EncodeToString(key.rsa.PublicKey.N.Bytes())
			entry.E = base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.rsa.PublicKey.E)).Bytes())
		} else {
			entry.KeyType = "EC"
			entry.Curve = "P-256"
			entry.X = base64.RawURLEncoding.EncodeToString(padCoordinate(key.ec.PublicKey.X))
			entry.Y = base64.RawURLEncoding.EncodeToString(padCoordinate(key.ec.PublicKey.Y))
		}
		document.Keys = append(document.Keys, entry)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}
	return raw
}

func encodeSegment(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JWT segment: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func signedJWT(t *testing.T, key testKey, claims map[string]any) string {
	t.Helper()
	header := encodeSegment(t, map[string]any{"alg": key.algorithm(), "kid": key.kid, "typ": "JWT"})
	payload := encodeSegment(t, claims)
	signed := header + "." + payload
	digest := sha256.Sum256([]byte(signed))
	var signature []byte
	switch {
	case key.rsa != nil:
		var err error
		signature, err = rsa.SignPKCS1v15(rand.Reader, key.rsa, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatalf("sign RS256 JWT: %v", err)
		}
	case key.ec != nil:
		r, s, err := ecdsa.Sign(rand.Reader, key.ec, digest[:])
		if err != nil {
			t.Fatalf("sign ES256 JWT: %v", err)
		}
		signature = append(padCoordinate(r), padCoordinate(s)...)
	default:
		t.Fatal("test key has neither RSA nor EC material")
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func unsignedJWT(t *testing.T, algorithm, kid string, claims map[string]any, signature []byte) string {
	t.Helper()
	header := encodeSegment(t, map[string]any{"alg": algorithm, "kid": kid, "typ": "JWT"})
	payload := encodeSegment(t, claims)
	return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func baseClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss":       testIssuer,
		"aud":       testAudience,
		"sub":       "user-123",
		"tenant_id": domain.NewTenantID().String(),
		"exp":       now.Add(10 * time.Minute).Unix(),
		"nbf":       now.Add(-time.Minute).Unix(),
		"iat":       now.Add(-time.Minute).Unix(),
		"roles":     []string{"reviewer"},
		"groups":    []string{"team-a"},
		"scope":     "runs:read runs:write",
	}
}

func bearerRequest(token string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func requireTokenError(t *testing.T, err error, wantReason string) *TokenError {
	t.Helper()
	var tokenErr *TokenError
	if !errors.As(err, &tokenErr) {
		t.Fatalf("error = %v (%T), want *TokenError", err, err)
	}
	if wantReason != "" && !strings.Contains(tokenErr.Reason, wantReason) {
		t.Fatalf("TokenError reason = %q, want it to mention %q", tokenErr.Reason, wantReason)
	}
	return tokenErr
}

type jwksServer struct {
	server *httptest.Server
	mu     sync.Mutex
	body   []byte
	hits   int
}

func newJWKSServer(t *testing.T, body []byte) *jwksServer {
	t.Helper()
	fixture := &jwksServer{body: body}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jwks" {
			http.NotFound(w, r)
			return
		}
		fixture.mu.Lock()
		fixture.hits++
		body := append([]byte(nil), fixture.body...)
		fixture.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (s *jwksServer) url() string { return s.server.URL + "/jwks" }

func (s *jwksServer) setBody(body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body = body
}

func (s *jwksServer) hitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func newOIDCAuthenticator(t *testing.T, jwksURL string, now func() time.Time, ttl time.Duration) *OIDCAuthenticator {
	t.Helper()
	authenticator, err := NewOIDC(OIDCConfig{
		Issuer:     testIssuer,
		Audience:   testAudience,
		JWKSURL:    jwksURL,
		ClockSkew:  time.Minute,
		CacheTTL:   ttl,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Now:        now,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	return authenticator
}

func TestDevAuthenticatorAcceptsConfiguredToken(t *testing.T) {
	t.Parallel()
	tenantID := domain.NewTenantID()
	authenticator := &DevAuthenticator{
		Token: "secret-token", TenantID: tenantID, Subject: "dev-user", Roles: []string{"reviewer"},
	}
	before := time.Now()
	principal, err := authenticator.Authenticate(t.Context(), bearerRequest("secret-token"))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal.Subject != "dev-user" {
		t.Fatalf("Subject = %q", principal.Subject)
	}
	if principal.TenantID != tenantID {
		t.Fatalf("TenantID = %q, want %q", principal.TenantID, tenantID)
	}
	for _, role := range []string{"admin", "operator", "viewer", "reviewer"} {
		if !principal.HasRole(role) {
			t.Fatalf("principal is missing role %q: %#v", role, principal.Roles)
		}
	}
	if !principal.HasRole("ADMIN") {
		t.Fatal("HasRole should be case-insensitive")
	}
	for _, scope := range []string{"runs:write", "runs:read"} {
		if !principal.HasScope(scope) {
			t.Fatalf("principal is missing scope %q: %#v", scope, principal.Scopes)
		}
	}
	if principal.HasScope("runs:admin") {
		t.Fatal("principal unexpectedly has runs:admin scope")
	}
	if principal.ExpiresAt.Before(before.Add(23 * time.Hour)) {
		t.Fatalf("ExpiresAt = %v, want roughly 24h from now", principal.ExpiresAt)
	}
}

func TestDevAuthenticatorRejectsBadHeaders(t *testing.T) {
	t.Parallel()
	tenantID := domain.NewTenantID()
	authenticator := &DevAuthenticator{Token: "secret-token", TenantID: tenantID, Subject: "dev-user"}
	tests := []struct {
		name       string
		header     string
		wantReason string
	}{
		{name: "missing header", header: "", wantReason: "missing authorization header"},
		{name: "wrong scheme", header: "Basic secret-token", wantReason: "Bearer scheme"},
		{name: "empty token", header: "Bearer   ", wantReason: "empty bearer token"},
		{name: "bad token", header: "Bearer other-token", wantReason: "invalid token"},
		{name: "no separator", header: "Bearersecret-token", wantReason: "Bearer scheme"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}
			principal, err := authenticator.Authenticate(t.Context(), request)
			if err == nil {
				t.Fatalf("Authenticate accepted bad header and returned %+v", principal)
			}
			requireTokenError(t, err, test.wantReason)
		})
	}
}

func TestDevAuthenticatorRequiresConfiguration(t *testing.T) {
	t.Parallel()
	var unconfigured *DevAuthenticator
	if _, err := unconfigured.Authenticate(t.Context(), bearerRequest("token")); err == nil {
		t.Fatal("nil authenticator accepted a request")
	} else {
		requireTokenError(t, err, "not configured")
	}
	empty := &DevAuthenticator{TenantID: domain.NewTenantID()}
	if _, err := empty.Authenticate(t.Context(), bearerRequest("token")); err == nil {
		t.Fatal("empty configured token accepted a request")
	} else {
		requireTokenError(t, err, "not configured")
	}
	invalidTenant := &DevAuthenticator{Token: "secret-token", TenantID: "not-a-uuid"}
	if _, err := invalidTenant.Authenticate(t.Context(), bearerRequest("secret-token")); err == nil {
		t.Fatal("invalid tenant was accepted")
	} else {
		requireTokenError(t, err, "tenant is not configured")
	}
}

func TestOIDCValidTokens(t *testing.T) {
	fixed := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	rsaKey := newRSAKey(t, "rsa-1")
	ecKey := newECKey(t, "ec-1")
	server := newJWKSServer(t, jwksJSON(t, rsaKey, ecKey))
	current := fixed
	authenticator := newOIDCAuthenticator(t, server.url(), func() time.Time { return current }, 10*time.Minute)

	claims := baseClaims(fixed)
	token := signedJWT(t, rsaKey, claims)
	principal, err := authenticator.Authenticate(t.Context(), bearerRequest(token))
	if err != nil {
		t.Fatalf("Authenticate RS256: %v", err)
	}
	if principal.Subject != "user-123" {
		t.Fatalf("Subject = %q", principal.Subject)
	}
	if principal.TenantID.String() != claims["tenant_id"].(string) {
		t.Fatalf("TenantID = %q, want %q", principal.TenantID, claims["tenant_id"])
	}
	if principal.Issuer != testIssuer {
		t.Fatalf("Issuer = %q", principal.Issuer)
	}
	if !principal.HasRole("reviewer") || !principal.HasRole("TEAM-A") {
		t.Fatalf("roles = %#v, want reviewer and team-a", principal.Roles)
	}
	if !principal.HasScope("runs:read") || !principal.HasScope("runs:write") {
		t.Fatalf("scopes = %#v", principal.Scopes)
	}
	if principal.HasScope("runs:admin") {
		t.Fatalf("unexpected scope: %#v", principal.Scopes)
	}
	if got, want := principal.ExpiresAt.Unix(), int64(claims["exp"].(int64)); got != want {
		t.Fatalf("ExpiresAt = %d, want %d", got, want)
	}
	if server.hitCount() != 1 {
		t.Fatalf("JWKS hits after first token = %d, want 1", server.hitCount())
	}

	// A second token with the same kid is served from the in-memory cache.
	if _, err := authenticator.Authenticate(t.Context(), bearerRequest(token)); err != nil {
		t.Fatalf("Authenticate cached RS256: %v", err)
	}
	if server.hitCount() != 1 {
		t.Fatalf("JWKS hits after cached token = %d, want 1", server.hitCount())
	}

	// ES256 tokens use the EC key from the same JWKS document without refetching.
	esToken := signedJWT(t, ecKey, baseClaims(fixed))
	principal, err = authenticator.Authenticate(t.Context(), bearerRequest(esToken))
	if err != nil {
		t.Fatalf("Authenticate ES256: %v", err)
	}
	if principal.Subject != "user-123" || !principal.HasRole("reviewer") {
		t.Fatalf("unexpected ES256 principal: %+v", principal)
	}

	// aud may be an array containing the expected audience.
	arrayClaims := baseClaims(fixed)
	arrayClaims["aud"] = []string{"other-audience", testAudience}
	if _, err := authenticator.Authenticate(t.Context(), bearerRequest(signedJWT(t, rsaKey, arrayClaims))); err != nil {
		t.Fatalf("Authenticate with aud array: %v", err)
	}

	// Roles may also be encoded as a space-separated string.
	stringClaims := baseClaims(fixed)
	stringClaims["roles"] = "viewer auditor"
	principal, err = authenticator.Authenticate(t.Context(), bearerRequest(signedJWT(t, rsaKey, stringClaims)))
	if err != nil {
		t.Fatalf("Authenticate with string roles: %v", err)
	}
	if !principal.HasRole("viewer") || !principal.HasRole("auditor") {
		t.Fatalf("roles = %#v, want viewer and auditor", principal.Roles)
	}

	if server.hitCount() != 1 {
		t.Fatalf("JWKS hits after cached variants = %d, want 1", server.hitCount())
	}
}

func TestOIDCRejectsInvalidTokens(t *testing.T) {
	fixed := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	rsaKey := newRSAKey(t, "rsa-1")
	server := newJWKSServer(t, jwksJSON(t, rsaKey))
	authenticator := newOIDCAuthenticator(t, server.url(), func() time.Time { return fixed }, 10*time.Minute)
	attacker := newRSAKey(t, "rsa-1")

	tests := []struct {
		name       string
		token      func() string
		wantReason string
	}{
		{
			name: "wrong issuer",
			token: func() string {
				claims := baseClaims(fixed)
				claims["iss"] = "https://evil.example"
				return signedJWT(t, rsaKey, claims)
			},
			wantReason: "issuer mismatch",
		},
		{
			name: "wrong audience",
			token: func() string {
				claims := baseClaims(fixed)
				claims["aud"] = "another-service"
				return signedJWT(t, rsaKey, claims)
			},
			wantReason: "audience mismatch",
		},
		{
			name: "audience array without match",
			token: func() string {
				claims := baseClaims(fixed)
				claims["aud"] = []string{"one", "two"}
				return signedJWT(t, rsaKey, claims)
			},
			wantReason: "audience mismatch",
		},
		{
			name: "expired",
			token: func() string {
				claims := baseClaims(fixed)
				claims["exp"] = fixed.Add(-2 * time.Minute).Unix()
				return signedJWT(t, rsaKey, claims)
			},
			wantReason: "expired",
		},
		{
			name: "missing exp",
			token: func() string {
				claims := baseClaims(fixed)
				delete(claims, "exp")
				return signedJWT(t, rsaKey, claims)
			},
			wantReason: "expired",
		},
		{
			name: "nbf in future",
			token: func() string {
				claims := baseClaims(fixed)
				claims["nbf"] = fixed.Add(2 * time.Minute).Unix()
				return signedJWT(t, rsaKey, claims)
			},
			wantReason: "not yet valid",
		},
		{
			name: "iat in future",
			token: func() string {
				claims := baseClaims(fixed)
				claims["iat"] = fixed.Add(2 * time.Minute).Unix()
				return signedJWT(t, rsaKey, claims)
			},
			wantReason: "issued in the future",
		},
		{
			name:       "bad signature",
			token:      func() string { return signedJWT(t, attacker, baseClaims(fixed)) },
			wantReason: "signature verification failed",
		},
		{
			name:       "unsupported HS256",
			token:      func() string { return unsignedJWT(t, "HS256", "rsa-1", baseClaims(fixed), []byte("signature")) },
			wantReason: "unsupported signing algorithm",
		},
		{
			name:       "unsupported none",
			token:      func() string { return unsignedJWT(t, "none", "rsa-1", baseClaims(fixed), nil) },
			wantReason: "unsupported signing algorithm",
		},
		{
			name: "missing tenant",
			token: func() string {
				claims := baseClaims(fixed)
				delete(claims, "tenant_id")
				return signedJWT(t, rsaKey, claims)
			},
			wantReason: "tenant claim is missing or invalid",
		},
		{
			name: "invalid tenant",
			token: func() string {
				claims := baseClaims(fixed)
				claims["tenant_id"] = "not-a-uuid"
				return signedJWT(t, rsaKey, claims)
			},
			wantReason: "tenant claim is missing or invalid",
		},
		{
			name: "numeric tenant",
			token: func() string {
				claims := baseClaims(fixed)
				claims["tenant_id"] = 42
				return signedJWT(t, rsaKey, claims)
			},
			wantReason: "tenant claim is missing or invalid",
		},
		{
			name: "missing subject",
			token: func() string {
				claims := baseClaims(fixed)
				delete(claims, "sub")
				return signedJWT(t, rsaKey, claims)
			},
			wantReason: "subject claim is missing",
		},
		{
			name:       "malformed token",
			token:      func() string { return "not-a-jwt" },
			wantReason: "malformed token",
		},
		{
			name: "malformed signature",
			token: func() string {
				header := encodeSegment(t, map[string]any{"alg": "RS256", "kid": "rsa-1"})
				payload := encodeSegment(t, baseClaims(fixed))
				return header + "." + payload + ".%%%"
			},
			wantReason: "malformed token signature",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			principal, err := authenticator.Authenticate(t.Context(), bearerRequest(test.token()))
			if err == nil {
				t.Fatalf("Authenticate accepted invalid token and returned %+v", principal)
			}
			requireTokenError(t, err, test.wantReason)
		})
	}
}

func TestOIDCUnknownKeyIDTriggersRefresh(t *testing.T) {
	fixed := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	first := newRSAKey(t, "rsa-1")
	second := newRSAKey(t, "rsa-2")
	server := newJWKSServer(t, jwksJSON(t, first))
	current := fixed
	authenticator := newOIDCAuthenticator(t, server.url(), func() time.Time { return current }, 10*time.Minute)
	token := signedJWT(t, second, baseClaims(fixed))

	// The first attempt fetches the JWKS once, misses the unknown kid, and fails.
	if _, err := authenticator.Authenticate(t.Context(), bearerRequest(token)); err == nil {
		t.Fatal("token with unknown kid was accepted")
	} else {
		requireTokenError(t, err, "signature verification failed")
	}
	if got := server.hitCount(); got != 1 {
		t.Fatalf("JWKS hits after unknown kid = %d, want 1", got)
	}

	// Once the key is published, the miss triggers a JWKS refresh and succeeds.
	server.setBody(jwksJSON(t, first, second))
	current = fixed.Add(2 * time.Second)
	principal, err := authenticator.Authenticate(t.Context(), bearerRequest(token))
	if err != nil {
		t.Fatalf("Authenticate after key publication: %v", err)
	}
	if principal.Subject != "user-123" {
		t.Fatalf("Subject = %q", principal.Subject)
	}
	if got := server.hitCount(); got != 2 {
		t.Fatalf("JWKS hits after refresh = %d, want 2", got)
	}

	// The refreshed key is cached.
	if _, err := authenticator.Authenticate(t.Context(), bearerRequest(token)); err != nil {
		t.Fatalf("Authenticate cached refreshed token: %v", err)
	}
	if got := server.hitCount(); got != 2 {
		t.Fatalf("JWKS hits after cached refresh = %d, want 2", got)
	}
}

func TestOIDCJWKSCacheTTLAndKeyRotation(t *testing.T) {
	fixed := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	// Rotation reuses the same kid with new key material.
	oldKey := newRSAKey(t, "rotate-1")
	newKey := newRSAKey(t, "rotate-1")
	server := newJWKSServer(t, jwksJSON(t, oldKey))
	current := fixed
	authenticator := newOIDCAuthenticator(t, server.url(), func() time.Time { return current }, time.Minute)

	oldToken := signedJWT(t, oldKey, baseClaims(fixed))
	if _, err := authenticator.Authenticate(t.Context(), bearerRequest(oldToken)); err != nil {
		t.Fatalf("Authenticate old token: %v", err)
	}
	if got := server.hitCount(); got != 1 {
		t.Fatalf("JWKS hits = %d, want 1", got)
	}
	if _, err := authenticator.Authenticate(t.Context(), bearerRequest(oldToken)); err != nil {
		t.Fatalf("Authenticate cached old token: %v", err)
	}
	if got := server.hitCount(); got != 1 {
		t.Fatalf("cached token refetched JWKS: hits = %d, want 1", got)
	}

	newToken := signedJWT(t, newKey, baseClaims(fixed))
	server.setBody(jwksJSON(t, newKey))

	// Before the TTL expires, the cached (stale) key is reused and the new
	// token fails without another fetch.
	if _, err := authenticator.Authenticate(t.Context(), bearerRequest(newToken)); err == nil {
		t.Fatal("token signed by a rotated key was accepted before cache expiry")
	} else {
		requireTokenError(t, err, "signature verification failed")
	}
	if got := server.hitCount(); got != 1 {
		t.Fatalf("JWKS hits before TTL expiry = %d, want 1", got)
	}

	// After the TTL, the failed verification forces a refetch and the rotated
	// key verifies.
	current = fixed.Add(2 * time.Minute)
	if _, err := authenticator.Authenticate(t.Context(), bearerRequest(newToken)); err != nil {
		t.Fatalf("Authenticate rotated token after TTL: %v", err)
	}
	if got := server.hitCount(); got != 2 {
		t.Fatalf("JWKS hits after TTL refresh = %d, want 2", got)
	}
	if _, err := authenticator.Authenticate(t.Context(), bearerRequest(newToken)); err != nil {
		t.Fatalf("Authenticate rotated token from cache: %v", err)
	}
	if got := server.hitCount(); got != 2 {
		t.Fatalf("rotated token refetched JWKS: hits = %d, want 2", got)
	}
}

func TestOIDCCustomClaimsAndDefaults(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	key := newECKey(t, "ec-custom")
	server := newJWKSServer(t, jwksJSON(t, key))
	authenticator, err := NewOIDC(OIDCConfig{
		Issuer: testIssuer, Audience: testAudience, JWKSURL: server.url(),
		TenantClaim: "org_id", RolesClaim: "permissions", GroupsClaim: "teams",
		ClockSkew: time.Minute, CacheTTL: time.Minute, Now: func() time.Time { return fixed },
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	claims := baseClaims(fixed)
	tenant := claims["tenant_id"]
	delete(claims, "tenant_id")
	claims["org_id"] = tenant
	delete(claims, "roles")
	claims["permissions"] = []string{"deployer"}
	delete(claims, "groups")
	claims["teams"] = []string{"blue"}
	principal, err := authenticator.Authenticate(t.Context(), bearerRequest(signedJWT(t, key, claims)))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal.TenantID.String() != tenant.(string) {
		t.Fatalf("TenantID = %q, want %q", principal.TenantID, tenant)
	}
	if !principal.HasRole("deployer") || !principal.HasRole("blue") {
		t.Fatalf("roles = %#v, want deployer and blue", principal.Roles)
	}

	// Unset optional OIDC settings receive safe defaults.
	defaulted, err := NewOIDC(OIDCConfig{Issuer: testIssuer, Audience: testAudience, JWKSURL: server.url()})
	if err != nil {
		t.Fatalf("NewOIDC defaults: %v", err)
	}
	if defaulted.config.TenantClaim != "tenant_id" || defaulted.config.RolesClaim != "roles" || defaulted.config.GroupsClaim != "groups" {
		t.Fatalf("unexpected claim defaults: %+v", defaulted.config)
	}
	if defaulted.config.ClockSkew != time.Minute || defaulted.config.CacheTTL != 10*time.Minute {
		t.Fatalf("unexpected timing defaults: %+v", defaulted.config)
	}
	if defaulted.config.Now == nil || defaulted.config.HTTPClient == nil {
		t.Fatal("Now and HTTPClient should be defaulted")
	}
}

func TestNewOIDCRequiresConfiguration(t *testing.T) {
	t.Parallel()
	tests := []OIDCConfig{
		{Audience: testAudience, JWKSURL: "https://example.com/jwks"},
		{Issuer: testIssuer, JWKSURL: "https://example.com/jwks"},
		{Issuer: testIssuer, Audience: testAudience},
	}
	for i, cfg := range tests {
		if _, err := NewOIDC(cfg); err == nil {
			t.Fatalf("NewOIDC accepted incomplete config %d: %+v", i, cfg)
		}
	}
}

func TestPrincipalHelpers(t *testing.T) {
	t.Parallel()
	principal := Principal{Roles: []string{"Admin", "viewer"}, Scopes: []string{"runs:read"}}
	if !principal.HasRole("admin") || !principal.HasRole("ADMIN") {
		t.Fatal("HasRole should be case-insensitive")
	}
	if principal.HasRole("operator") {
		t.Fatal("HasRole matched an absent role")
	}
	if !principal.HasScope("runs:read") {
		t.Fatal("HasScope missed an exact scope")
	}
	if principal.HasScope("RUNS:READ") || principal.HasScope("runs:write") {
		t.Fatal("HasScope should be exact and case-sensitive")
	}
}

func TestOIDCJWKSRemovedKeyRejectedAfterTTL(t *testing.T) {
	fixed := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	oldKey := newRSAKey(t, "removed-1")
	newKey := newRSAKey(t, "replacement-1")
	server := newJWKSServer(t, jwksJSON(t, oldKey))
	current := fixed
	authenticator := newOIDCAuthenticator(t, server.url(), func() time.Time { return current }, time.Minute)

	oldToken := signedJWT(t, oldKey, baseClaims(fixed))
	if _, err := authenticator.Authenticate(t.Context(), bearerRequest(oldToken)); err != nil {
		t.Fatalf("Authenticate old token: %v", err)
	}

	// The trusted key is removed from JWKS while the old token is still fresh
	// from the signer's perspective. Once the cache TTL elapses, the removed
	// key must no longer authenticate.
	server.setBody(jwksJSON(t, newKey))
	current = fixed.Add(2 * time.Minute)
	if _, err := authenticator.Authenticate(t.Context(), bearerRequest(oldToken)); err == nil {
		t.Fatal("token signed by a removed key was accepted after the JWKS cache TTL")
	} else {
		requireTokenError(t, err, "signature verification failed")
	}
}
