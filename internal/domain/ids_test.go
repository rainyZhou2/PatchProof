package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/patchproof/patchproof/internal/domain"
)

func TestGeneratedIDsAreValid(t *testing.T) {
	tests := []struct {
		name string
		id   interface {
			Validate() error
		}
	}{
		{"tenant", domain.NewTenantID()},
		{"run", domain.NewRunID()},
		{"step", domain.NewStepID()},
		{"attempt", domain.NewAttemptID()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.id.Validate(); err != nil {
				t.Fatalf("generated id invalid: %v", err)
			}
		})
	}
	if err := domain.NewEvidenceID().Validate(); err != nil {
		t.Fatalf("generated evidence id invalid: %v", err)
	}
	if err := domain.NewReportID().Validate(); err != nil {
		t.Fatalf("generated report id invalid: %v", err)
	}
	if _, err := domain.ParseProjectID(domain.NewProjectID().String()); err != nil {
		t.Fatalf("generated project id invalid: %v", err)
	}
	if domain.NewEventID() == "" || domain.NewSignatureID() == "" {
		t.Fatal("generated event/signature ids are empty")
	}
}

func TestParseIDErrors(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"not a uuid", "not-a-uuid"},
		{"uuid v1", "6ba7b810-9dad-11d1-80b4-00c04fd430c8"},
		{"truncated", "11111111-1111-4111-8111"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := domain.ParseTenantID(tc.raw); err == nil {
				t.Fatalf("ParseTenantID(%q) accepted", tc.raw)
			}
			if _, err := domain.ParseRunID(tc.raw); err == nil {
				t.Fatalf("ParseRunID(%q) accepted", tc.raw)
			}
			if _, err := domain.ParseStepID(tc.raw); err == nil {
				t.Fatalf("ParseStepID(%q) accepted", tc.raw)
			}
			if _, err := domain.ParseAttemptID(tc.raw); err == nil {
				t.Fatalf("ParseAttemptID(%q) accepted", tc.raw)
			}
			if _, err := domain.ParseProjectID(tc.raw); err == nil {
				t.Fatalf("ParseProjectID(%q) accepted", tc.raw)
			}
		})
	}
}

func TestParseIDNormalizesToLowercase(t *testing.T) {
	raw := "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"
	id, err := domain.ParseTenantID(raw)
	if err != nil {
		t.Fatalf("parse uppercase uuid: %v", err)
	}
	if got := id.String(); got != strings.ToLower(raw) {
		t.Fatalf("normalized id = %q, want %q", got, strings.ToLower(raw))
	}
	if err := id.Validate(); err != nil {
		t.Fatalf("normalized id invalid: %v", err)
	}
}

func TestZeroIDsInvalid(t *testing.T) {
	var (
		tenant   domain.TenantID
		run      domain.RunID
		step     domain.StepID
		attempt  domain.AttemptID
		evidence domain.EvidenceID
		report   domain.ReportID
	)
	for name, err := range map[string]error{
		"tenant":   tenant.Validate(),
		"run":      run.Validate(),
		"step":     step.Validate(),
		"attempt":  attempt.Validate(),
		"evidence": evidence.Validate(),
		"report":   report.Validate(),
	} {
		if err == nil {
			t.Errorf("zero %s id accepted", name)
		}
	}
}

func TestRequireHelpers(t *testing.T) {
	if err := domain.RequireNonEmpty("field", "  "); err == nil {
		t.Error("whitespace accepted by RequireNonEmpty")
	}
	if err := domain.RequireNonEmpty("field", "value"); err != nil {
		t.Errorf("non-empty value rejected: %v", err)
	}
	if err := domain.RequireMaxLen("field", "abc", 2); err == nil {
		t.Error("over-long value accepted by RequireMaxLen")
	}
	if err := domain.RequireMaxLen("field", "ab", 2); err != nil {
		t.Errorf("max-length value rejected: %v", err)
	}
	var validationErr *domain.ValidationError
	if err := domain.Validationf("field", "bad %d", 1); !errors.As(err, &validationErr) {
		t.Fatalf("Validationf error = %v, want *ValidationError", err)
	}
}

func TestCASRefValidate(t *testing.T) {
	valid := domain.NewCASRef(digestHexA, 10)
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid CAS ref rejected: %v", err)
	}
	if valid.Algorithm != "sha256" {
		t.Errorf("algorithm = %q", valid.Algorithm)
	}
	if valid.Digest != digestHexA {
		t.Errorf("digest = %q, want stripped hex", valid.Digest)
	}
	if valid.Hex() != digestHexA {
		t.Errorf("Hex() = %q", valid.Hex())
	}
	if valid.URI() != "cas://sha256/"+digestHexA {
		t.Errorf("URI() = %q", valid.URI())
	}

	prefixed := domain.CASRef{Algorithm: "sha256", Digest: digestA, SizeBytes: 0}
	if err := prefixed.Validate(); err != nil {
		t.Fatalf("prefixed digest rejected: %v", err)
	}
	if prefixed.Hex() != digestHexA {
		t.Errorf("prefixed Hex() = %q", prefixed.Hex())
	}

	tests := []struct {
		name string
		ref  domain.CASRef
	}{
		{"wrong algorithm", domain.CASRef{Algorithm: "sha1", Digest: digestHexA}},
		{"empty algorithm", domain.CASRef{Digest: digestHexA}},
		{"short digest", domain.CASRef{Algorithm: "sha256", Digest: "abc"}},
		{"long digest", domain.CASRef{Algorithm: "sha256", Digest: digestHexA + "a"}},
		{"uppercase digest", domain.CASRef{Algorithm: "sha256", Digest: strings.ToUpper(digestHexA)}},
		{"non-hex digest", domain.CASRef{Algorithm: "sha256", Digest: strings.Repeat("g", 64)}},
		{"negative size", domain.CASRef{Algorithm: "sha256", Digest: digestHexA, SizeBytes: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.ref.Validate(); err == nil {
				t.Fatalf("invalid CAS ref accepted: %+v", tc.ref)
			}
		})
	}
}

func TestParseCASURI(t *testing.T) {
	uri := "cas://sha256/" + digestHexA
	ref, err := domain.ParseCASURI(uri)
	if err != nil {
		t.Fatalf("parse CAS URI: %v", err)
	}
	if ref.Algorithm != "sha256" || ref.Hex() != digestHexA {
		t.Fatalf("parsed ref = %+v", ref)
	}
	if ref.URI() != uri {
		t.Fatalf("round-tripped URI = %q, want %q", ref.URI(), uri)
	}

	invalid := []string{
		"",
		"sha256/" + digestHexA,
		"http://sha256/" + digestHexA,
		"cas://sha256",
		"cas://sha256/" + digestHexA + "/extra",
		"cas://sha1/" + digestHexA,
		"cas://sha256/not-a-digest",
		"cas://sha256/" + strings.ToUpper(digestHexA),
	}
	for _, raw := range invalid {
		if _, err := domain.ParseCASURI(raw); err == nil {
			t.Errorf("ParseCASURI(%q) accepted", raw)
		}
	}
}
