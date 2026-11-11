package proofverify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	casfs "github.com/patchproof/patchproof/internal/adapters/cas/fs"
	cass3 "github.com/patchproof/patchproof/internal/adapters/cas/s3"
	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/platform/signing"
	"github.com/patchproof/patchproof/internal/ports"
	"github.com/patchproof/patchproof/pkg/evidence"
)

type Options struct {
	ReportPath       string
	CASRoot          string
	S3Endpoint       string
	S3Region         string
	S3Bucket         string
	S3AccessKey      string
	S3SecretKey      string
	Keys             []string
	PolicyPath       string
	RequireSignature bool
	Deep             bool
	MaxAge           time.Duration
	Now              time.Time
}

func Verify(ctx context.Context, options Options) (evidence.VerificationResult, domain.Report, error) {
	var report domain.Report
	if options.ReportPath == "" {
		return evidence.VerificationResult{}, report, fmt.Errorf("report path is required")
	}
	raw, err := os.ReadFile(options.ReportPath)
	if err != nil {
		return evidence.VerificationResult{}, report, fmt.Errorf("read report: %w", err)
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return evidence.VerificationResult{}, report, fmt.Errorf("decode report: %w", err)
	}
	trust := evidence.NewTrustStore()
	for _, entry := range options.Keys {
		keyID, path := splitKey(entry)
		publicKey, err := signing.LoadPublicKey(path)
		if err != nil {
			return evidence.VerificationResult{}, report, fmt.Errorf("load key %s: %w", path, err)
		}
		if err := trust.Add(evidence.TrustedKey{KeyID: keyID, Identity: keyID, PublicKey: publicKey}); err != nil {
			return evidence.VerificationResult{}, report, err
		}
	}
	var store ports.ArtifactStore
	switch {
	case options.CASRoot != "":
		created, err := casfs.New(casfs.Config{Root: options.CASRoot, CreateRoot: false})
		if err != nil {
			return evidence.VerificationResult{}, report, fmt.Errorf("open filesystem CAS: %w", err)
		}
		store = created
	case options.S3Bucket != "":
		created, err := cass3.New(ctx, cass3.Config{
			Endpoint: options.S3Endpoint, Region: options.S3Region, Bucket: options.S3Bucket,
			AccessKeyID: options.S3AccessKey, SecretAccessKey: options.S3SecretKey, UsePathStyle: true,
		})
		if err != nil {
			return evidence.VerificationResult{}, report, fmt.Errorf("open S3 CAS: %w", err)
		}
		store = created
	default:
		return evidence.VerificationResult{}, report, fmt.Errorf("a CAS root or S3 bucket is required")
	}
	verifyOptions := evidence.VerifyOptions{
		Trust: trust, Now: options.Now, RequireSignature: options.RequireSignature,
		MaxReportAge: options.MaxAge, Deep: options.Deep,
	}
	if verifyOptions.Now.IsZero() {
		verifyOptions.Now = time.Now().UTC()
	}
	if options.PolicyPath != "" {
		policyRaw, err := os.ReadFile(options.PolicyPath)
		if err != nil {
			return evidence.VerificationResult{}, report, fmt.Errorf("read policy: %w", err)
		}
		document, err := domain.ParsePolicyDocument(policyRaw)
		if err != nil {
			return evidence.VerificationResult{}, report, fmt.Errorf("parse policy: %w", err)
		}
		verifyOptions.Policy = &document
	}
	source := &MultiScopeSource{Store: store, TenantID: report.TenantID, Region: report.Provenance.Region}
	result, err := evidence.VerifyReport(ctx, report, source, verifyOptions)
	if err != nil {
		return result, report, err
	}
	return result, report, nil
}

func splitKey(entry string) (string, string) {
	if index := strings.Index(entry, "="); index > 0 {
		return entry[:index], entry[index+1:]
	}
	base := filepath.Base(entry)
	extension := filepath.Ext(base)
	return strings.TrimSuffix(base, extension), entry
}
