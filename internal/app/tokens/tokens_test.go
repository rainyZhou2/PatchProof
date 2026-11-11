package tokens

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

func testRef() domain.AttemptRef {
	return domain.AttemptRef{
		TenantID: "11111111-1111-4111-8111-111111111111", RunID: "22222222-2222-4222-8222-222222222222",
		StepID: "33333333-3333-4333-8333-333333333333", AttemptID: "44444444-4444-4444-8444-444444444444", Generation: 3,
	}
}

func TestIssueAndVerify(t *testing.T) {
	issuer, err := NewIssuer([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, expires, err := issuer.Issue(context.Background(), testRef(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !expires.After(time.Now()) {
		t.Fatal("expected a future expiry")
	}
	ref, err := issuer.Verify(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if ref != testRef() {
		t.Fatalf("roundtrip mismatch: %#v", ref)
	}
}

func TestTamperedTokenRejected(t *testing.T) {
	issuer, _ := NewIssuer([]byte(strings.Repeat("k", 32)))
	token, _, _ := issuer.Issue(context.Background(), testRef(), time.Minute)
	parts := strings.Split(token, ".")
	tampered := parts[0] + "x." + parts[1]
	if _, err := issuer.Verify(context.Background(), tampered); err == nil {
		t.Fatal("tampered token accepted")
	}
	if _, err := issuer.Verify(context.Background(), "not-a-token"); err == nil {
		t.Fatal("malformed token accepted")
	}
}

func TestExpiredAndForeignToken(t *testing.T) {
	issuer, _ := NewIssuer([]byte(strings.Repeat("k", 32)))
	token, _, _ := issuer.Issue(context.Background(), testRef(), time.Minute)
	other, _ := NewIssuer([]byte(strings.Repeat("z", 32)))
	if _, err := other.Verify(context.Background(), token); err == nil {
		t.Fatal("token verified with a different key")
	}
	expiring := &Issuer{key: issuer.key, now: func() time.Time { return time.Now().Add(2 * time.Hour) }}
	if _, err := expiring.Verify(context.Background(), token); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestShortKeyRejected(t *testing.T) {
	if _, err := NewIssuer([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}
