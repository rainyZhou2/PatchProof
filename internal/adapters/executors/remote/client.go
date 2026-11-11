// Package remote implements the controller-side executor client for an
// execution cluster agent. The wire contract is JSON over HTTP(S):
//
//	GET    /v1/capabilities                 -> ports.ExecutorCapabilities
//	POST   /v1/executions                   -> ports.ExecutionRef
//	GET    /v1/executions/{external_id}     -> ports.ExecutionObservation
//	DELETE /v1/executions/{external_id}     -> 204
//
// GET and DELETE accept the optional query parameters "executor" and
// "namespace", which are echoed into the execution reference. Every request
// carries the configured bearer token; 404 means the execution is lost, 401
// and 403 indicate credential problems, and 5xx/429 are retryable.
package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
)

const (
	defaultTimeout   = 30 * time.Second
	maxResponseBytes = 4 << 20
	executorKind     = "remote"
)

var (
	// ErrNotFound is returned when the agent does not know an execution.
	ErrNotFound = errors.New("remote executor: execution not found")
	// ErrUnauthorized is returned when the agent rejects the bearer token.
	ErrUnauthorized = errors.New("remote executor: agent rejected the bearer token")
	// ErrForbidden is returned when the agent denies the caller.
	ErrForbidden = errors.New("remote executor: agent denied the request")
)

// Config configures the remote executor client.
type Config struct {
	// Endpoint is the absolute HTTP(S) base URL of the cluster agent.
	Endpoint string
	// Token authenticates the controller to the agent.
	Token string
	// ClusterID is attached to execution references when the agent omits it.
	ClusterID domain.ClusterID
	// Timeout bounds each request and defaults to 30 seconds.
	Timeout time.Duration
	// TLSConfig overrides the TLS client configuration for HTTPS endpoints.
	TLSConfig *tls.Config
}

// RetryableError reports a transient transport or server failure.
type RetryableError struct {
	StatusCode int
	Detail     string
	Err        error
}

func (e *RetryableError) Error() string {
	if e.StatusCode == 0 {
		return "remote executor: transient failure: " + e.Detail
	}
	return fmt.Sprintf("remote executor: HTTP %d: %s", e.StatusCode, e.Detail)
}

// Unwrap exposes the underlying transport error.
func (e *RetryableError) Unwrap() error { return e.Err }

// Retryable marks the failure as safe to retry.
func (e *RetryableError) Retryable() bool { return true }

var _ ports.Executor = (*Executor)(nil)

// Executor delegates execution to a cluster agent over HTTP.
type Executor struct {
	endpoint  string
	token     string
	clusterID domain.ClusterID
	timeout   time.Duration
	client    *http.Client
}

// New validates cfg and returns a remote executor client.
func New(cfg Config) (*Executor, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	if endpoint == "" {
		return nil, errors.New("remote executor: endpoint is required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("remote executor: parse endpoint: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("remote executor: endpoint must use http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, errors.New("remote executor: endpoint must include a host")
	}
	if cfg.Token == "" {
		return nil, errors.New("remote executor: bearer token is required")
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("remote executor: timeout must not be negative")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.TLSConfig != nil && parsed.Scheme != "https" {
		return nil, errors.New("remote executor: TLS configuration requires an https endpoint")
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       cfg.TLSConfig,
	}
	return &Executor{
		endpoint:  endpoint,
		token:     cfg.Token,
		clusterID: cfg.ClusterID,
		timeout:   cfg.Timeout,
		client:    &http.Client{Transport: transport, Timeout: cfg.Timeout},
	}, nil
}

// Capabilities fetches the agent's advertised capabilities.
func (e *Executor) Capabilities(ctx context.Context) (ports.ExecutorCapabilities, error) {
	var capabilities ports.ExecutorCapabilities
	if err := e.do(ctx, http.MethodGet, "/v1/capabilities", nil, &capabilities); err != nil {
		return ports.ExecutorCapabilities{}, err
	}
	return capabilities, nil
}

// Start submits an execution to the agent.
func (e *Executor) Start(ctx context.Context, request ports.ExecutionRequest) (ports.ExecutionRef, error) {
	payload := executionRequest{
		Spec:         request.Spec,
		AttemptToken: request.AttemptToken,
		CallbackURL:  request.CallbackURL,
		WorkspaceRef: request.WorkspaceRef,
	}
	var ref ports.ExecutionRef
	if err := e.do(ctx, http.MethodPost, "/v1/executions", payload, &ref); err != nil {
		return ports.ExecutionRef{}, err
	}
	if strings.TrimSpace(ref.ExternalID) == "" {
		return ports.ExecutionRef{}, errors.New("remote executor: agent returned an empty external id")
	}
	if ref.ClusterID == "" {
		ref.ClusterID = e.clusterID
	}
	if ref.Executor == "" {
		ref.Executor = executorKind
	}
	return ref, nil
}

// Observe fetches the current state of an execution. An execution unknown to
// the agent is reported as LOST.
func (e *Executor) Observe(ctx context.Context, ref ports.ExecutionRef) (ports.ExecutionObservation, error) {
	path, err := executionPath(ref)
	if err != nil {
		return ports.ExecutionObservation{}, err
	}
	var observation ports.ExecutionObservation
	if err := e.do(ctx, http.MethodGet, path, nil, &observation); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ports.ExecutionObservation{
				State:      ports.ExecutionLost,
				ErrorClass: domain.ErrWorkerLost,
				Message:    "agent does not know execution " + ref.ExternalID,
			}, nil
		}
		return ports.ExecutionObservation{}, err
	}
	if observation.State == "" {
		return ports.ExecutionObservation{}, errors.New("remote executor: agent returned an empty execution state")
	}
	return observation, nil
}

// Cancel requests cancellation. Executions the agent no longer tracks are
// treated as already cancelled.
func (e *Executor) Cancel(ctx context.Context, ref ports.ExecutionRef) error {
	path, err := executionPath(ref)
	if err != nil {
		return err
	}
	if err := e.do(ctx, http.MethodDelete, path, nil, nil); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

type executionRequest struct {
	Spec         domain.StepSpec `json:"spec"`
	AttemptToken string          `json:"attempt_token,omitempty"`
	CallbackURL  string          `json:"callback_url,omitempty"`
	WorkspaceRef domain.CASRef   `json:"workspace_ref"`
}

func executionPath(ref ports.ExecutionRef) (string, error) {
	id := strings.TrimSpace(ref.ExternalID)
	if id == "" {
		return "", errors.New("remote executor: execution reference has no external id")
	}
	if len(id) > 128 || strings.ContainsAny(id, "/?#") {
		return "", fmt.Errorf("remote executor: invalid external id %q", id)
	}
	query := url.Values{}
	if ref.Executor != "" {
		query.Set("executor", ref.Executor)
	}
	if ref.Namespace != "" {
		query.Set("namespace", ref.Namespace)
	}
	path := "/v1/executions/" + url.PathEscape(id)
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return path, nil
}

func (e *Executor) do(ctx context.Context, method, path string, body, out any) error {
	requestCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("remote executor: encode request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(requestCtx, method, e.endpoint+path, reader)
	if err != nil {
		return fmt.Errorf("remote executor: build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+e.token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Request-Id", newRequestID())
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := e.client.Do(request)
	if err != nil {
		if requestCtx.Err() != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return &RetryableError{Detail: "transport failure: " + err.Error(), Err: err}
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return &RetryableError{StatusCode: response.StatusCode, Detail: "read response: " + err.Error(), Err: err}
	}
	if int64(len(payload)) > maxResponseBytes {
		return fmt.Errorf("remote executor: response exceeds %d bytes", maxResponseBytes)
	}

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if out == nil || len(bytes.TrimSpace(payload)) == 0 {
			return nil
		}
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("remote executor: decode response: %w", err)
		}
		return nil
	}
	return statusError(response.StatusCode, payload)
}

func statusError(status int, payload []byte) error {
	detail := problemDetail(payload)
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: HTTP 401: %s", ErrUnauthorized, detail)
	case http.StatusForbidden:
		return fmt.Errorf("%w: HTTP 403: %s", ErrForbidden, detail)
	case http.StatusNotFound:
		return fmt.Errorf("%w: HTTP 404: %s", ErrNotFound, detail)
	}
	if status == http.StatusTooManyRequests || status >= 500 {
		return &RetryableError{StatusCode: status, Detail: detail}
	}
	return fmt.Errorf("remote executor: HTTP %d: %s", status, detail)
}

func problemDetail(payload []byte) string {
	var problem struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(payload, &problem); err != nil {
		return strings.TrimSpace(string(payload))
	}
	if problem.Detail != "" {
		return problem.Detail
	}
	if problem.Title != "" {
		return problem.Title
	}
	return "request failed"
}

func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}
