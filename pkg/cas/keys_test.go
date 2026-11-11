package cas_test

import (
	"strings"
	"testing"

	"github.com/patchproof/patchproof/pkg/cas"
)

const (
	validDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// validDigestPrefix is the expected fan-out prefix derived from validDigest.
	validDigestPrefix = "01/23"
)

func TestNormalizeDigest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "plain lowercase hex", input: validDigest, want: validDigest},
		{name: "sha256 prefix", input: "sha256:" + validDigest, want: validDigest},
		{name: "surrounding whitespace", input: "  sha256:" + validDigest + "\n", want: validDigest},
		{name: "empty", input: "", wantErr: true},
		{name: "prefix only", input: "sha256:", wantErr: true},
		{name: "too short", input: validDigest[:63], wantErr: true},
		{name: "too long", input: validDigest + "0", wantErr: true},
		{name: "uppercase", input: strings.ToUpper(validDigest), wantErr: true},
		{name: "non hex", input: strings.Repeat("g", 64), wantErr: true},
		{name: "double prefix", input: "sha256:sha256:" + validDigest, wantErr: true},
		{name: "algorithm prefix", input: "sha512:" + validDigest, wantErr: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := cas.NormalizeDigest(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("NormalizeDigest(%q) = %q, want error", test.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeDigest(%q): %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("NormalizeDigest(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestBlobKey(t *testing.T) {
	t.Parallel()
	key, err := cas.BlobKey("tenant-1", "run", "sha256:"+validDigest)
	if err != nil {
		t.Fatalf("BlobKey: %v", err)
	}
	want := "cas/tenant-1/run/sha256/" + validDigestPrefix + "/" + validDigest
	if key != want {
		t.Fatalf("BlobKey = %q, want %q", key, want)
	}

	validNamespace := "Tenant_1.2-3"
	key, err = cas.BlobKey(validNamespace, validNamespace, validDigest)
	if err != nil {
		t.Fatalf("BlobKey with mixed-case namespace: %v", err)
	}
	if !strings.HasPrefix(key, "cas/"+validNamespace+"/"+validNamespace+"/sha256/") {
		t.Fatalf("unexpected blob key %q", key)
	}

	invalid := []struct {
		name        string
		tenant      string
		trustDomain string
		digest      string
		wantInError string
	}{
		{name: "empty tenant", tenant: "", trustDomain: "run", digest: validDigest, wantInError: "tenant"},
		{name: "tenant leading dash", tenant: "-tenant", trustDomain: "run", digest: validDigest, wantInError: "tenant"},
		{name: "tenant leading dot", tenant: ".tenant", trustDomain: "run", digest: validDigest, wantInError: "tenant"},
		{name: "tenant path separator", tenant: "tenant/one", trustDomain: "run", digest: validDigest, wantInError: "tenant"},
		{name: "tenant space", tenant: "tenant one", trustDomain: "run", digest: validDigest, wantInError: "tenant"},
		{name: "tenant too long", tenant: strings.Repeat("a", 65), trustDomain: "run", digest: validDigest, wantInError: "tenant"},
		{name: "empty trust domain", tenant: "tenant-1", trustDomain: "", digest: validDigest, wantInError: "trust domain"},
		{name: "trust domain traversal", tenant: "tenant-1", trustDomain: "..", digest: validDigest, wantInError: "trust domain"},
		{name: "invalid digest", tenant: "tenant-1", trustDomain: "run", digest: "not-a-digest", wantInError: "digest"},
	}
	for _, test := range invalid {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			key, err := cas.BlobKey(test.tenant, test.trustDomain, test.digest)
			if err == nil {
				t.Fatalf("BlobKey(%q, %q, %q) = %q, want error", test.tenant, test.trustDomain, test.digest, key)
			}
			if !strings.Contains(err.Error(), test.wantInError) {
				t.Fatalf("BlobKey error = %q, want it to mention %q", err, test.wantInError)
			}
		})
	}
}

func TestBlobKeyIsStable(t *testing.T) {
	t.Parallel()
	first, err := cas.BlobKey("tenant-1", "run", validDigest)
	if err != nil {
		t.Fatalf("BlobKey: %v", err)
	}
	for i := 0; i < 5; i++ {
		next, err := cas.BlobKey("tenant-1", "run", validDigest)
		if err != nil {
			t.Fatalf("BlobKey %d: %v", i, err)
		}
		if next != first {
			t.Fatalf("BlobKey %d = %q, want %q", i, next, first)
		}
	}
	if got, err := cas.DigestFromBlobKey(first); err != nil || got != validDigest {
		t.Fatalf("DigestFromBlobKey(%q) = %q, %v; want %q, nil", first, got, err, validDigest)
	}
}

func TestReportKey(t *testing.T) {
	t.Parallel()
	key, err := cas.ReportKey("tenant-1", "run-1", "report.json")
	if err != nil {
		t.Fatalf("ReportKey: %v", err)
	}
	if want := "reports/tenant-1/run-1/report.json"; key != want {
		t.Fatalf("ReportKey = %q, want %q", key, want)
	}
	tests := []struct {
		name            string
		tenant, run, id string
	}{
		{name: "empty tenant", tenant: "", run: "run-1", id: "report.json"},
		{name: "empty run", tenant: "tenant-1", run: "", id: "report.json"},
		{name: "empty name", tenant: "tenant-1", run: "run-1", id: ""},
		{name: "name with slash", tenant: "tenant-1", run: "run-1", id: "nested/report.json"},
		{name: "name traversal", tenant: "tenant-1", run: "run-1", id: ".."},
		{name: "run with space", tenant: "tenant-1", run: "run 1", id: "report.json"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if key, err := cas.ReportKey(test.tenant, test.run, test.id); err == nil {
				t.Fatalf("ReportKey(%q, %q, %q) = %q, want error", test.tenant, test.run, test.id, key)
			}
		})
	}
}

func TestReproducerKey(t *testing.T) {
	t.Parallel()
	key, err := cas.ReproducerKey("tenant-1", "run-1", "case-1")
	if err != nil {
		t.Fatalf("ReproducerKey: %v", err)
	}
	if want := "reproducers/tenant-1/run-1/case-1/bundle.tar.zst"; key != want {
		t.Fatalf("ReproducerKey = %q, want %q", key, want)
	}
	for _, test := range []struct {
		name            string
		tenant, run, id string
	}{
		{name: "empty tenant", tenant: "", run: "run-1", id: "case-1"},
		{name: "empty run", tenant: "tenant-1", run: "", id: "case-1"},
		{name: "empty case", tenant: "tenant-1", run: "run-1", id: ""},
		{name: "case with slash", tenant: "tenant-1", run: "run-1", id: "a/b"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if key, err := cas.ReproducerKey(test.tenant, test.run, test.id); err == nil {
				t.Fatalf("ReproducerKey(%q, %q, %q) = %q, want error", test.tenant, test.run, test.id, key)
			}
		})
	}
}

func TestDigestFromBlobKey(t *testing.T) {
	t.Parallel()
	key, err := cas.BlobKey("tenant-1", "run", validDigest)
	if err != nil {
		t.Fatalf("BlobKey: %v", err)
	}
	got, err := cas.DigestFromBlobKey(key)
	if err != nil {
		t.Fatalf("DigestFromBlobKey: %v", err)
	}
	if got != validDigest {
		t.Fatalf("DigestFromBlobKey = %q, want %q", got, validDigest)
	}

	// DigestFromBlobKey only inspects the final path segment; any key whose last
	// segment is a valid digest yields that digest.
	got, err = cas.DigestFromBlobKey("some/other/prefix/" + validDigest)
	if err != nil || got != validDigest {
		t.Fatalf("DigestFromBlobKey = %q, %v; want %q, nil", got, err, validDigest)
	}

	for _, bad := range []string{"", "single", "prefix/" + validDigest[:63], "prefix/" + strings.ToUpper(validDigest), "prefix/"} {
		if got, err := cas.DigestFromBlobKey(bad); err == nil {
			t.Fatalf("DigestFromBlobKey(%q) = %q, want error", bad, got)
		}
	}
}

func TestBlobKeyUsesNormalizedDigest(t *testing.T) {
	t.Parallel()
	fromPlain, err := cas.BlobKey("tenant-1", "run", validDigest)
	if err != nil {
		t.Fatalf("BlobKey plain: %v", err)
	}
	fromPrefixed, err := cas.BlobKey("tenant-1", "run", "sha256:"+validDigest)
	if err != nil {
		t.Fatalf("BlobKey prefixed: %v", err)
	}
	if fromPlain != fromPrefixed {
		t.Fatalf("keys differ for plain and prefixed digests: %q vs %q", fromPlain, fromPrefixed)
	}
}
