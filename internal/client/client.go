// Package client wraps the generated API client with authentication, the
// CLI's identification headers, and errors that read well in a terminal.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/teraldev/cli/internal/api"
	"github.com/teraldev/cli/internal/version"
)

// Client calls the Teral API.
type Client struct {
	// API is the generated client. Use [Call] to invoke it.
	API *api.Client
	// BaseURL is the API endpoint.
	BaseURL string
	// Source describes the credential, for error hints: "api_key",
	// "session", or "" for anonymous calls.
	Source string
}

// New creates a client. token may be empty for the public login endpoints.
func New(baseURL, token, source string) (*Client, error) {
	httpClient := &http.Client{Timeout: 90 * time.Second}
	raw, err := api.NewClient(baseURL,
		api.WithHTTPClient(httpClient),
		api.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("User-Agent", version.UserAgent())
			req.Header.Set("X-Teral-Client", version.Client())
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			return nil
		}),
	)
	if err != nil {
		return nil, err
	}
	return &Client{API: raw, BaseURL: baseURL, Source: source}, nil
}

// Call runs one generated client method and decodes a 2xx response body
// into T. Other statuses become an [*Error].
//
//	who, err := client.Call[api.V1AuthWhoamiResponseBody](ctx, c, func(ctx context.Context, a *api.Client) (*http.Response, error) {
//		return a.AuthWhoami(ctx, api.AuthWhoamiJSONRequestBody{})
//	})
func Call[T any](ctx context.Context, c *Client, fn func(context.Context, *api.Client) (*http.Response, error)) (*T, error) {
	res, err := fn(ctx, c.API)
	if err != nil {
		return nil, networkError(c.BaseURL, err)
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, networkError(c.BaseURL, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, decodeError(res, body, c.Source)
	}
	out := new(T)
	if err := json.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("unexpected response from %s (HTTP %d): %w", c.BaseURL, res.StatusCode, err)
	}
	return out, nil
}

// Error is an API error response.
type Error struct {
	Status    int
	Title     string
	Detail    string
	Type      string
	RequestID string
	// Hint suggests what to do next.
	Hint string
}

func (e *Error) Error() string {
	msg := e.Detail
	if msg == "" {
		msg = e.Title
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return msg
}

// IsStatus reports whether err is an API error with the given status.
func IsStatus(err error, status int) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Status == status
}

func decodeError(res *http.Response, body []byte, source string) error {
	var envelope struct {
		Meta struct {
			RequestID string `json:"requestId"`
		} `json:"meta"`
		Error struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
			Type   string `json:"type"`
			Status int    `json:"status"`
			Errors []struct {
				Location string `json:"location"`
				Message  string `json:"message"`
			} `json:"errors"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)

	e := &Error{
		Status:    res.StatusCode,
		Title:     envelope.Error.Title,
		Detail:    envelope.Error.Detail,
		Type:      envelope.Error.Type,
		RequestID: envelope.Meta.RequestID,
	}
	if e.RequestID == "" {
		e.RequestID = res.Header.Get("X-Teral-Request-Id")
	}
	if len(envelope.Error.Errors) > 0 {
		var parts []string
		for _, v := range envelope.Error.Errors {
			loc := strings.TrimPrefix(v.Location, "body.")
			if loc != "" && loc != "body" {
				parts = append(parts, loc+": "+v.Message)
			} else {
				parts = append(parts, v.Message)
			}
		}
		e.Detail = strings.TrimSuffix(e.Detail, ".") + ": " + strings.Join(parts, "; ")
	}

	switch res.StatusCode {
	case http.StatusUnauthorized:
		if source == "api_key" {
			e.Hint = "Check the TERAL_API_KEY environment variable. Create a new key in the dashboard under Settings → API Keys."
		} else {
			e.Hint = "Run `teral login` to sign in."
		}
	case http.StatusForbidden:
		e.Hint = "Your role or API key doesn't allow this. Ask a workspace admin, or check the key's permissions."
	case http.StatusTooManyRequests:
		e.Hint = "Wait a moment and try again."
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		e.Hint = "This is likely on Teral's side. Try again, and contact support with the request id if it persists."
	}
	return e
}

func networkError(baseURL string, err error) error {
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.As(err, &netErr) && netErr.Timeout():
		return fmt.Errorf("the Teral API at %s did not respond in time: %w", baseURL, err)
	default:
		return fmt.Errorf("could not reach the Teral API at %s: %w", baseURL, err)
	}
}
