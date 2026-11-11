package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
)

type Config struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	UserAgent  string
}

type Client struct {
	baseURL   string
	token     string
	client    *http.Client
	userAgent string
}

type ProblemError struct {
	StatusCode int
	Code       string
	Title      string
	Detail     string
}

func (e *ProblemError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("api error %d (%s): %s", e.StatusCode, e.Code, e.Detail)
	}
	return fmt.Sprintf("api error %d: %s", e.StatusCode, e.Detail)
}

func New(config Config) (*Client, error) {
	if config.BaseURL == "" {
		return nil, fmt.Errorf("client: base url is required")
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("client: base url must be absolute")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if config.UserAgent == "" {
		config.UserAgent = "patchproof-cli/dev"
	}
	return &Client{
		baseURL: strings.TrimRight(config.BaseURL, "/"), token: config.Token,
		client: config.HTTPClient, userAgent: config.UserAgent,
	}, nil
}

type SubmitRunRequest struct {
	ProjectID       string         `json:"project_id"`
	BaseCommit      string         `json:"base_commit"`
	HeadCommit      string         `json:"head_commit"`
	PatchDigest     string         `json:"patch_digest,omitempty"`
	PolicyRef       string         `json:"policy_ref,omitempty"`
	PolicyDigest    string         `json:"policy_digest,omitempty"`
	RequestedChecks []string       `json:"requested_checks,omitempty"`
	Budget          *domain.Budget `json:"budget,omitempty"`
	Region          string         `json:"region,omitempty"`
}

type RunEnvelope struct {
	Run       domain.Run `json:"run"`
	StatusURL string     `json:"status_url"`
	ReportURL string     `json:"report_url"`
}

func (c *Client) SubmitRun(ctx context.Context, requestKey string, request SubmitRunRequest) (RunEnvelope, error) {
	var envelope RunEnvelope
	err := c.do(ctx, http.MethodPost, "/api/v1/runs", nil, request, &envelope, map[string]string{"Idempotency-Key": requestKey})
	return envelope, err
}

func (c *Client) GetRun(ctx context.Context, runID string) (RunEnvelope, error) {
	var envelope RunEnvelope
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID), nil, nil, &envelope, nil)
	return envelope, err
}

type RunList struct {
	Runs       []domain.Run `json:"runs"`
	NextCursor string       `json:"next_cursor"`
}

func (c *Client) ListRuns(ctx context.Context, query url.Values) (RunList, error) {
	var list RunList
	err := c.do(ctx, http.MethodGet, "/api/v1/runs?"+query.Encode(), nil, nil, &list, nil)
	return list, err
}

type StepsResponse struct {
	RunID        domain.RunID                      `json:"run_id"`
	Steps        []domain.Step                     `json:"steps"`
	Dependencies map[domain.StepID][]domain.StepID `json:"dependencies"`
	Claims       []domain.Claim                    `json:"claims"`
}

func (c *Client) Steps(ctx context.Context, runID string) (StepsResponse, error) {
	var response StepsResponse
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID)+"/steps", nil, nil, &response, nil)
	return response, err
}

func (c *Client) CancelRun(ctx context.Context, runID string) (RunEnvelope, error) {
	var envelope RunEnvelope
	err := c.do(ctx, http.MethodPost, "/api/v1/runs/"+url.PathEscape(runID)+":cancel", nil, nil, &envelope, nil)
	return envelope, err
}

func (c *Client) Report(ctx context.Context, runID, format string) ([]byte, error) {
	return c.raw(ctx, "/api/v1/runs/"+url.PathEscape(runID)+"/report?format="+url.QueryEscape(format))
}

func (c *Client) Evidence(ctx context.Context, runID string) ([]domain.Evidence, error) {
	var response struct {
		Evidence []domain.Evidence `json:"evidence"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID)+"/evidence", nil, nil, &response, nil)
	return response.Evidence, err
}

func (c *Client) Counterexamples(ctx context.Context, runID string) ([]domain.Counterexample, error) {
	var response struct {
		Counterexamples []domain.Counterexample `json:"counterexamples"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(runID)+"/counterexamples", nil, nil, &response, nil)
	return response.Counterexamples, err
}

func (c *Client) CreateProject(ctx context.Context, request map[string]any) (domain.Project, error) {
	var project domain.Project
	err := c.do(ctx, http.MethodPost, "/api/v1/projects", nil, request, &project, nil)
	return project, err
}

func (c *Client) PutPolicy(ctx context.Context, projectID string, document []byte) (map[string]any, error) {
	var response map[string]any
	err := c.do(ctx, http.MethodPost, "/api/v1/policies", nil, map[string]any{"project_id": projectID, "document": string(document)}, &response, nil)
	return response, err
}

func (c *Client) ValidatePolicy(ctx context.Context, document []byte) (map[string]any, error) {
	var response map[string]any
	err := c.do(ctx, http.MethodPost, "/api/v1/policies:validate", nil, map[string]any{"document": string(document)}, &response, nil)
	return response, err
}

func (c *Client) Clusters(ctx context.Context) ([]domain.Cluster, error) {
	var response struct {
		Clusters []domain.Cluster `json:"clusters"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/clusters", nil, nil, &response, nil)
	return response.Clusters, err
}

func (c *Client) Reproduce(ctx context.Context, runID, caseID string) (RunEnvelope, error) {
	var envelope RunEnvelope
	err := c.do(ctx, http.MethodPost, "/api/v1/reproductions", nil, map[string]any{"run_id": runID, "case_id": caseID}, &envelope, nil)
	return envelope, err
}

func (c *Client) Health(ctx context.Context) error {
	_, err := c.raw(ctx, "/healthz")
	return err
}

func (c *Client) raw(ctx context.Context, path string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("User-Agent", c.userAgent)
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, decodeProblem(response.StatusCode, body)
	}
	return body, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, payload any, out any, headers map[string]string) error {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeProblem(response.StatusCode, data)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func decodeProblem(status int, body []byte) error {
	var document struct {
		Code   string `json:"code"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(body, &document)
	if document.Detail == "" {
		document.Detail = truncate(string(body), 512)
	}
	return &ProblemError{StatusCode: status, Code: document.Code, Title: document.Title, Detail: document.Detail}
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
