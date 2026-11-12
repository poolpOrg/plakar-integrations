/*
 * Copyright (c) 2026 Gilles Chehade <gilles@poolp.org>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package common

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxErrorBody bounds how much of an error response we read back to build
// the error message.
const maxErrorBody = 8 << 10

// MaxResponse bounds how much of an answer we are willing to read: the
// instance is not ours, and a page of executions with run data is the
// largest legitimate answer.
const MaxResponse = 256 << 20

// maxResponse is MaxResponse, lowered by tests.
var maxResponse = MaxResponse

// Client is a thin REST client over the n8n public API. It is safe for
// concurrent use.
type Client struct {
	cfg  *Config
	http *http.Client
}

// NewClient builds a Client from a parsed Config.
func NewClient(cfg *Config) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.Timeout},
	}
}

// APIError is a non-2xx answer from the n8n API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("n8n api: HTTP %d", e.Status)
	}
	return fmt.Sprintf("n8n api: HTTP %d: %s", e.Status, e.Message)
}

// IsStatus reports whether err is an APIError with the given HTTP status.
func IsStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// IsUnlicensed reports whether err is n8n refusing a feature the instance's
// license does not include ("Your license does not allow for variables").
// A plain 403 is not enough: a scoped API key lacking a permission gets the
// same status, and that one must surface.
func IsUnlicensed(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden &&
		strings.Contains(strings.ToLower(apiErr.Message), "license")
}

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Request, error) {
	u := c.cfg.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-N8N-API-KEY", c.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		// Let the transport replay the body on retry.
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}
	return req, nil
}

// do executes a request. Reads (GET) and idempotent writes (PUT) are retried
// a bounded number of times on transient errors (429, 5xx, transport),
// honouring Retry-After when present. A POST is never retried: an answer
// lost after the write committed would turn a retry into a duplicate
// workflow or credential, and the caller reports the record failed instead.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	maxAttempts := 5
	if req.Method != http.MethodGet && req.Method != http.MethodPut {
		maxAttempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if req.Context().Err() != nil || maxAttempts == 1 {
				return nil, err
			}
			lastErr = err
			if !sleep(req.Context(), backoff(attempt, "")) {
				return nil, req.Context().Err()
			}
			continue
		}
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && maxAttempts > 1 {
			retryAfter := resp.Header.Get("Retry-After")
			lastErr = apiError(resp)
			resp.Body.Close()
			if !sleep(req.Context(), backoff(attempt, retryAfter)) {
				return nil, req.Context().Err()
			}
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("request %s %s failed after %d attempts: %w", req.Method, req.URL.Path, maxAttempts, lastErr)
}

// call performs a request and decodes a JSON answer into out when out is not
// nil. Any non-2xx status is returned as an *APIError.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, body []byte, out any) error {
	req, err := c.newRequest(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}
	answer, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxResponse)+1))
	if err != nil {
		return fmt.Errorf("%s %s: reading answer: %w", method, path, err)
	}
	if len(answer) > maxResponse {
		return fmt.Errorf("%s %s: answer larger than %d bytes", method, path, maxResponse)
	}
	if err := json.Unmarshal(answer, out); err != nil {
		return fmt.Errorf("%s %s: decoding answer: %w", method, path, err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.call(ctx, http.MethodGet, path, query, nil, out)
}

func (c *Client) send(ctx context.Context, method, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%s %s: encoding body: %w", method, path, err)
	}
	return c.call(ctx, method, path, nil, raw, out)
}

// Ping verifies the API key and connectivity with a cheap authenticated call.
func (c *Client) Ping(ctx context.Context) error {
	q := url.Values{}
	q.Set("limit", "1")
	var out struct {
		Data []json.RawMessage `json:"data"`
	}
	return c.get(ctx, "/workflows", q, &out)
}

// list walks a cursor-paginated endpoint, invoking fn for each page of raw
// items. n8n answers {"data": [...], "nextCursor": "..."}; the cursor is
// absent or null on the last page.
func (c *Client) list(ctx context.Context, path string, extra url.Values, fn func([]json.RawMessage) error) error {
	cursor := ""
	for {
		q := url.Values{}
		for k, vs := range extra {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		q.Set("limit", "250")
		if cursor != "" {
			q.Set("cursor", cursor)
		}

		var page struct {
			Data       []json.RawMessage `json:"data"`
			NextCursor *string           `json:"nextCursor"`
		}
		if err := c.get(ctx, path, q, &page); err != nil {
			return err
		}
		if err := fn(page.Data); err != nil {
			return err
		}
		if page.NextCursor == nil || *page.NextCursor == "" || len(page.Data) == 0 {
			return nil
		}
		if *page.NextCursor == cursor {
			return fmt.Errorf("list %s: server returned the same cursor twice", path)
		}
		cursor = *page.NextCursor
	}
}

// apiError turns a non-2xx response into an *APIError, extracting the n8n
// {"message": ...} envelope when present.
func apiError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var env struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &env) == nil && env.Message != "" {
		return &APIError{Status: resp.StatusCode, Message: env.Message}
	}
	return &APIError{Status: resp.StatusCode, Message: string(bytes.TrimSpace(body))}
}

// backoff returns the delay before the next retry attempt. A server-provided
// Retry-After (seconds) takes precedence over exponential backoff.
func backoff(attempt int, retryAfter string) time.Duration {
	if retryAfter != "" {
		if secs, err := strconv.Atoi(retryAfter); err == nil && secs >= 0 {
			return min(time.Duration(secs)*time.Second, 30*time.Second)
		}
	}
	return min(time.Duration(1<<attempt)*time.Second, 30*time.Second)
}

// sleep waits for d or until ctx is cancelled. It returns false if the wait
// was cut short by cancellation.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
