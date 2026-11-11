package domain

import "strings"

// ReplaySpec pins a counterexample input so a follow-up run can re-execute the
// exact stored case instead of generating a fresh corpus.
type ReplaySpec struct {
	SourceRunID RunID   `json:"source_run_id"`
	CaseID      string  `json:"case_id"`
	ClaimID     ClaimID `json:"claim_id"`
	InputRef    CASRef  `json:"input_ref"`
	InputCodec  string  `json:"input_codec"`
}

func (r ReplaySpec) Validate() error {
	if err := r.SourceRunID.Validate(); err != nil {
		return err
	}
	if err := RequireNonEmpty("replay.case_id", r.CaseID); err != nil {
		return err
	}
	if err := RequireMaxLen("replay.case_id", r.CaseID, 200); err != nil {
		return err
	}
	if err := RequireNonEmpty("replay.claim_id", string(r.ClaimID)); err != nil {
		return err
	}
	if err := r.InputRef.Validate(); err != nil {
		return err
	}
	if err := RequireNonEmpty("replay.input_codec", r.InputCodec); err != nil {
		return err
	}
	return nil
}

func (r ReplaySpec) Normalized() ReplaySpec {
	r.CaseID = strings.TrimSpace(r.CaseID)
	r.ClaimID = ClaimID(strings.TrimSpace(string(r.ClaimID)))
	r.InputCodec = strings.TrimSpace(r.InputCodec)
	return r
}
