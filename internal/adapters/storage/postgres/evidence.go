package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

// AppendEvidence inserts immutable evidence documents.
func (s *Store) AppendEvidence(ctx context.Context, evidence []domain.Evidence) error {
	if len(evidence) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin append evidence: %w", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()
	seen := make(map[domain.EvidenceID]struct{}, len(evidence))
	for i := range evidence {
		document := evidence[i]
		if err := document.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[document.ID]; duplicate {
			return &domain.ConflictError{Resource: "evidence", ID: string(document.ID), Reason: "duplicate evidence id in batch"}
		}
		seen[document.ID] = struct{}{}
		var runExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM runs WHERE run_id = $1::uuid AND tenant_id = $2::uuid
		)`, document.RunID.String(), document.TenantID.String()).Scan(&runExists); err != nil {
			return mapError(err, "run", document.RunID.String())
		}
		if !runExists {
			return notFound("run", document.RunID.String())
		}
		var evidenceExists bool
		if err := tx.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM evidence_documents WHERE evidence_id = $1::uuid)",
			document.ID.String()).Scan(&evidenceExists); err != nil {
			return mapError(err, "evidence", document.ID.String())
		}
		if evidenceExists {
			return &domain.ConflictError{Resource: "evidence", ID: string(document.ID), Reason: "already exists"}
		}
		createdAt := document.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		} else {
			createdAt = createdAt.UTC()
		}
		raw, err := encodeJSON(document)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO evidence_documents (
			evidence_id, tenant_id, run_id, step_id, claim_id, kind, result, document, created_at
		) VALUES ($1::uuid, $2::uuid, $3::uuid, NULLIF($4, '')::uuid, $5, $6, $7, $8, $9)`,
			document.ID.String(), document.TenantID.String(), document.RunID.String(),
			document.StepID.String(), string(document.ClaimID), string(document.Kind),
			string(document.Result), raw, createdAt,
		); err != nil {
			return mapError(err, "evidence", document.ID.String())
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit append evidence: %w", err)
	}
	finished = true
	return nil
}

// ListEvidence returns the evidence documents of a run in insertion order.
func (s *Store) ListEvidence(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) ([]domain.Evidence, error) {
	if err := tenantID.Validate(); err != nil {
		return nil, err
	}
	if err := runID.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT document FROM evidence_documents
		WHERE tenant_id = $1::uuid AND run_id = $2::uuid
		ORDER BY created_at, evidence_id`, tenantID.String(), runID.String())
	if err != nil {
		return nil, mapError(err, "evidence", runID.String())
	}
	return collectEvidence(rows)
}

// ListCounterexamples returns the counterexamples of a run in creation order.
func (s *Store) ListCounterexamples(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) ([]domain.Counterexample, error) {
	if err := tenantID.Validate(); err != nil {
		return nil, err
	}
	if err := runID.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT document FROM counterexamples
		WHERE tenant_id = $1::uuid AND run_id = $2::uuid
		ORDER BY created_at, case_id`, tenantID.String(), runID.String())
	if err != nil {
		return nil, mapError(err, "counterexample", runID.String())
	}
	return collectCounterexamples(rows)
}

// PutReport stores the latest report for a run.
func (s *Store) PutReport(ctx context.Context, report domain.Report) error {
	if err := report.Validate(); err != nil {
		return err
	}
	generatedAt := report.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = time.Now().UTC()
	} else {
		generatedAt = generatedAt.UTC()
	}
	raw, err := encodeJSON(report)
	if err != nil {
		return err
	}
	var runExists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM runs WHERE run_id = $1::uuid AND tenant_id = $2::uuid
	)`, report.RunID.String(), report.TenantID.String()).Scan(&runExists); err != nil {
		return mapError(err, "run", report.RunID.String())
	}
	if !runExists {
		return notFound("run", report.RunID.String())
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO reports (
		run_id, tenant_id, project_id, report_id, verdict, attestation_status, merkle_root,
		document, created_at, updated_at
	) VALUES ($1::uuid, $2::uuid, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, $5, $6, $7, $8, $9, $9)
	ON CONFLICT (run_id) DO UPDATE SET
		tenant_id = EXCLUDED.tenant_id,
		project_id = EXCLUDED.project_id,
		report_id = EXCLUDED.report_id,
		verdict = EXCLUDED.verdict,
		attestation_status = EXCLUDED.attestation_status,
		merkle_root = EXCLUDED.merkle_root,
		document = EXCLUDED.document,
		created_at = EXCLUDED.created_at,
		updated_at = EXCLUDED.updated_at`,
		report.RunID.String(), report.TenantID.String(), report.ProjectID.String(),
		report.ReportID.String(), string(report.Verdict), string(report.Attestation),
		report.MerkleRoot, raw, generatedAt,
	)
	if err != nil {
		return mapError(err, "report", report.RunID.String())
	}
	return nil
}

// GetReport loads the latest report for a run.
func (s *Store) GetReport(ctx context.Context, tenantID domain.TenantID, runID domain.RunID) (domain.Report, error) {
	if err := tenantID.Validate(); err != nil {
		return domain.Report{}, err
	}
	if err := runID.Validate(); err != nil {
		return domain.Report{}, err
	}
	report, err := scanReport(s.pool.QueryRow(ctx,
		"SELECT document FROM reports WHERE tenant_id = $1::uuid AND run_id = $2::uuid",
		tenantID.String(), runID.String()))
	if err != nil {
		return domain.Report{}, mapError(err, "report", runID.String())
	}
	return report, nil
}
