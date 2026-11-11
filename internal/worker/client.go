package worker

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
	"github.com/patchproof/patchproof/internal/platform/retry"
)

type ClientConfig struct {
	BaseURL    string
	Timeout    time.Duration
	MaxRetries int
	HTTPClient *http.Client
}

type Client struct {
	baseURL string
	client  *http.Client
	policy  retry.Policy
}

func NewClient(config ClientConfig) (*Client, error) {
	if strings.TrimSpace(config.BaseURL) == "" {
		return nil, fmt.Errorf("worker client: base URL is required")
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("worker client: base URL must be absolute")
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: config.Timeout}
	}
	attempts := config.MaxRetries
	if attempts <= 0 {
		attempts = 4
	}
	return &Client{
		baseURL: strings.TrimRight(config.BaseURL, "/"),
		client:  config.HTTPClient,
		policy: retry.Policy{
			MaxAttempts: attempts,
			BaseDelay:   500 * time.Millisecond,
			MaxDelay:    10 * time.Second,
			Jitter:      0.2,
			IsRetryable: func(err error) bool {
				var status *statusError
				if ok := asStatusError(err, &status); ok {
					return status.code >= 500 || status.code == http.StatusTooManyRequests
				}
				return true
			},
		},
	}, nil
}

func (c *Client) Start(ctx context.Context, ref domain.AttemptRef, token string) error {
	body := map[string]any{"generation": ref.Generation, "started_at": time.Now().UTC()}
	return c.post(ctx, fmt.Sprintf("/internal/v1/attempts/%s:start", ref.AttemptID), token, body, nil)
}

func (c *Client) Heartbeat(ctx context.Context, ref domain.AttemptRef, token string) error {
	body := map[string]any{"generation": ref.Generation}
	return c.post(ctx, fmt.Sprintf("/internal/v1/attempts/%s:heartbeat", ref.AttemptID), token, body, nil)
}

func (c *Client) Complete(ctx context.Context, ref domain.AttemptRef, token string, result domain.StepResult) error {
	if err := result.Validate(); err != nil {
		return fmt.Errorf("worker client: invalid result: %w", err)
	}
	body := struct {
		Generation int64             `json:"generation"`
		Result     domain.StepResult `json:"result"`
	}{Generation: ref.Generation, Result: result}
	return c.post(ctx, fmt.Sprintf("/internal/v1/attempts/%s:complete", ref.AttemptID), token, body, nil)
}

func (c *Client) Fail(ctx context.Context, ref domain.AttemptRef, token string, class domain.ErrorClass, message string, retryable bool) error {
	if !class.Valid() {
		class = domain.ErrExecutorError
	}
	body := map[string]any{
		"generation":  ref.Generation,
		"error_class": class,
		"message":     truncate(message, 8192),
		"retryable":   retryable,
	}
	return c.post(ctx, fmt.Sprintf("/internal/v1/attempts/%s:fail", ref.AttemptID), token, body, nil)
}

func (c *Client) post(ctx context.Context, path, token string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	endpoint := c.baseURL + path
	return retry.Do(ctx, c.policy, func(ctx context.Context) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := c.client.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		limited := io.LimitReader(response.Body, 1<<20)
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if out != nil {
				return json.NewDecoder(limited).Decode(out)
			}
			_, _ = io.Copy(io.Discard, limited)
			return nil
		}
		detail, _ := io.ReadAll(limited)
		return &statusError{code: response.StatusCode, body: strings.TrimSpace(string(detail))}
	})
}

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("worker callback returned %d: %s", e.code, e.body)
}

func asStatusError(err error, target **statusError) bool {
	value, ok := err.(*statusError)
	if ok {
		*target = value
	}
	return ok
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
