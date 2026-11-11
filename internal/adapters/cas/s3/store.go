// Package s3 provides a content-addressed artifact store backed by an
// S3-compatible object store. Objects are laid out using pkg/cas.BlobKey and
// optionally a configured key prefix.
package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyendpoints "github.com/aws/smithy-go/endpoints"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/cas"
)

const (
	defaultRegion     = "us-east-1"
	defaultPresignTTL = 15 * time.Minute
	maxPresignTTL     = 7 * 24 * time.Hour
	copyBufferSize    = 128 << 10
)

var (
	// ErrIntegrity reports an object whose size or digest does not match its
	// content address.
	ErrIntegrity = errors.New("artifact integrity check failed")
	// ErrBlobTooLarge reports a blob that exceeds Config.MaxBlobBytes.
	ErrBlobTooLarge = errors.New("artifact exceeds maximum blob size")
)

// Config configures an S3-compatible artifact store.
type Config struct {
	// Endpoint is an optional custom service endpoint URL.
	Endpoint string
	// Region is the signing region; us-east-1 when empty.
	Region string
	// Bucket is the target bucket.
	Bucket string
	// AccessKeyID and SecretAccessKey enable static credentials when both set.
	AccessKeyID     string
	SecretAccessKey string
	// ForcePathStyle is kept for compatibility; it is equivalent to UsePathStyle.
	ForcePathStyle bool
	// MaxBlobBytes limits a single blob when greater than zero.
	MaxBlobBytes int64
	// PresignTTL is the default presigned URL lifetime.
	PresignTTL time.Duration
	// Prefix is an optional key prefix below the bucket root.
	Prefix string
	// UsePathStyle addresses buckets as path segments instead of host names.
	UsePathStyle bool
}

// Store is an S3-backed content-addressed artifact store.
type Store struct {
	client       *awss3.Client
	presigner    *awss3.PresignClient
	bucket       string
	prefix       string
	maxBlobBytes int64
	presignTTL   time.Duration
}

var (
	_ ports.ArtifactStore = (*Store)(nil)
	_ ports.Presigner     = (*Store)(nil)
)

// New creates an S3 artifact store. Network access is deferred until the first
// store operation.
func New(ctx context.Context, cfg Config) (*Store, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	region := cfg.Region
	if region == "" {
		region = defaultRegion
	}
	loadOptions := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if cfg.AccessKeyID != "" {
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("s3 cas: load aws config: %w", err)
	}
	client := awss3.NewFromConfig(awsConfig, clientOptions(cfg)...)
	return &Store{
		client:       client,
		presigner:    awss3.NewPresignClient(client),
		bucket:       cfg.Bucket,
		prefix:       cfg.Prefix,
		maxBlobBytes: cfg.MaxBlobBytes,
		presignTTL:   cfg.PresignTTL,
	}, nil
}

// Put streams r into a temporary file while hashing it, then uploads it under
// its SHA-256 content address. An existing object with the expected size is
// treated as success.
func (s *Store) Put(ctx context.Context, scope ports.SecurityScope, r io.Reader) (domain.CASRef, error) {
	if err := scope.Validate(); err != nil {
		return domain.CASRef{}, fmt.Errorf("s3 cas put: %w", err)
	}
	if r == nil {
		return domain.CASRef{}, errors.New("s3 cas put: reader must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return domain.CASRef{}, err
	}
	temp, err := os.CreateTemp("", "patchproof-cas-put-*")
	if err != nil {
		return domain.CASRef{}, fmt.Errorf("s3 cas put: create temp file: %w", err)
	}
	tempName := temp.Name()
	defer func() {
		temp.Close()
		os.Remove(tempName)
	}()

	digest := sha256.New()
	written, err := copyBlob(ctx, io.MultiWriter(digest, temp), r, s.maxBlobBytes)
	if err != nil {
		return domain.CASRef{}, err
	}
	if err := temp.Sync(); err != nil {
		return domain.CASRef{}, fmt.Errorf("s3 cas put: sync temp file: %w", err)
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		return domain.CASRef{}, fmt.Errorf("s3 cas put: seek temp file: %w", err)
	}

	ref := domain.NewCASRef(hex.EncodeToString(digest.Sum(nil)), written)
	key, err := s.objectKey(scope, ref)
	if err != nil {
		return domain.CASRef{}, fmt.Errorf("s3 cas put: %w", err)
	}

	head, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	switch {
	case err == nil:
		if aws.ToInt64(head.ContentLength) != written {
			return domain.CASRef{}, fmt.Errorf("%w: object %s has size %d, expected %d", ErrIntegrity, ref.URI(), aws.ToInt64(head.ContentLength), written)
		}
		return ref, nil
	case isNotFound(err):
	default:
		return domain.CASRef{}, fmt.Errorf("s3 cas put: head object %s: %w", ref.URI(), err)
	}

	if _, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          temp,
		ContentLength: aws.Int64(written),
	}); err != nil {
		return domain.CASRef{}, fmt.Errorf("s3 cas put: upload object %s: %w", ref.URI(), err)
	}
	return ref, nil
}

// Open returns a reader that verifies the object digest and size at EOF.
func (s *Store) Open(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) (io.ReadCloser, error) {
	key, err := s.objectKey(scope, ref)
	if err != nil {
		return nil, fmt.Errorf("s3 cas open: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("s3 cas open %s: %w", ref.URI(), os.ErrNotExist)
		}
		return nil, fmt.Errorf("s3 cas open %s: %w", ref.URI(), err)
	}
	if out.Body == nil {
		return nil, fmt.Errorf("s3 cas open %s: empty response body", ref.URI())
	}
	if out.ContentLength != nil && *out.ContentLength != ref.SizeBytes {
		out.Body.Close()
		return nil, fmt.Errorf("%w: object %s has size %d, expected %d", ErrIntegrity, ref.URI(), *out.ContentLength, ref.SizeBytes)
	}
	return newVerifyingReader(out.Body, ref.Hex(), ref.SizeBytes), nil
}

// Stat returns object metadata for a stored blob.
func (s *Store) Stat(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) (ports.BlobInfo, error) {
	key, err := s.objectKey(scope, ref)
	if err != nil {
		return ports.BlobInfo{}, fmt.Errorf("s3 cas stat: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ports.BlobInfo{}, err
	}
	out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return ports.BlobInfo{}, fmt.Errorf("s3 cas stat %s: %w", ref.URI(), os.ErrNotExist)
		}
		return ports.BlobInfo{}, fmt.Errorf("s3 cas stat %s: %w", ref.URI(), err)
	}
	return ports.BlobInfo{
		Ref:         domain.NewCASRef(ref.Hex(), aws.ToInt64(out.ContentLength)),
		Namespace:   scope.Namespace(),
		CreatedAt:   aws.ToTime(out.LastModified),
		ContentType: aws.ToString(out.ContentType),
		Metadata:    out.Metadata,
	}, nil
}

// Delete removes an object. Deleting a missing object succeeds.
func (s *Store) Delete(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef) error {
	key, err := s.objectKey(scope, ref)
	if err != nil {
		return fmt.Errorf("s3 cas delete: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("s3 cas delete %s: %w", ref.URI(), err)
	}
	return nil
}

// Health verifies access to the configured bucket.
func (s *Store) Health(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		return fmt.Errorf("s3 cas health: bucket %q: %w", s.bucket, err)
	}
	return nil
}

// PresignGet returns a presigned download URL for a stored blob.
func (s *Store) PresignGet(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef, ttl time.Duration) (string, http.Header, error) {
	key, err := s.objectKey(scope, ref)
	if err != nil {
		return "", nil, fmt.Errorf("s3 cas presign get: %w", err)
	}
	expiry, err := s.resolveTTL(ttl)
	if err != nil {
		return "", nil, fmt.Errorf("s3 cas presign get: %w", err)
	}
	request, err := s.presigner.PresignGetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, func(options *awss3.PresignOptions) {
		options.Expires = expiry
	})
	if err != nil {
		return "", nil, fmt.Errorf("s3 cas presign get %s: %w", ref.URI(), err)
	}
	return request.URL, request.SignedHeader, nil
}

// PresignPut returns a presigned upload ticket. It does not verify the uploaded
// bytes; callers must confirm the digest with Stat after the upload.
func (s *Store) PresignPut(ctx context.Context, scope ports.SecurityScope, ref domain.CASRef, ttl time.Duration) (ports.UploadTicket, error) {
	key, err := s.objectKey(scope, ref)
	if err != nil {
		return ports.UploadTicket{}, fmt.Errorf("s3 cas presign put: %w", err)
	}
	expiry, err := s.resolveTTL(ttl)
	if err != nil {
		return ports.UploadTicket{}, fmt.Errorf("s3 cas presign put: %w", err)
	}
	request, err := s.presigner.PresignPutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, func(options *awss3.PresignOptions) {
		options.Expires = expiry
	})
	if err != nil {
		return ports.UploadTicket{}, fmt.Errorf("s3 cas presign put %s: %w", ref.URI(), err)
	}
	return ports.UploadTicket{
		URL:       request.URL,
		Method:    http.MethodPut,
		Headers:   request.SignedHeader,
		Key:       ref,
		ExpiresAt: time.Now().UTC().Add(expiry),
	}, nil
}

func (s *Store) objectKey(scope ports.SecurityScope, ref domain.CASRef) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if err := ref.Validate(); err != nil {
		return "", err
	}
	return buildObjectKey(s.prefix, string(scope.TenantID), scope.TrustDomain, ref.Hex())
}

func (s *Store) resolveTTL(ttl time.Duration) (time.Duration, error) {
	if ttl <= 0 {
		ttl = s.presignTTL
	}
	if ttl <= 0 {
		ttl = defaultPresignTTL
	}
	if ttl > maxPresignTTL {
		return 0, fmt.Errorf("presign ttl %s exceeds the maximum of %s", ttl, maxPresignTTL)
	}
	return ttl, nil
}

func buildObjectKey(prefix, tenantID, trustDomain, digest string) (string, error) {
	key, err := cas.BlobKey(tenantID, trustDomain, digest)
	if err != nil {
		return "", err
	}
	if prefix == "" {
		return key, nil
	}
	return prefix + "/" + key, nil
}

func normalizeConfig(cfg Config) (Config, error) {
	bucket := strings.TrimSpace(cfg.Bucket)
	if bucket == "" {
		return Config{}, errors.New("s3 cas: bucket must not be empty")
	}
	if strings.ContainsAny(bucket, "/\\ \t\r\n") {
		return Config{}, fmt.Errorf("s3 cas: invalid bucket name %q", bucket)
	}
	if cfg.MaxBlobBytes < 0 {
		return Config{}, errors.New("s3 cas: max blob bytes must not be negative")
	}
	if (cfg.AccessKeyID == "") != (cfg.SecretAccessKey == "") {
		return Config{}, errors.New("s3 cas: access key id and secret access key must be set together")
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return Config{}, fmt.Errorf("s3 cas: endpoint %q must be an absolute URL", cfg.Endpoint)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return Config{}, fmt.Errorf("s3 cas: endpoint %q must use http or https", cfg.Endpoint)
		}
		endpoint = strings.TrimRight(endpoint, "/")
	}
	prefix, err := normalizePrefix(cfg.Prefix)
	if err != nil {
		return Config{}, err
	}
	presignTTL := cfg.PresignTTL
	if presignTTL <= 0 {
		presignTTL = defaultPresignTTL
	}
	if presignTTL > maxPresignTTL {
		return Config{}, fmt.Errorf("s3 cas: presign ttl %s exceeds the maximum of %s", presignTTL, maxPresignTTL)
	}
	return Config{
		Endpoint:        endpoint,
		Region:          strings.TrimSpace(cfg.Region),
		Bucket:          bucket,
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		ForcePathStyle:  cfg.ForcePathStyle,
		MaxBlobBytes:    cfg.MaxBlobBytes,
		PresignTTL:      presignTTL,
		Prefix:          prefix,
		UsePathStyle:    cfg.UsePathStyle,
	}, nil
}

func normalizePrefix(raw string) (string, error) {
	prefix := strings.Trim(strings.TrimSpace(raw), "/")
	if prefix == "" {
		return "", nil
	}
	for _, part := range strings.Split(prefix, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("s3 cas: invalid key prefix %q", raw)
		}
	}
	return prefix, nil
}

func clientOptions(cfg Config) []func(*awss3.Options) {
	return []func(*awss3.Options){
		func(options *awss3.Options) {
			options.UsePathStyle = cfg.UsePathStyle || cfg.ForcePathStyle
			if cfg.Endpoint != "" {
				options.EndpointResolverV2 = staticEndpointResolver{endpoint: cfg.Endpoint}
			}
		},
	}
}

// staticEndpointResolver overrides the service endpoint while delegating to the
// default S3 rules so bucket addressing and path style stay consistent.
type staticEndpointResolver struct {
	endpoint string
}

func (r staticEndpointResolver) ResolveEndpoint(ctx context.Context, params awss3.EndpointParameters) (smithyendpoints.Endpoint, error) {
	params.Endpoint = aws.String(r.endpoint)
	return awss3.NewDefaultEndpointResolverV2().ResolveEndpoint(ctx, params)
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var noSuchKey *s3types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var noSuchBucket *s3types.NoSuchBucket
	if errors.As(err, &noSuchBucket) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "NoSuchBucket", "404":
			return true
		}
	}
	return false
}

func copyBlob(ctx context.Context, dst io.Writer, src io.Reader, maxBytes int64) (int64, error) {
	buffer := make([]byte, copyBufferSize)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			written += int64(n)
			if maxBytes > 0 && written > maxBytes {
				return written, fmt.Errorf("%w: limit is %d bytes", ErrBlobTooLarge, maxBytes)
			}
			if _, writeErr := dst.Write(buffer[:n]); writeErr != nil {
				return written, fmt.Errorf("write blob: %w", writeErr)
			}
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, fmt.Errorf("read blob: %w", readErr)
		}
	}
}
