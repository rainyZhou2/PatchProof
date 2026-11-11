package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

type Principal struct {
	Subject   string
	TenantID  domain.TenantID
	Roles     []string
	Scopes    []string
	Issuer    string
	ExpiresAt time.Time
}

func (p Principal) HasRole(role string) bool {
	for _, candidate := range p.Roles {
		if strings.EqualFold(candidate, role) {
			return true
		}
	}
	return false
}

func (p Principal) HasScope(scope string) bool {
	for _, candidate := range p.Scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}

type Authenticator interface {
	Authenticate(ctx context.Context, request *http.Request) (Principal, error)
}

type TokenError struct {
	Reason string
}

func (e *TokenError) Error() string { return "authentication failed: " + e.Reason }

type DevAuthenticator struct {
	Token    string
	TenantID domain.TenantID
	Subject  string
	Roles    []string
}

func (a *DevAuthenticator) Authenticate(_ context.Context, request *http.Request) (Principal, error) {
	if a == nil || a.Token == "" {
		return Principal{}, &TokenError{Reason: "developer authentication is not configured"}
	}
	token, err := bearerToken(request)
	if err != nil {
		return Principal{}, err
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(a.Token)) != 1 {
		return Principal{}, &TokenError{Reason: "invalid token"}
	}
	if err := a.TenantID.Validate(); err != nil {
		return Principal{}, &TokenError{Reason: "developer tenant is not configured"}
	}
	return Principal{
		Subject: a.Subject, TenantID: a.TenantID, Roles: append([]string{"admin", "operator", "viewer"}, a.Roles...),
		Scopes: []string{"runs:write", "runs:read"}, ExpiresAt: time.Now().Add(24 * time.Hour),
	}, nil
}

type OIDCConfig struct {
	Issuer      string
	Audience    string
	JWKSURL     string
	TenantClaim string
	RolesClaim  string
	GroupsClaim string
	ClockSkew   time.Duration
	CacheTTL    time.Duration
	HTTPClient  *http.Client
	Now         func() time.Time
}

type OIDCAuthenticator struct {
	config OIDCConfig
	keys   *keyCache
	now    func() time.Time
}

func NewOIDC(config OIDCConfig) (*OIDCAuthenticator, error) {
	if config.Issuer == "" || config.Audience == "" || config.JWKSURL == "" {
		return nil, fmt.Errorf("oidc: issuer, audience and jwks url are required")
	}
	if config.TenantClaim == "" {
		config.TenantClaim = "tenant_id"
	}
	if config.RolesClaim == "" {
		config.RolesClaim = "roles"
	}
	if config.GroupsClaim == "" {
		config.GroupsClaim = "groups"
	}
	if config.ClockSkew <= 0 {
		config.ClockSkew = time.Minute
	}
	if config.CacheTTL <= 0 {
		config.CacheTTL = 10 * time.Minute
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &OIDCAuthenticator{
		config: config,
		now:    config.Now,
		keys:   newKeyCache(config.JWKSURL, config.HTTPClient, config.CacheTTL, config.Now),
	}, nil
}

func (a *OIDCAuthenticator) Authenticate(ctx context.Context, request *http.Request) (Principal, error) {
	token, err := bearerToken(request)
	if err != nil {
		return Principal{}, err
	}
	if len(token) > 16384 {
		return Principal{}, &TokenError{Reason: "token is too large"}
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, &TokenError{Reason: "malformed token"}
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Principal{}, &TokenError{Reason: "malformed token header"}
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Principal{}, &TokenError{Reason: "malformed token payload"}
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Principal{}, &TokenError{Reason: "malformed token signature"}
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
		Type      string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Principal{}, &TokenError{Reason: "malformed token header"}
	}
	if header.Algorithm != "RS256" && header.Algorithm != "ES256" {
		return Principal{}, &TokenError{Reason: "unsupported signing algorithm"}
	}
	keys, err := a.keys.Get(ctx, header.KeyID)
	if err != nil {
		return Principal{}, &TokenError{Reason: "signing keys are unavailable"}
	}
	signed := []byte(parts[0] + "." + parts[1])
	if !verifySignature(header.Algorithm, keys, signed, signature) {
		keys, err = a.keys.Refresh(ctx, header.KeyID)
		if err != nil || !verifySignature(header.Algorithm, keys, signed, signature) {
			return Principal{}, &TokenError{Reason: "signature verification failed"}
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return Principal{}, &TokenError{Reason: "malformed token claims"}
	}
	now := a.now()
	if err := a.validateClaims(claims, now); err != nil {
		return Principal{}, err
	}
	tenant, _ := claims[a.config.TenantClaim].(string)
	tenantID, err := domain.ParseTenantID(tenant)
	if err != nil {
		return Principal{}, &TokenError{Reason: "tenant claim is missing or invalid"}
	}
	principal := Principal{
		Subject:   stringClaim(claims, "sub"),
		TenantID:  tenantID,
		Roles:     listClaim(claims, a.config.RolesClaim),
		Scopes:    scopeClaim(claims),
		Issuer:    stringClaim(claims, "iss"),
		ExpiresAt: time.Unix(int64Claim(claims, "exp"), 0),
	}
	principal.Roles = append(principal.Roles, listClaim(claims, a.config.GroupsClaim)...)
	if principal.Subject == "" {
		return Principal{}, &TokenError{Reason: "subject claim is missing"}
	}
	return principal, nil
}

func (a *OIDCAuthenticator) validateClaims(claims map[string]any, now time.Time) error {
	if issuer := stringClaim(claims, "iss"); issuer != a.config.Issuer {
		return &TokenError{Reason: "issuer mismatch"}
	}
	if !audienceMatches(claims["aud"], a.config.Audience) {
		return &TokenError{Reason: "audience mismatch"}
	}
	exp := int64Claim(claims, "exp")
	if exp == 0 || now.After(time.Unix(exp, 0).Add(a.config.ClockSkew)) {
		return &TokenError{Reason: "token is expired"}
	}
	if nbf := int64Claim(claims, "nbf"); nbf != 0 && now.Add(a.config.ClockSkew).Before(time.Unix(nbf, 0)) {
		return &TokenError{Reason: "token is not yet valid"}
	}
	if iat := int64Claim(claims, "iat"); iat != 0 && now.Add(a.config.ClockSkew).Before(time.Unix(iat, 0)) {
		return &TokenError{Reason: "token issued in the future"}
	}
	return nil
}

func bearerToken(request *http.Request) (string, error) {
	header := request.Header.Get("Authorization")
	if header == "" {
		return "", &TokenError{Reason: "missing authorization header"}
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", &TokenError{Reason: "authorization header must use the Bearer scheme"}
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", &TokenError{Reason: "empty bearer token"}
	}
	return token, nil
}

func audienceMatches(value any, expected string) bool {
	switch typed := value.(type) {
	case string:
		return typed == expected
	case []any:
		for _, item := range typed {
			if candidate, ok := item.(string); ok && candidate == expected {
				return true
			}
		}
	}
	return false
}

func stringClaim(claims map[string]any, key string) string {
	value, _ := claims[key].(string)
	return value
}

func int64Claim(claims map[string]any, key string) int64 {
	value, ok := claims[key].(float64)
	if !ok {
		return 0
	}
	return int64(value)
}

func listClaim(claims map[string]any, key string) []string {
	value, ok := claims[key]
	if !ok {
		return nil
	}
	switch typed := value.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if candidate, ok := item.(string); ok && candidate != "" {
				out = append(out, candidate)
			}
		}
		return out
	case string:
		return strings.Fields(typed)
	default:
		return nil
	}
}

func scopeClaim(claims map[string]any) []string {
	raw, _ := claims["scope"].(string)
	if raw == "" {
		return nil
	}
	return strings.Fields(raw)
}

func verifySignature(algorithm string, keys []crypto.PublicKey, signed, signature []byte) bool {
	digest := sha256.Sum256(signed)
	for _, key := range keys {
		switch algorithm {
		case "RS256":
			rsaKey, ok := key.(*rsa.PublicKey)
			if !ok {
				continue
			}
			if rsa.VerifyPKCS1v15(rsaKey, crypto.SHA256, digest[:], signature) == nil {
				return true
			}
		case "ES256":
			ecKey, ok := key.(*ecdsa.PublicKey)
			if !ok || ecKey.Curve != elliptic.P256() {
				continue
			}
			if len(signature) != 64 {
				continue
			}
			r := new(big.Int).SetBytes(signature[:32])
			s := new(big.Int).SetBytes(signature[32:])
			if ecdsa.Verify(ecKey, digest[:], r, s) {
				return true
			}
		}
	}
	return false
}

type keyCache struct {
	url       string
	client    *http.Client
	ttl       time.Duration
	now       func() time.Time
	mu        sync.RWMutex
	keys      []crypto.PublicKey
	byID      map[string]crypto.PublicKey
	expiresAt time.Time
	lastFetch time.Time
}

func newKeyCache(url string, client *http.Client, ttl time.Duration, now func() time.Time) *keyCache {
	return &keyCache{url: url, client: client, ttl: ttl, now: now, byID: map[string]crypto.PublicKey{}}
}

func (c *keyCache) Get(ctx context.Context, keyID string) ([]crypto.PublicKey, error) {
	c.mu.RLock()
	key, ok := c.byID[keyID]
	fresh := c.now().Before(c.expiresAt)
	all := c.keys
	c.mu.RUnlock()
	if ok {
		return []crypto.PublicKey{key}, nil
	}
	if fresh && keyID == "" && len(all) > 0 {
		return all, nil
	}
	return c.Refresh(ctx, keyID)
}

func (c *keyCache) Refresh(ctx context.Context, keyID string) ([]crypto.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.now().Before(c.expiresAt) && keyID != "" {
		if key, ok := c.byID[keyID]; ok {
			return []crypto.PublicKey{key}, nil
		}
	}
	if c.now().Sub(c.lastFetch) < time.Second {
		return c.keysFor(keyID), nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("jwks endpoint returned %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	keys, byID, err := parseJWKS(body)
	if err != nil {
		return nil, err
	}
	c.keys = keys
	c.byID = byID
	c.expiresAt = c.now().Add(c.ttl)
	c.lastFetch = c.now()
	if len(keys) == 0 {
		return nil, errors.New("jwks contains no supported keys")
	}
	return c.keysFor(keyID), nil
}

func (c *keyCache) keysFor(keyID string) []crypto.PublicKey {
	if keyID == "" {
		return c.keys
	}
	if key, ok := c.byID[keyID]; ok {
		return []crypto.PublicKey{key}
	}
	return nil
}

func parseJWKS(body []byte) ([]crypto.PublicKey, map[string]crypto.PublicKey, error) {
	var document struct {
		Keys []struct {
			KeyType   string `json:"kty"`
			KeyID     string `json:"kid"`
			Use       string `json:"use"`
			Algorithm string `json:"alg"`
			N         string `json:"n"`
			E         string `json:"e"`
			Curve     string `json:"crv"`
			X         string `json:"x"`
			Y         string `json:"y"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, nil, fmt.Errorf("parse jwks: %w", err)
	}
	keys := make([]crypto.PublicKey, 0, len(document.Keys))
	byID := map[string]crypto.PublicKey{}
	for _, entry := range document.Keys {
		if entry.Use != "" && entry.Use != "sig" {
			continue
		}
		switch entry.KeyType {
		case "RSA":
			modulus, err := base64.RawURLEncoding.DecodeString(entry.N)
			if err != nil || entry.E == "" {
				continue
			}
			exponentBytes, err := base64.RawURLEncoding.DecodeString(entry.E)
			if err != nil {
				continue
			}
			exponent := 0
			for _, b := range exponentBytes {
				exponent = exponent<<8 | int(b)
			}
			if exponent == 0 || exponent > 1<<31 {
				continue
			}
			key := &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: exponent}
			keys = append(keys, key)
			if entry.KeyID != "" {
				byID[entry.KeyID] = key
			}
		case "EC":
			if entry.Curve != "P-256" {
				continue
			}
			x, err := base64.RawURLEncoding.DecodeString(entry.X)
			if err != nil {
				continue
			}
			y, err := base64.RawURLEncoding.DecodeString(entry.Y)
			if err != nil {
				continue
			}
			key := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
			keys = append(keys, key)
			if entry.KeyID != "" {
				byID[entry.KeyID] = key
			}
		}
	}
	return keys, byID, nil
}
