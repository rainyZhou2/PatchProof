package domain

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

type (
	TenantID    string
	ProjectID   string
	RunID       string
	StepID      string
	AttemptID   string
	EvidenceID  string
	ReportID    string
	EventID     string
	ClaimID     string
	ShardID     int
	ClusterID   string
	SignatureID string
)

func NewTenantID() TenantID       { return TenantID(uuid.NewString()) }
func NewProjectID() ProjectID     { return ProjectID(uuid.NewString()) }
func NewRunID() RunID             { return RunID(uuid.NewString()) }
func NewStepID() StepID           { return StepID(uuid.NewString()) }
func NewAttemptID() AttemptID     { return AttemptID(uuid.NewString()) }
func NewEvidenceID() EvidenceID   { return EvidenceID(uuid.NewString()) }
func NewReportID() ReportID       { return ReportID(uuid.NewString()) }
func NewEventID() EventID         { return EventID(uuid.NewString()) }
func NewSignatureID() SignatureID { return SignatureID(uuid.NewString()) }

func ParseTenantID(s string) (TenantID, error) {
	return parseUUIDID(s, "tenant", func(v string) TenantID { return TenantID(v) })
}
func ParseProjectID(s string) (ProjectID, error) {
	return parseUUIDID(s, "project", func(v string) ProjectID { return ProjectID(v) })
}
func ParseRunID(s string) (RunID, error) {
	return parseUUIDID(s, "run", func(v string) RunID { return RunID(v) })
}
func ParseStepID(s string) (StepID, error) {
	return parseUUIDID(s, "step", func(v string) StepID { return StepID(v) })
}
func ParseAttemptID(s string) (AttemptID, error) {
	return parseUUIDID(s, "attempt", func(v string) AttemptID { return AttemptID(v) })
}

func parseUUIDID[T ~string](raw, kind string, conv func(string) T) (T, error) {
	var zero T
	if strings.TrimSpace(raw) == "" {
		return zero, &ValidationError{Field: kind + "_id", Reason: "must not be empty"}
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return zero, &ValidationError{Field: kind + "_id", Reason: "must be a UUID"}
	}
	if parsed.Version() != 4 && parsed.Version() != 7 {
		return zero, &ValidationError{Field: kind + "_id", Reason: "must be a random or time-ordered UUID"}
	}
	return conv(parsed.String()), nil
}

func (id TenantID) String() string   { return string(id) }
func (id ProjectID) String() string  { return string(id) }
func (id RunID) String() string      { return string(id) }
func (id StepID) String() string     { return string(id) }
func (id AttemptID) String() string  { return string(id) }
func (id EvidenceID) String() string { return string(id) }
func (id ReportID) String() string   { return string(id) }
func (id ClusterID) String() string  { return string(id) }
func (id EventID) String() string    { return string(id) }
func (id ClaimID) String() string    { return string(id) }

func (id TenantID) Validate() error {
	_, err := ParseTenantID(string(id))
	return err
}

func (id RunID) Validate() error {
	_, err := ParseRunID(string(id))
	return err
}

func (id StepID) Validate() error {
	_, err := ParseStepID(string(id))
	return err
}

func (id AttemptID) Validate() error {
	_, err := ParseAttemptID(string(id))
	return err
}

func (id EvidenceID) Validate() error {
	_, err := parseUUIDID(string(id), "evidence", func(v string) EvidenceID { return EvidenceID(v) })
	return err
}

func (id ReportID) Validate() error {
	_, err := parseUUIDID(string(id), "report", func(v string) ReportID { return ReportID(v) })
	return err
}

func RequireNonEmpty(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return &ValidationError{Field: field, Reason: "must not be empty"}
	}
	return nil
}

func RequireMaxLen(field, value string, max int) error {
	if len(value) > max {
		return &ValidationError{Field: field, Reason: fmt.Sprintf("must be at most %d characters", max)}
	}
	return nil
}
