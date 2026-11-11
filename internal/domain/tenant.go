package domain

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

type Tenant struct {
	ID                TenantID     `json:"tenant_id"`
	HomeRegion        string       `json:"home_region"`
	SecurityTier      SecurityTier `json:"security_tier"`
	Status            TenantStatus `json:"status"`
	ActiveWriterEpoch int64        `json:"active_writer_epoch"`
	Quota             TenantQuota  `json:"quota"`
	DisplayName       string       `json:"display_name"`
	CreatedAt         time.Time    `json:"created_at"`
	UpdatedAt         time.Time    `json:"updated_at"`
}

type TenantQuota struct {
	MaxConcurrentRuns  int     `json:"max_concurrent_runs"`
	MaxConcurrentSteps int     `json:"max_concurrent_steps"`
	CPUCoreSecondsDay  float64 `json:"cpu_core_seconds_per_day"`
	FuzzSecondsDay     float64 `json:"fuzz_seconds_per_day"`
	StorageBytes       int64   `json:"storage_bytes"`
	APIRPS             int     `json:"api_rps"`
}

func (q TenantQuota) Validate() error {
	if q.MaxConcurrentRuns < 0 || q.MaxConcurrentSteps < 0 {
		return &ValidationError{Field: "quota", Reason: "concurrency limits must not be negative"}
	}
	if q.CPUCoreSecondsDay < 0 || q.FuzzSecondsDay < 0 || q.StorageBytes < 0 || q.APIRPS < 0 {
		return &ValidationError{Field: "quota", Reason: "quota values must not be negative"}
	}
	return nil
}

func (t Tenant) Validate() error {
	if err := t.ID.Validate(); err != nil {
		return err
	}
	if err := RequireNonEmpty("home_region", t.HomeRegion); err != nil {
		return err
	}
	if !t.SecurityTier.Valid() {
		return &ValidationError{Field: "security_tier", Reason: "unknown security tier"}
	}
	if !t.Status.Valid() {
		return &ValidationError{Field: "status", Reason: "unknown tenant status"}
	}
	return t.Quota.Validate()
}

type Project struct {
	ID                  ProjectID `json:"project_id"`
	TenantID            TenantID  `json:"tenant_id"`
	Provider            string    `json:"provider"`
	RepositoryURI       string    `json:"repository_uri"`
	DefaultBranch       string    `json:"default_branch"`
	DefaultPolicyDigest string    `json:"default_policy_digest"`
	AllowedRegions      []string  `json:"allowed_regions"`
	Classification      string    `json:"classification"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

func (p Project) Validate() error {
	if p.ID.String() == "" {
		return &ValidationError{Field: "project_id", Reason: "must not be empty"}
	}
	if err := p.TenantID.Validate(); err != nil {
		return err
	}
	if err := RequireNonEmpty("provider", p.Provider); err != nil {
		return err
	}
	if err := RequireNonEmpty("repository_uri", p.RepositoryURI); err != nil {
		return err
	}
	u, err := url.Parse(p.RepositoryURI)
	if err != nil || u.Scheme == "" {
		return &ValidationError{Field: "repository_uri", Reason: "must be an absolute URI"}
	}
	if !strings.EqualFold(u.Scheme, "file") && u.Host == "" {
		return &ValidationError{Field: "repository_uri", Reason: "must include a host"}
	}
	if strings.EqualFold(u.Scheme, "file") {
		if p.Classification != "trusted-local" {
			return &ValidationError{Field: "repository_uri", Reason: "file repositories are only allowed for trusted-local projects"}
		}
		if !filepath.IsAbs(u.Path) {
			return &ValidationError{Field: "repository_uri", Reason: "file repository path must be absolute"}
		}
	}
	return nil
}

type PolicySource string

const (
	PolicySourceAdmin      PolicySource = "ADMIN"
	PolicySourceBaseBranch PolicySource = "BASE_BRANCH"
	PolicySourceRequest    PolicySource = "REQUEST"
)

func (s PolicySource) Valid() bool {
	return s == PolicySourceAdmin || s == PolicySourceBaseBranch || s == PolicySourceRequest
}

type Policy struct {
	TenantID  TenantID     `json:"tenant_id"`
	ProjectID ProjectID    `json:"project_id,omitempty"`
	Digest    string       `json:"digest"`
	Version   string       `json:"version"`
	Source    PolicySource `json:"source"`
	Trusted   bool         `json:"trusted"`
	Document  []byte       `json:"document"`
	CreatedBy string       `json:"created_by"`
	CreatedAt time.Time    `json:"created_at"`
}

func (p Policy) Validate() error {
	if err := p.TenantID.Validate(); err != nil {
		return err
	}
	if err := RequireNonEmpty("policy_digest", p.Digest); err != nil {
		return err
	}
	if !p.Source.Valid() {
		return &ValidationError{Field: "policy_source", Reason: "unknown policy source"}
	}
	if len(p.Document) == 0 {
		return &ValidationError{Field: "policy_document", Reason: "must not be empty"}
	}
	if !strings.HasPrefix(p.Digest, "sha256:") {
		return &ValidationError{Field: "policy_digest", Reason: "must use sha256 prefix"}
	}
	return nil
}

type SnapshotManifest struct {
	Digest     string         `json:"digest"`
	Commit     string         `json:"commit"`
	Repository string         `json:"repository"`
	Files      []SnapshotFile `json:"files"`
	TotalBytes int64          `json:"total_bytes"`
	FileCount  int            `json:"file_count"`
	Submodules []string       `json:"submodules,omitempty"`
	FetchedAt  time.Time      `json:"fetched_at"`
	BaseDigest string         `json:"base_digest,omitempty"`
}

type SnapshotFile struct {
	Path       string `json:"path"`
	Digest     string `json:"digest"`
	Size       int64  `json:"size"`
	Mode       string `json:"mode"`
	Executable bool   `json:"executable"`
}

type RepositoryRef struct {
	Provider    string `json:"provider"`
	URI         string `json:"uri"`
	Commit      string `json:"commit"`
	TenantID    string `json:"tenant_id"`
	TrustDomain string `json:"trust_domain"`
	MaxBytes    int64  `json:"max_bytes"`
}

func (r RepositoryRef) Validate() error {
	if err := RequireNonEmpty("provider", r.Provider); err != nil {
		return err
	}
	if err := RequireNonEmpty("repository_uri", r.URI); err != nil {
		return err
	}
	if len(r.Commit) < 7 || len(r.Commit) > 64 {
		return &ValidationError{Field: "commit", Reason: "must be a full or abbreviated commit hash"}
	}
	for _, c := range r.Commit {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return &ValidationError{Field: "commit", Reason: "must be hexadecimal"}
		}
	}
	if r.MaxBytes < 0 {
		return fmt.Errorf("%w: max_bytes", &ValidationError{Field: "max_bytes", Reason: "must not be negative"})
	}
	return nil
}
