package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/app/admin"
	"github.com/patchproof/patchproof/internal/app/runservice"
	"github.com/patchproof/patchproof/internal/domain"
)

type budgetRequest struct {
	CPUSeconds       float64 `json:"cpu_seconds"`
	WallSeconds      int64   `json:"wall_seconds"`
	FuzzCPUSeconds   float64 `json:"fuzz_cpu_seconds"`
	MaxParallelSteps int     `json:"max_parallel_steps"`
}

type createRunRequest struct {
	ProjectID       string        `json:"project_id"`
	BaseCommit      string        `json:"base_commit"`
	HeadCommit      string        `json:"head_commit"`
	PatchDigest     string        `json:"patch_digest"`
	PolicyRef       string        `json:"policy_ref"`
	PolicyDigest    string        `json:"policy_digest"`
	RequestedChecks []string      `json:"requested_checks"`
	Budget          budgetRequest `json:"budget"`
	Region          string        `json:"region"`
}

type runResponse struct {
	Run       domain.Run `json:"run"`
	StatusURL string     `json:"status_url"`
	ReportURL string     `json:"report_url"`
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireRoles(w, r, "operator", "admin")
	if !ok {
		return
	}
	principal, _ := principalFrom(r.Context())
	requestKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if requestKey == "" {
		writeProblem(w, r, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header is required")
		return
	}
	if len(requestKey) > 200 {
		writeProblem(w, r, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Idempotency-Key must be at most 200 characters")
		return
	}
	var body createRunRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	projectID, err := domain.ParseProjectID(body.ProjectID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	run, err := s.deps.Runs.Submit(r.Context(), runservice.SubmitInput{
		TenantID: principal.TenantID, ProjectID: projectID, RequestKey: requestKey,
		BaseCommit: body.BaseCommit, HeadCommit: body.HeadCommit, PatchDigest: body.PatchDigest,
		PolicyRef: body.PolicyRef, PolicyDigest: body.PolicyDigest,
		RequestedChecks: body.RequestedChecks,
		Budget: domain.Budget{
			CPUSeconds: body.Budget.CPUSeconds, WallSeconds: body.Budget.WallSeconds,
			FuzzCPUSeconds: body.Budget.FuzzCPUSeconds, MaxParallelSteps: body.Budget.MaxParallelSteps,
		},
		Region: body.Region, CreatedBy: actor,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	statusURL := s.runURL(run.ID)
	w.Header().Set("Location", statusURL)
	writeJSON(w, http.StatusAccepted, runResponse{Run: run, StatusURL: statusURL, ReportURL: statusURL + "/report"})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRoles(w, r, "viewer", "operator", "admin", "platform-admin"); !ok {
		return
	}
	principal, _ := principalFrom(r.Context())
	runID, ok := parseRunID(w, r)
	if !ok {
		return
	}
	run, err := s.deps.Runs.Get(r.Context(), principal.TenantID, runID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, runResponse{Run: run, StatusURL: s.runURL(run.ID), ReportURL: s.runURL(run.ID) + "/report"})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRoles(w, r, "viewer", "operator", "admin", "platform-admin"); !ok {
		return
	}
	principal, _ := principalFrom(r.Context())
	query := r.URL.Query()
	filter := domain.RunFilter{TenantID: principal.TenantID, Cursor: query.Get("cursor")}
	if raw := query.Get("project_id"); raw != "" {
		projectID, err := domain.ParseProjectID(raw)
		if err != nil {
			writeError(w, r, err)
			return
		}
		filter.ProjectID = projectID
	}
	for _, raw := range query["status"] {
		for _, value := range strings.Split(raw, ",") {
			status := domain.RunStatus(strings.ToUpper(strings.TrimSpace(value)))
			if !status.Valid() {
				writeProblem(w, r, http.StatusBadRequest, "INVALID_STATUS", "unknown run status "+value)
				return
			}
			filter.Statuses = append(filter.Statuses, status)
		}
	}
	if raw := query.Get("created_after"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "INVALID_TIME", "created_after must be RFC3339")
			return
		}
		filter.CreatedAfter = &parsed
	}
	if raw := query.Get("created_before"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "INVALID_TIME", "created_before must be RFC3339")
			return
		}
		filter.CreatedBefore = &parsed
	}
	limit, err := parseLimit(query.Get("limit"), 50, 500)
	if err != nil {
		writeError(w, r, err)
		return
	}
	filter.Limit = limit
	page, err := s.deps.Runs.List(r.Context(), filter)
	if err != nil {
		writeError(w, r, err)
		return
	}
	type listResponse struct {
		Runs       []domain.Run `json:"runs"`
		NextCursor string       `json:"next_cursor,omitempty"`
	}
	writeJSON(w, http.StatusOK, listResponse{Runs: page.Runs, NextCursor: page.NextCursor})
}

func (s *Server) handleRunSteps(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRoles(w, r, "viewer", "operator", "admin", "platform-admin"); !ok {
		return
	}
	principal, _ := principalFrom(r.Context())
	runID, ok := parseRunID(w, r)
	if !ok {
		return
	}
	dag, err := s.deps.Runs.DAG(r.Context(), principal.TenantID, runID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	type stepsResponse struct {
		RunID        domain.RunID                      `json:"run_id"`
		Steps        []domain.Step                     `json:"steps"`
		Dependencies map[domain.StepID][]domain.StepID `json:"dependencies"`
		Claims       []domain.Claim                    `json:"claims"`
	}
	dependencies := dag.Dependencies
	if dependencies == nil {
		dependencies = map[domain.StepID][]domain.StepID{}
	}
	writeJSON(w, http.StatusOK, stepsResponse{RunID: runID, Steps: dag.Steps, Dependencies: dependencies, Claims: dag.Claims})
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireRoles(w, r, "operator", "admin")
	if !ok {
		return
	}
	principal, _ := principalFrom(r.Context())
	runID, ok := parseRunID(w, r)
	if !ok {
		return
	}
	run, err := s.deps.Runs.Cancel(r.Context(), principal.TenantID, runID, actor)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, runResponse{Run: run, StatusURL: s.runURL(run.ID), ReportURL: s.runURL(run.ID) + "/report"})
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRoles(w, r, "viewer", "operator", "admin", "platform-admin"); !ok {
		return
	}
	principal, _ := principalFrom(r.Context())
	runID, ok := parseRunID(w, r)
	if !ok {
		return
	}
	report, err := s.deps.Runs.Report(r.Context(), principal.TenantID, runID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "json"
	}
	reporter, ok := s.deps.Reporters[format]
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "UNSUPPORTED_FORMAT", "unknown report format "+format)
		return
	}
	evidenceList, _ := s.deps.Runs.Evidence(r.Context(), principal.TenantID, runID)
	payload, err := reporter.Render(r.Context(), report, evidenceList)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", reporter.ContentType())
	w.Header().Set("X-PatchProof-Merkle-Root", report.MerkleRoot)
	w.Header().Set("X-PatchProof-Attestation", string(report.Attestation))
	w.Header().Set("Content-Disposition", `inline; filename="patchproof-`+runID.String()+`.`+format+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func (s *Server) handleEvidence(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRoles(w, r, "viewer", "operator", "admin", "platform-admin"); !ok {
		return
	}
	principal, _ := principalFrom(r.Context())
	runID, ok := parseRunID(w, r)
	if !ok {
		return
	}
	evidenceList, err := s.deps.Runs.Evidence(r.Context(), principal.TenantID, runID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "evidence": evidenceList})
}

func (s *Server) handleCounterexamples(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRoles(w, r, "viewer", "operator", "admin", "platform-admin"); !ok {
		return
	}
	principal, _ := principalFrom(r.Context())
	runID, ok := parseRunID(w, r)
	if !ok {
		return
	}
	counterexamples, err := s.deps.Runs.Counterexamples(r.Context(), principal.TenantID, runID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "counterexamples": counterexamples})
}

type reproductionRequest struct {
	RunID  string `json:"run_id"`
	CaseID string `json:"case_id"`
}

func (s *Server) handleReproduction(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireRoles(w, r, "operator", "admin")
	if !ok {
		return
	}
	principal, _ := principalFrom(r.Context())
	var body reproductionRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	runID, err := domain.ParseRunID(body.RunID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if strings.TrimSpace(body.CaseID) == "" {
		writeProblem(w, r, http.StatusBadRequest, "INVALID_CASE", "case_id is required")
		return
	}
	run, err := s.deps.Runs.Get(r.Context(), principal.TenantID, runID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	counterexamples, err := s.deps.Runs.Counterexamples(r.Context(), principal.TenantID, runID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var found *domain.Counterexample
	for i := range counterexamples {
		if counterexamples[i].CaseID == body.CaseID {
			found = &counterexamples[i]
			break
		}
	}
	if found == nil {
		writeProblem(w, r, http.StatusNotFound, "CASE_NOT_FOUND", "counterexample is not part of the run")
		return
	}
	if !found.Reproduced {
		writeProblem(w, r, http.StatusUnprocessableEntity, "CASE_NOT_REPRODUCED", "counterexample was not reproduced and cannot be replayed")
		return
	}
	if found.ClaimID == "native-fuzz" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "CASE_NOT_REPLAYABLE", "native fuzz counterexamples do not support controlled replay")
		return
	}
	reproduction, err := s.deps.Runs.Submit(r.Context(), runservice.SubmitInput{
		TenantID: run.TenantID, ProjectID: run.ProjectID,
		RequestKey: "repro-" + run.ID.String() + "-" + body.CaseID,
		BaseCommit: run.BaseCommit, HeadCommit: run.HeadCommit, PatchDigest: run.PatchDigest,
		PolicyDigest: run.PolicyDigest, RequestedChecks: []string{"differential-fuzz"},
		Budget: run.Budget, Region: run.Region, CreatedBy: actor,
		Replay: &domain.ReplaySpec{
			SourceRunID: run.ID, CaseID: found.CaseID, ClaimID: found.ClaimID,
			InputRef: found.InputRef, InputCodec: found.InputCodec,
		},
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Location", s.runURL(reproduction.ID))
	writeJSON(w, http.StatusAccepted, runResponse{Run: reproduction, StatusURL: s.runURL(reproduction.ID), ReportURL: s.runURL(reproduction.ID) + "/report"})
}

type projectRequest struct {
	Provider            string   `json:"provider"`
	RepositoryURI       string   `json:"repository_uri"`
	DefaultBranch       string   `json:"default_branch"`
	DefaultPolicyDigest string   `json:"default_policy_digest"`
	AllowedRegions      []string `json:"allowed_regions"`
	Classification      string   `json:"classification"`
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireRoles(w, r, "admin", "platform-admin")
	if !ok {
		return
	}
	principal, _ := principalFrom(r.Context())
	var body projectRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	project, err := s.deps.Admin.CreateProject(r.Context(), principal.TenantID, actor, admin.ProjectInput{
		Provider: body.Provider, RepositoryURI: body.RepositoryURI, DefaultBranch: body.DefaultBranch,
		DefaultPolicyDigest: body.DefaultPolicyDigest, AllowedRegions: body.AllowedRegions, Classification: body.Classification,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, project)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRoles(w, r, "viewer", "operator", "admin", "platform-admin"); !ok {
		return
	}
	principal, _ := principalFrom(r.Context())
	projectID, err := domain.ParseProjectID(r.PathValue("project_id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	project, err := s.deps.Admin.GetProject(r.Context(), principal.TenantID, projectID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

type policyRequest struct {
	ProjectID string `json:"project_id"`
	Document  string `json:"document"`
}

func (s *Server) handlePutPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireRoles(w, r, "admin", "platform-admin")
	if !ok {
		return
	}
	principal, _ := principalFrom(r.Context())
	var body policyRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	var projectID domain.ProjectID
	if body.ProjectID != "" {
		parsed, err := domain.ParseProjectID(body.ProjectID)
		if err != nil {
			writeError(w, r, err)
			return
		}
		projectID = parsed
	}
	policy, err := s.deps.Admin.PutPolicy(r.Context(), principal.TenantID, projectID, actor, []byte(body.Document))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"digest": policy.Digest, "version": policy.Version, "created_at": policy.CreatedAt})
}

func (s *Server) handleValidatePolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRoles(w, r, "viewer", "operator", "admin", "platform-admin"); !ok {
		return
	}
	var body policyRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	validation, err := s.deps.Admin.ValidatePolicy([]byte(body.Document))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, validation)
}

func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRoles(w, r, "admin", "platform-admin"); !ok {
		return
	}
	clusters, err := s.deps.Admin.ListClusters(r.Context(), r.URL.Query().Get("region"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": clusters})
}

type tenantRequest struct {
	HomeRegion   string `json:"home_region"`
	SecurityTier string `json:"security_tier"`
	DisplayName  string `json:"display_name"`
	Quota        struct {
		MaxConcurrentRuns  int     `json:"max_concurrent_runs"`
		MaxConcurrentSteps int     `json:"max_concurrent_steps"`
		CPUCoreSecondsDay  float64 `json:"cpu_core_seconds_per_day"`
		FuzzSecondsDay     float64 `json:"fuzz_seconds_per_day"`
		StorageBytes       int64   `json:"storage_bytes"`
		APIRPS             int     `json:"api_rps"`
	} `json:"quota"`
}

func (s *Server) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	actor, ok := requireRoles(w, r, "platform-admin")
	if !ok {
		return
	}
	var body tenantRequest
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	tenant, err := s.deps.Admin.CreateTenant(r.Context(), actor, admin.TenantInput{
		HomeRegion: body.HomeRegion, SecurityTier: domain.SecurityTier(body.SecurityTier), DisplayName: body.DisplayName,
		Quota: domain.TenantQuota{
			MaxConcurrentRuns: body.Quota.MaxConcurrentRuns, MaxConcurrentSteps: body.Quota.MaxConcurrentSteps,
			CPUCoreSecondsDay: body.Quota.CPUCoreSecondsDay, FuzzSecondsDay: body.Quota.FuzzSecondsDay,
			StorageBytes: body.Quota.StorageBytes, APIRPS: body.Quota.APIRPS,
		},
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, tenant)
}

func (s *Server) handleGetTenant(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireRoles(w, r, "admin", "platform-admin"); !ok {
		return
	}
	principal, _ := principalFrom(r.Context())
	tenantID, err := domain.ParseTenantID(r.PathValue("tenant_id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if tenantID != principal.TenantID && !hasRole(principal.Roles, "platform-admin") {
		writeProblem(w, r, http.StatusForbidden, "FORBIDDEN", "principal cannot read another tenant")
		return
	}
	tenant, err := s.deps.Admin.GetTenant(r.Context(), tenantID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tenant)
}

func (s *Server) runURL(runID domain.RunID) string {
	base := strings.TrimRight(s.deps.PublicURL, "/")
	path := "/api/v1/runs/" + runID.String()
	if base == "" {
		return path
	}
	return base + path
}
