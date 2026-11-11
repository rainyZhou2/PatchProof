package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/security/auth"
)

type contextKey string

const (
	principalKey contextKey = "principal"
	requestIDKey contextKey = "request-id"
)

func withPrincipal(ctx context.Context, principal auth.Principal) context.Context {
	return context.WithValue(ctx, principalKey, principal)
}

func principalFrom(ctx context.Context) (auth.Principal, bool) {
	principal, ok := ctx.Value(principalKey).(auth.Principal)
	return principal, ok
}

func withRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey, requestID)
}

func requestIDFrom(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey).(string)
	return value
}

func tenantHash(tenantID domain.TenantID) string {
	sum := sha256.Sum256([]byte(tenantID))
	return hex.EncodeToString(sum[:6])
}
