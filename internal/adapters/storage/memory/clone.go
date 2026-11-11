package memory

import (
	"github.com/patchproof/patchproof/internal/domain"
)

func cloneTenant(t domain.Tenant) domain.Tenant {
	c := t
	c.Quota = t.Quota
	return c
}

func cloneProject(p domain.Project) domain.Project {
	c := p
	c.AllowedRegions = cloneStrings(p.AllowedRegions)
	return c
}

func clonePolicy(p domain.Policy) domain.Policy {
	c := p
	if p.Document != nil {
		c.Document = append([]byte(nil), p.Document...)
	}
	return c
}

func cloneRun(r domain.Run) domain.Run {
	c := r
	c.RequestedChecks = cloneStrings(r.RequestedChecks)
	c.PlannedAt = cloneTime(r.PlannedAt)
	c.StartedAt = cloneTime(r.StartedAt)
	c.FinishedAt = cloneTime(r.FinishedAt)
	if r.Replay != nil {
		replay := *r.Replay
		c.Replay = &replay
	}
	return c
}

func cloneRequirements(r domain.ResourceRequirements) domain.ResourceRequirements {
	c := r
	c.RequiredFeatures = cloneStrings(r.RequiredFeatures)
	return c
}

func cloneStep(s domain.Step) domain.Step {
	c := s
	c.Requirements = cloneRequirements(s.Requirements)
	c.Options = cloneStringMap(s.Options)
	c.LeaseDeadline = cloneTime(s.LeaseDeadline)
	c.NextAttemptAt = cloneTime(s.NextAttemptAt)
	c.StartedAt = cloneTime(s.StartedAt)
	c.FinishedAt = cloneTime(s.FinishedAt)
	return c
}

func cloneAttempt(a domain.Attempt) domain.Attempt {
	c := a
	c.StartedAt = cloneTime(a.StartedAt)
	c.FinishedAt = cloneTime(a.FinishedAt)
	return c
}

func cloneLease(l domain.Lease) domain.Lease {
	c := l
	c.Requirements = cloneRequirements(l.Requirements)
	return c
}

func cloneClaim(c domain.Claim) domain.Claim {
	out := c
	out.Assumptions = cloneStrings(c.Assumptions)
	return out
}

func cloneClaims(claims []domain.Claim) []domain.Claim {
	if claims == nil {
		return nil
	}
	out := make([]domain.Claim, len(claims))
	for i := range claims {
		out[i] = cloneClaim(claims[i])
	}
	return out
}

func cloneCASRefs(refs []domain.CASRef) []domain.CASRef {
	if refs == nil {
		return nil
	}
	return append([]domain.CASRef(nil), refs...)
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneEvidence(e domain.Evidence) domain.Evidence {
	c := e
	c.Scope.Details = cloneStringMap(e.Scope.Details)
	c.Environment.Extra = cloneStringMap(e.Environment.Extra)
	c.Assumptions = cloneStrings(e.Assumptions)
	c.Inputs = cloneCASRefs(e.Inputs)
	c.Artifacts = cloneCASRefs(e.Artifacts)
	c.Limits = cloneStrings(e.Limits)
	return c
}

func cloneCounterexample(c domain.Counterexample) domain.Counterexample {
	out := c
	out.Nondeterminism = cloneStrings(c.Nondeterminism)
	return out
}

func cloneClaimAssessments(in []domain.ClaimAssessment) []domain.ClaimAssessment {
	if in == nil {
		return nil
	}
	out := make([]domain.ClaimAssessment, len(in))
	for i, c := range in {
		out[i] = c
		out[i].EvidenceIDs = append([]domain.EvidenceID(nil), c.EvidenceIDs...)
	}
	return out
}

func cloneFindings(in []domain.Finding) []domain.Finding {
	if in == nil {
		return nil
	}
	out := make([]domain.Finding, len(in))
	for i, f := range in {
		out[i] = f
		out[i].EvidenceIDs = append([]domain.EvidenceID(nil), f.EvidenceIDs...)
	}
	return out
}

func cloneSignatures(in []domain.Signature) []domain.Signature {
	if in == nil {
		return nil
	}
	out := make([]domain.Signature, len(in))
	for i, s := range in {
		out[i] = s
		if s.Signature != nil {
			out[i].Signature = append([]byte(nil), s.Signature...)
		}
	}
	return out
}

func cloneReport(r domain.Report) domain.Report {
	c := r
	c.RequestedChecks = cloneStrings(r.RequestedChecks)
	c.CompletedChecks = cloneStrings(r.CompletedChecks)
	c.Claims = cloneClaimAssessments(r.Claims)
	c.Counterexamples = nil
	if r.Counterexamples != nil {
		c.Counterexamples = make([]domain.Counterexample, len(r.Counterexamples))
		for i := range r.Counterexamples {
			c.Counterexamples[i] = cloneCounterexample(r.Counterexamples[i])
		}
	}
	c.Findings = cloneFindings(r.Findings)
	c.Assumptions = cloneStrings(r.Assumptions)
	c.Limitations = cloneStrings(r.Limitations)
	c.Provenance.EngineVersions = cloneStringMap(r.Provenance.EngineVersions)
	c.Signatures = cloneSignatures(r.Signatures)
	return c
}

func cloneDAG(d domain.DAG) domain.DAG {
	c := d
	c.Steps = nil
	if d.Steps != nil {
		c.Steps = make([]domain.Step, len(d.Steps))
		for i := range d.Steps {
			c.Steps[i] = cloneStep(d.Steps[i])
		}
	}
	c.Claims = cloneClaims(d.Claims)
	c.Dependencies = nil
	if d.Dependencies != nil {
		c.Dependencies = make(map[domain.StepID][]domain.StepID, len(d.Dependencies))
		for id, deps := range d.Dependencies {
			c.Dependencies[id] = append([]domain.StepID(nil), deps...)
		}
	}
	return c
}

func cloneCluster(c domain.Cluster) domain.Cluster {
	out := c
	out.Architectures = cloneStrings(c.Architectures)
	out.Languages = cloneStrings(c.Languages)
	out.Engines = cloneStrings(c.Engines)
	out.RuntimeClasses = cloneStrings(c.RuntimeClasses)
	out.Labels = cloneStringMap(c.Labels)
	if c.IsolationTiers != nil {
		out.IsolationTiers = append([]domain.IsolationTier(nil), c.IsolationTiers...)
	}
	return out
}

func cloneOutboxEvent(e domain.OutboxEvent) domain.OutboxEvent {
	c := e
	if e.Payload != nil {
		c.Payload = append([]byte(nil), e.Payload...)
	}
	c.DeliveredAt = cloneTime(e.DeliveredAt)
	return c
}

func cloneAuditEntry(e domain.AuditEntry) domain.AuditEntry {
	return e
}

func cloneMetrics(m domain.StepMetrics) domain.StepMetrics {
	return m
}
