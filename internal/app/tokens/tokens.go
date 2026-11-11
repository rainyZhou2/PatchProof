package tokens

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

type AttemptToken struct {
	TenantID   domain.TenantID  `json:"tenant_id"`
	RunID      domain.RunID     `json:"run_id"`
	StepID     domain.StepID    `json:"step_id"`
	AttemptID  domain.AttemptID `json:"attempt_id"`
	Generation int64            `json:"generation"`
	IssuedAt   time.Time        `json:"iat"`
	ExpiresAt  time.Time        `json:"exp"`
}

type Issuer struct {
	key []byte
	now func() time.Time
}

func NewIssuer(key []byte) (*Issuer, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf("attempt token key must be at least 32 bytes")
	}
	return &Issuer{key: append([]byte(nil), key...), now: func() time.Time { return time.Now().UTC() }}, nil
}

func (i *Issuer) Issue(_ context.Context, ref domain.AttemptRef, ttl time.Duration) (string, time.Time, error) {
	if ttl < time.Minute || ttl > 24*time.Hour {
		return "", time.Time{}, fmt.Errorf("attempt token ttl must be between 1m and 24h")
	}
	now := i.now()
	token := AttemptToken{
		TenantID:   ref.TenantID,
		RunID:      ref.RunID,
		StepID:     ref.StepID,
		AttemptID:  ref.AttemptID,
		Generation: ref.Generation,
		IssuedAt:   now,
		ExpiresAt:  now.Add(ttl),
	}
	payload, err := json.Marshal(token)
	if err != nil {
		return "", time.Time{}, err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signature := i.sign([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(signature), token.ExpiresAt, nil
}

func (i *Issuer) Verify(_ context.Context, raw string) (domain.AttemptRef, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return domain.AttemptRef{}, fmt.Errorf("malformed attempt token")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return domain.AttemptRef{}, fmt.Errorf("malformed attempt token")
	}
	if !hmac.Equal(signature, i.sign([]byte(parts[0]))) {
		return domain.AttemptRef{}, fmt.Errorf("attempt token signature is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return domain.AttemptRef{}, fmt.Errorf("malformed attempt token payload")
	}
	var token AttemptToken
	if err := json.Unmarshal(payload, &token); err != nil {
		return domain.AttemptRef{}, fmt.Errorf("malformed attempt token payload")
	}
	if i.now().After(token.ExpiresAt) {
		return domain.AttemptRef{}, fmt.Errorf("attempt token expired")
	}
	if i.now().Before(token.IssuedAt.Add(-time.Minute)) {
		return domain.AttemptRef{}, fmt.Errorf("attempt token issued in the future")
	}
	if token.Generation <= 0 {
		return domain.AttemptRef{}, fmt.Errorf("attempt token has an invalid generation")
	}
	ref := domain.AttemptRef{
		TenantID:   token.TenantID,
		RunID:      token.RunID,
		StepID:     token.StepID,
		AttemptID:  token.AttemptID,
		Generation: token.Generation,
	}
	if err := ref.Validate(); err != nil {
		return domain.AttemptRef{}, err
	}
	return ref, nil
}

func (i *Issuer) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, i.key)
	mac.Write(payload)
	return mac.Sum(nil)
}
