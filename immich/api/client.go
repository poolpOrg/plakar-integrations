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

// Package api is the part of the Immich HTTP API that the importer and the
// exporter use, over a client that authenticates with an API key.
package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// maxJSONBody bounds every JSON response we decode.  A search page of
	// a thousand assets with EXIF is a few megabytes.
	maxJSONBody = 64 << 20

	// maxErrorBody is how much of an error response we keep for the message.
	maxErrorBody = 4 << 10

	// MaxSidecarSize bounds a sidecar download; XMP files are kilobytes.
	MaxSidecarSize = 64 << 20
)

// Config is what it takes to talk to one Immich server as one user.
type Config struct {
	// Location is immich://host[:port][/prefix].
	Location string

	// APIKey authenticates every request.
	APIKey string

	// Insecure skips the verification of the server certificate.
	Insecure bool

	// Plaintext talks HTTP instead of HTTPS.  Loopback hosts default to
	// plaintext, everything else to TLS.
	Plaintext *bool
}

// Client talks to one Immich server with one API key, and with the
// maintenance cookie once MaintenanceLogin has set it.
type Client struct {
	base   *url.URL
	key    string
	hc     *http.Client
	cookie *http.Cookie
}

// Error is a non-2xx answer from the server.
type Error struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Message)
}

// IsStatus reports whether err is a server answer with that status.
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

// ParseLocation turns immich://host[:port][/prefix] (or immich+dump://) into
// the base URL of the API, http://host:port/prefix/api or https://...,
// following Config.
func ParseLocation(location string, plaintext *bool) (*url.URL, error) {
	u, err := url.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("invalid location %q: %w", location, err)
	}
	if u.Scheme != "immich" && u.Scheme != "immich+dump" {
		return nil, fmt.Errorf("invalid location %q: scheme must be immich:// or immich+dump://", location)
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid location %q: missing host", location)
	}
	if u.User != nil {
		return nil, fmt.Errorf("invalid location %q: credentials do not belong in the location, use PLAKAR_IMMICH_API_KEY", location)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid location %q: unexpected query or fragment", location)
	}

	plain := isLoopback(u.Hostname())
	if plaintext != nil {
		plain = *plaintext
	}

	base := &url.URL{Scheme: "https", Host: u.Host}
	if plain {
		base.Scheme = "http"
	}
	base.Path = strings.TrimSuffix(u.Path, "/") + "/api"
	return base, nil
}

func isLoopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// New builds a client; it does not talk to the server.
func New(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("missing API key, set PLAKAR_IMMICH_API_KEY")
	}

	base, err := ParseLocation(cfg.Location, cfg.Plaintext)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   16,
	}
	if cfg.Insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in, for self-signed servers
	}

	host := base.Host
	hc := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Host != host {
				return fmt.Errorf("refusing redirect to another host %q", req.URL.Host)
			}
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}

	return &Client{base: base, key: cfg.APIKey, hc: hc}, nil
}

// Host is the host[:port] part of the server address.
func (c *Client) Host() string { return c.base.Host }

// BaseURL is the API root, for the manifest.
func (c *Client) BaseURL() string { return c.base.String() }

func (c *Client) url(p string, q url.Values) string {
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.TrimPrefix(p, "/")
	if q != nil {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// do sends one request and returns the response when its status is 2xx; any
// other status is returned as *Error with the body drained.
func (c *Client) do(ctx context.Context, method, p string, q url.Values, body io.Reader, contentType string, extra http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url(p, q), body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, p, err)
	}
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("Accept", "application/json")
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, p, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer closeQuietly(resp.Body)
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, &Error{Method: method, Path: p, Status: resp.StatusCode, Message: errorMessage(msg)}
	}
	return resp, nil
}

// errorMessage pulls the message out of Immich's JSON error envelope, or
// returns the trimmed body when it is not one.
func errorMessage(body []byte) string {
	var env struct {
		Message any `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Message != nil {
		switch m := env.Message.(type) {
		case string:
			return m
		case []any:
			parts := make([]string, 0, len(m))
			for _, p := range m {
				parts = append(parts, fmt.Sprint(p))
			}
			return strings.Join(parts, "; ")
		}
	}
	return strings.TrimSpace(string(body))
}

// doJSON sends in as JSON and returns the response for the caller to read.
func (c *Client) doJSON(ctx context.Context, method, p string, in any) (*http.Response, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("%s %s: encode request: %w", method, p, err)
	}
	return c.do(ctx, method, p, nil, bytes.NewReader(b), "application/json", nil)
}

// call sends in as JSON when it is not nil and decodes the answer into out
// when out is not nil.
func (c *Client) call(ctx context.Context, method, p string, q url.Values, in, out any) error {
	var (
		resp *http.Response
		err  error
	)
	if in != nil {
		resp, err = c.doJSON(ctx, method, p, in)
	} else {
		resp, err = c.do(ctx, method, p, q, nil, "", nil)
	}
	if err != nil {
		return err
	}
	defer closeQuietly(resp.Body)

	return decode(resp, method, p, out)
}

// closeQuietly closes a body we only read from; its close error carries
// nothing the read did not already report.
func closeQuietly(c io.Closer) {
	_ = c.Close()
}

func decode(resp *http.Response, method, p string, out any) error {
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSONBody))
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody+1))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, p, err)
	}
	if len(b) > maxJSONBody {
		return fmt.Errorf("%s %s: response larger than %d bytes", method, p, maxJSONBody)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, p, err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, p string, q url.Values, out any) error {
	return c.call(ctx, http.MethodGet, p, q, nil, out)
}

func (c *Client) post(ctx context.Context, p string, in, out any) error {
	return c.call(ctx, http.MethodPost, p, nil, in, out)
}

func (c *Client) put(ctx context.Context, p string, in, out any) error {
	return c.call(ctx, http.MethodPut, p, nil, in, out)
}

func (c *Client) patch(ctx context.Context, p string, in, out any) error {
	return c.call(ctx, http.MethodPatch, p, nil, in, out)
}

// Download streams a binary endpoint.  The reader is bounded: by the
// Content-Length when the server sends one, else by limit, and it fails with
// an error rather than returning more than that.
func (c *Client) Download(ctx context.Context, p string, limit int64) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, p, nil, nil, "", http.Header{"Accept": {"application/octet-stream"}})
	if err != nil {
		return nil, err
	}

	bound := limit
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n >= 0 {
			if limit > 0 && n > limit {
				closeQuietly(resp.Body)
				return nil, fmt.Errorf("GET %s: body of %d bytes exceeds the limit of %d", p, n, limit)
			}
			bound = n
		}
	}
	if bound <= 0 {
		return resp.Body, nil
	}
	return &boundedBody{rc: resp.Body, remaining: bound, p: p}, nil
}

// boundedBody fails once a body grows past what was announced or allowed.
type boundedBody struct {
	rc        io.ReadCloser
	remaining int64
	p         string
}

func (b *boundedBody) Read(buf []byte) (int, error) {
	if b.remaining <= 0 {
		// Peek one byte: anything past the bound is an error, not EOF.
		var one [1]byte
		n, err := b.rc.Read(one[:])
		if n > 0 {
			return 0, fmt.Errorf("GET %s: body longer than announced", b.p)
		}
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	if int64(len(buf)) > b.remaining {
		buf = buf[:b.remaining]
	}
	n, err := b.rc.Read(buf)
	b.remaining -= int64(n)
	return n, err
}

func (b *boundedBody) Close() error { return b.rc.Close() }
