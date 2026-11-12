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

package hass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

// maxErrorBody bounds how much of an error response we keep for the
// message, the server is not ours.
const maxErrorBody = 4096

// Client is an authenticated HTTP client for one Home Assistant instance.
type Client struct {
	loc   Location
	token string
	http  *http.Client
}

// NewClient returns a client for loc using a long-lived access token.
func NewClient(loc Location, token string) *Client {
	return &Client{loc: loc, token: token, http: http.DefaultClient}
}

// NewClientWithHTTP is NewClient with a custom HTTP client, for tests
// and for callers that need their own transport.
func NewClientWithHTTP(loc Location, token string, hc *http.Client) *Client {
	c := NewClient(loc, token)
	c.http = hc
	return c
}

// Location returns the instance the client talks to.
func (c *Client) Location() Location { return c.loc }

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.loc.URL(path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return req, nil
}

// httpError turns a non-2xx response into an error carrying the status
// and the beginning of the body.
func httpError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return fmt.Errorf("%s", resp.Status)
	}
	return fmt.Errorf("%s: %s", resp.Status, msg)
}

// Ping checks that the instance is reachable and the token accepted.
func (c *Client) Ping(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", c.loc, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %w", c.loc, httpError(resp))
	}
	return nil
}

// Download streams a backup held by agentID.  With a password the
// instance decrypts a protected backup on the fly, the returned size
// is then unknown (-1).
func (c *Client) Download(ctx context.Context, backupID, agentID, password string) (io.ReadCloser, int64, error) {
	q := url.Values{"agent_id": {agentID}}
	if password != "" {
		q.Set("password", password)
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/api/backup/download/"+url.PathEscape(backupID)+"?"+q.Encode(), nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("download %s: %w", backupID, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, 0, fmt.Errorf("download %s: %w", backupID, httpError(resp))
	}
	return resp.Body, resp.ContentLength, nil
}

// Upload sends a backup tar to agentID and returns the backup id the
// instance assigned to it.  The body is streamed, so the caller may
// hand over a pipe.
func (c *Client) Upload(ctx context.Context, agentID, filename string, r io.Reader) (string, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, r); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.CloseWithError(mw.Close())
	}()

	q := url.Values{"agent_id": {agentID}}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/backup/upload?"+q.Encode(), pr)
	if err != nil {
		pr.CloseWithError(err)
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	// make sure the producer goroutine is released whatever happened
	pr.CloseWithError(errors.New("upload aborted"))
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upload: %w", httpError(resp))
	}

	var result struct {
		BackupID string `json:"backup_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&result); err != nil {
		return "", fmt.Errorf("upload: unexpected response: %w", err)
	}
	if result.BackupID == "" {
		return "", errors.New("upload: the instance returned no backup id")
	}
	return result.BackupID, nil
}
