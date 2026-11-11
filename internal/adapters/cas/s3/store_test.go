package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/cas"
)

func testScope() ports.SecurityScope {
	return ports.SecurityScope{
		TenantID:    domain.NewTenantID(),
		TrustDomain: "test.patchproof.example",
		Region:      "region-a",
	}
}

func testRef(t *testing.T, content []byte) domain.CASRef {
	t.Helper()
	sum := sha256.Sum256(content)
	return domain.NewCASRef(hex.EncodeToString(sum[:]), int64(len(content)))
}

func TestBuildObjectKey(t *testing.T) {
	scope := testScope()
	ref := testRef(t, []byte("content"))
	base, err := cas.BlobKey(string(scope.TenantID), scope.TrustDomain, ref.Hex())
	if err != nil {
		t.Fatalf("blob key: %v", err)
	}
	cases := []struct {
		prefix string
		want   string
	}{
		{"", base},
		{"evidence", "evidence/" + base},
	}
	for _, tc := range cases {
		got, err := buildObjectKey(tc.prefix, string(scope.TenantID), scope.TrustDomain, ref.Hex())
		if err != nil {
			t.Fatalf("build key with prefix %q: %v", tc.prefix, err)
		}
		if got != tc.want {
			t.Fatalf("build key with prefix %q = %q, want %q", tc.prefix, got, tc.want)
		}
	}
	if _, err := buildObjectKey("", "bad tenant", scope.TrustDomain, ref.Hex()); err == nil {
		t.Fatal("expected invalid tenant to fail")
	}
	if _, err := buildObjectKey("", string(scope.TenantID), "../escape", ref.Hex()); err == nil {
		t.Fatal("expected invalid trust domain to fail")
	}
	if _, err := buildObjectKey("", string(scope.TenantID), scope.TrustDomain, "zz"); err == nil {
		t.Fatal("expected invalid digest to fail")
	}
}

func TestStoreObjectKeyValidatesRef(t *testing.T) {
	store := &Store{prefix: "evidence"}
	scope := testScope()
	ref := testRef(t, []byte("content"))
	key, err := store.objectKey(scope, ref)
	if err != nil {
		t.Fatalf("object key: %v", err)
	}
	if !strings.HasPrefix(key, "evidence/cas/"+string(scope.TenantID)+"/") {
		t.Fatalf("unexpected object key %q", key)
	}
	badScope := scope
	badScope.TrustDomain = ""
	if _, err := store.objectKey(badScope, ref); err == nil {
		t.Fatal("expected empty trust domain to fail")
	}
	badRef := ref
	badRef.Algorithm = "md5"
	if _, err := store.objectKey(scope, badRef); err == nil {
		t.Fatal("expected unsupported algorithm to fail")
	}
}

func TestNormalizeConfig(t *testing.T) {
	valid := Config{
		Endpoint:        "https://minio.example:9000/",
		Region:          "eu-west-1",
		Bucket:          "patchproof",
		AccessKeyID:     "key",
		SecretAccessKey: "secret",
		ForcePathStyle:  true,
		MaxBlobBytes:    1024,
		Prefix:          "/evidence/",
	}
	normalized, err := normalizeConfig(valid)
	if err != nil {
		t.Fatalf("normalize valid config: %v", err)
	}
	if normalized.Endpoint != "https://minio.example:9000" {
		t.Fatalf("endpoint = %q", normalized.Endpoint)
	}
	if normalized.Prefix != "evidence" {
		t.Fatalf("prefix = %q", normalized.Prefix)
	}
	if normalized.PresignTTL != defaultPresignTTL {
		t.Fatalf("presign ttl = %s, want %s", normalized.PresignTTL, defaultPresignTTL)
	}

	bad := []Config{
		{Bucket: ""},
		{Bucket: "with/slash"},
		{Bucket: "bucket", MaxBlobBytes: -1},
		{Bucket: "bucket", AccessKeyID: "key"},
		{Bucket: "bucket", SecretAccessKey: "secret"},
		{Bucket: "bucket", Endpoint: "minio.example:9000"},
		{Bucket: "bucket", Endpoint: "ftp://minio.example"},
		{Bucket: "bucket", Prefix: "../escape"},
		{Bucket: "bucket", Prefix: "a//b"},
		{Bucket: "bucket", PresignTTL: 8 * 24 * time.Hour},
	}
	for _, cfg := range bad {
		if _, err := normalizeConfig(cfg); err == nil {
			t.Fatalf("expected config %+v to fail", cfg)
		}
	}
}

func TestClientOptions(t *testing.T) {
	options := &awss3.Options{}
	for _, apply := range clientOptions(Config{Endpoint: "https://minio.example", ForcePathStyle: true}) {
		apply(options)
	}
	if !options.UsePathStyle {
		t.Fatal("expected path style addressing")
	}
	resolver, ok := options.EndpointResolverV2.(staticEndpointResolver)
	if !ok {
		t.Fatalf("endpoint resolver = %T, want staticEndpointResolver", options.EndpointResolverV2)
	}
	resolved, err := resolver.ResolveEndpoint(context.Background(), awss3.EndpointParameters{
		Bucket:         aws.String("test-bucket"),
		Region:         aws.String("us-east-1"),
		ForcePathStyle: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("resolve endpoint: %v", err)
	}
	if resolved.URI.Host != "minio.example" || !strings.Contains(resolved.URI.Path, "test-bucket") {
		t.Fatalf("resolved endpoint = %q", resolved.URI.String())
	}

	options = &awss3.Options{}
	for _, apply := range clientOptions(Config{UsePathStyle: true}) {
		apply(options)
	}
	if !options.UsePathStyle || options.EndpointResolverV2 != nil {
		t.Fatalf("unexpected options: path=%v resolver=%v", options.UsePathStyle, options.EndpointResolverV2)
	}

	options = &awss3.Options{}
	for _, apply := range clientOptions(Config{}) {
		apply(options)
	}
	if options.UsePathStyle || options.EndpointResolverV2 != nil {
		t.Fatalf("unexpected default options: path=%v resolver=%v", options.UsePathStyle, options.EndpointResolverV2)
	}
}

func TestResolveTTL(t *testing.T) {
	store := &Store{presignTTL: defaultPresignTTL}
	for _, tc := range []struct {
		request time.Duration
		want    time.Duration
	}{
		{0, defaultPresignTTL},
		{time.Minute, time.Minute},
		{maxPresignTTL, maxPresignTTL},
	} {
		got, err := store.resolveTTL(tc.request)
		if err != nil {
			t.Fatalf("resolve ttl %s: %v", tc.request, err)
		}
		if got != tc.want {
			t.Fatalf("resolve ttl %s = %s, want %s", tc.request, got, tc.want)
		}
	}
	if _, err := store.resolveTTL(maxPresignTTL + time.Second); err == nil {
		t.Fatal("expected ttl above the maximum to fail")
	}
	empty := &Store{}
	if got, err := empty.resolveTTL(0); err != nil || got != defaultPresignTTL {
		t.Fatalf("empty store resolve ttl = %s, %v", got, err)
	}
}

func TestVerifyingReader(t *testing.T) {
	content := []byte("verified content")
	ref := testRef(t, content)

	reader := newVerifyingReader(io.NopCloser(bytes.NewReader(content)), ref.Hex(), ref.SizeBytes)
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(data, content) {
		t.Fatal("content mismatch")
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	corrupt := newVerifyingReader(io.NopCloser(bytes.NewReader([]byte("VERIFIED CONTENT"))), ref.Hex(), ref.SizeBytes)
	if _, err := io.ReadAll(corrupt); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt read error = %v, want ErrIntegrity", err)
	}
	if err := corrupt.Close(); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt close error = %v, want ErrIntegrity", err)
	}

	short := newVerifyingReader(io.NopCloser(bytes.NewReader(content)), ref.Hex(), ref.SizeBytes+1)
	if _, err := io.ReadAll(short); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("short read error = %v, want ErrIntegrity", err)
	}

	exact := newVerifyingReader(io.NopCloser(bytes.NewReader(content)), ref.Hex(), ref.SizeBytes)
	buffer := make([]byte, len(content))
	if _, err := io.ReadFull(exact, buffer); err != nil {
		t.Fatalf("read exact size: %v", err)
	}
	if err := exact.Close(); err != nil {
		t.Fatalf("close after exact read: %v", err)
	}

	partial := newVerifyingReader(io.NopCloser(bytes.NewReader(content)), ref.Hex(), ref.SizeBytes)
	buffer = make([]byte, 2)
	if _, err := io.ReadFull(partial, buffer); err != nil {
		t.Fatalf("read prefix: %v", err)
	}
	if err := partial.Close(); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("close before EOF error = %v, want ErrIntegrity", err)
	}
}

func TestPresignUsesEndpointAndExpiry(t *testing.T) {
	awsConfig := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("access", "secret", ""),
	}
	client := awss3.NewFromConfig(awsConfig, clientOptions(Config{
		Endpoint:       "https://minio.example",
		Bucket:         "test-bucket",
		ForcePathStyle: true,
	})...)
	store := &Store{
		client:     client,
		presigner:  awss3.NewPresignClient(client),
		bucket:     "test-bucket",
		prefix:     "evidence",
		presignTTL: time.Minute,
	}
	scope := testScope()
	ref := testRef(t, []byte("content"))

	presigned, _, err := store.PresignGet(context.Background(), scope, ref, 5*time.Minute)
	if err != nil {
		t.Fatalf("presign get: %v", err)
	}
	parsed, err := url.Parse(presigned)
	if err != nil {
		t.Fatalf("parse presigned url: %v", err)
	}
	if parsed.Host != "minio.example" {
		t.Fatalf("presigned host = %q", parsed.Host)
	}
	if !strings.HasPrefix(parsed.Path, "/test-bucket/evidence/cas/") {
		t.Fatalf("presigned path = %q", parsed.Path)
	}
	if got := parsed.Query().Get("X-Amz-Expires"); got != "300" {
		t.Fatalf("presigned expiry = %q, want 300", got)
	}
	if parsed.Query().Get("X-Amz-Signature") == "" {
		t.Fatal("presigned url is not signed")
	}

	ticket, err := store.PresignPut(context.Background(), scope, ref, 2*time.Minute)
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}
	if ticket.Method != http.MethodPut || ticket.Key != ref {
		t.Fatalf("unexpected ticket: %+v", ticket)
	}
	parsed, err = url.Parse(ticket.URL)
	if err != nil {
		t.Fatalf("parse ticket url: %v", err)
	}
	if parsed.Host != "minio.example" || !strings.HasPrefix(parsed.Path, "/test-bucket/evidence/cas/") {
		t.Fatalf("ticket url = %q", ticket.URL)
	}
	if got := parsed.Query().Get("X-Amz-Expires"); got != "120" {
		t.Fatalf("ticket expiry = %q, want 120", got)
	}
	if untilExpiry := time.Until(ticket.ExpiresAt); untilExpiry < time.Minute || untilExpiry > 3*time.Minute {
		t.Fatalf("ticket expires in %s, want about 2m", untilExpiry)
	}
}

func TestIsNotFound(t *testing.T) {
	if isNotFound(nil) {
		t.Fatal("nil must not be not-found")
	}
	if isNotFound(errors.New("boom")) {
		t.Fatal("generic error must not be not-found")
	}
	if !isNotFound(&s3types.NotFound{}) {
		t.Fatal("typed NotFound was not detected")
	}
	if !isNotFound(&s3types.NoSuchKey{}) {
		t.Fatal("typed NoSuchKey was not detected")
	}
}

func TestLiveS3(t *testing.T) {
	endpoint := os.Getenv("PATCHPROOF_TEST_S3_ENDPOINT")
	bucket := os.Getenv("PATCHPROOF_TEST_S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("set PATCHPROOF_TEST_S3_ENDPOINT and PATCHPROOF_TEST_S3_BUCKET to run live S3 tests")
	}
	region := os.Getenv("PATCHPROOF_TEST_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, err := New(ctx, Config{
		Endpoint:        endpoint,
		Region:          region,
		Bucket:          bucket,
		AccessKeyID:     os.Getenv("PATCHPROOF_TEST_S3_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("PATCHPROOF_TEST_S3_SECRET_KEY"),
		ForcePathStyle:  true,
		MaxBlobBytes:    1 << 20,
		PresignTTL:      5 * time.Minute,
		Prefix:          "patchproof-tests",
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	scope := testScope()
	content := []byte("live s3 artifact")
	ref, err := store.Put(ctx, scope, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	reader, err := store.Open(ctx, scope, ref)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	read, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !bytes.Equal(read, content) {
		t.Fatal("content mismatch")
	}
	info, err := store.Stat(ctx, scope, ref)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Ref.SizeBytes != int64(len(content)) {
		t.Fatalf("stat size = %d", info.Ref.SizeBytes)
	}
	url, _, err := store.PresignGet(ctx, scope, ref, time.Minute)
	if err != nil || !strings.Contains(url, "X-Amz-Signature") {
		t.Fatalf("presign get url = %q, err %v", url, err)
	}
	ticket, err := store.PresignPut(ctx, scope, ref, time.Minute)
	if err != nil {
		t.Fatalf("presign put: %v", err)
	}
	if ticket.Method != http.MethodPut || ticket.URL == "" || ticket.Key != ref {
		t.Fatalf("unexpected ticket: %+v", ticket)
	}
	if err := store.Delete(ctx, scope, ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
