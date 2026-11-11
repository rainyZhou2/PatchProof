package postgres

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/patchproof/patchproof/internal/domain"
)

func TestRunCursorRoundTrip(t *testing.T) {
	createdAt := time.Date(2026, 5, 12, 3, 4, 5, 987654321, time.FixedZone("offset", 8*3600))
	runID := domain.RunID(uuid.NewString())
	encoded := encodeRunCursor(createdAt, runID)
	if encoded == "" {
		t.Fatal("encoded cursor is empty")
	}
	decoded, err := decodeRunCursor(encoded)
	if err != nil {
		t.Fatalf("decodeRunCursor: %v", err)
	}
	if !decoded.CreatedAt.Equal(createdAt) {
		t.Fatalf("created_at round trip mismatch: %v != %v", decoded.CreatedAt, createdAt)
	}
	if decoded.CreatedAt.Location() != time.UTC {
		t.Fatalf("decoded cursor must be UTC, got %v", decoded.CreatedAt.Location())
	}
	if decoded.RunID != runID {
		t.Fatalf("run id round trip mismatch: %s != %s", decoded.RunID, runID)
	}
}

func TestDecodeRunCursorRejectsInvalidInput(t *testing.T) {
	runID := uuid.NewString()
	validTimestamp := time.Now().UTC().Format(time.RFC3339Nano)
	cases := map[string]string{
		"empty":          "",
		"not base64url":  "!!!!",
		"missing pipe":   base64.RawURLEncoding.EncodeToString([]byte(validTimestamp + runID)),
		"bad timestamp":  base64.RawURLEncoding.EncodeToString([]byte("yesterday|" + runID)),
		"bad run id":     base64.RawURLEncoding.EncodeToString([]byte(validTimestamp + "|not-a-uuid")),
		"extra segments": base64.RawURLEncoding.EncodeToString([]byte(validTimestamp + "|" + runID + "|extra")),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeRunCursor(raw); err == nil {
				t.Fatalf("expected error for %q", name)
			}
		})
	}
}

func TestClampLimit(t *testing.T) {
	cases := []struct {
		limit    int
		fallback int
		maximum  int
		want     int
	}{
		{0, 50, 500, 50},
		{-3, 50, 500, 50},
		{10, 50, 500, 10},
		{501, 50, 500, 500},
		{500, 50, 500, 500},
	}
	for _, tc := range cases {
		if got := clampLimit(tc.limit, tc.fallback, tc.maximum); got != tc.want {
			t.Fatalf("clampLimit(%d, %d, %d) = %d, want %d", tc.limit, tc.fallback, tc.maximum, got, tc.want)
		}
	}
}
