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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseConfig(t *testing.T) {
	t.Setenv("N8N_API_KEY", "")

	tests := []struct {
		name    string
		config  map[string]string
		env     string
		want    *Config
		wantErr string
	}{
		{
			name:   "api_key option",
			config: map[string]string{"location": "n8n://n8n.example.com", "api_key": "k"},
			want:   &Config{Host: "n8n.example.com", BaseURL: "https://n8n.example.com/api/v1", APIKey: "k", Timeout: DefaultTimeout},
		},
		{
			name:   "key in userinfo",
			config: map[string]string{"location": "n8n://k@n8n.example.com"},
			want:   &Config{Host: "n8n.example.com", BaseURL: "https://n8n.example.com/api/v1", APIKey: "k", Timeout: DefaultTimeout},
		},
		{
			name:   "env fallback",
			config: map[string]string{"location": "n8n://n8n.example.com"},
			env:    "env-key",
			want:   &Config{Host: "n8n.example.com", BaseURL: "https://n8n.example.com/api/v1", APIKey: "env-key", Timeout: DefaultTimeout},
		},
		{
			name:   "http scheme, port and prefix",
			config: map[string]string{"location": "n8n://localhost:5678/n8n/", "api_key": "k", "scheme": "http", "timeout": "5s"},
			want:   &Config{Host: "localhost:5678", BaseURL: "http://localhost:5678/n8n/api/v1", APIKey: "k", Timeout: 5e9},
		},
		{
			name:    "missing key",
			config:  map[string]string{"location": "n8n://n8n.example.com"},
			wantErr: "missing n8n API key",
		},
		{
			name:    "key twice",
			config:  map[string]string{"location": "n8n://k@n8n.example.com", "api_key": "k"},
			wantErr: "can not use <key>@host syntax and api_key parameter",
		},
		{
			name:    "wrong scheme",
			config:  map[string]string{"location": "https://n8n.example.com", "api_key": "k"},
			wantErr: "location scheme must be n8n://",
		},
		{
			name:    "no host",
			config:  map[string]string{"location": "n8n:///path", "api_key": "k"},
			wantErr: "has no host",
		},
		{
			name:    "bad scheme option",
			config:  map[string]string{"location": "n8n://h", "api_key": "k", "scheme": "ftp"},
			wantErr: "invalid scheme",
		},
		{
			name:    "bad timeout",
			config:  map[string]string{"location": "n8n://h", "api_key": "k", "timeout": "soon"},
			wantErr: "invalid timeout",
		},
		{
			name:    "missing location",
			config:  map[string]string{},
			wantErr: "missing location",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("N8N_API_KEY", tt.env)
			got, err := ParseConfig(tt.config)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.NotContains(t, got.Origin(), got.APIKey)
		})
	}
}

func TestParseBool(t *testing.T) {
	v, err := ParseBool(map[string]string{}, "x", true)
	require.NoError(t, err)
	require.True(t, v)

	v, err = ParseBool(map[string]string{"x": "no"}, "x", true)
	require.NoError(t, err)
	require.False(t, v)

	_, err = ParseBool(map[string]string{"x": "maybe"}, "x", true)
	require.ErrorContains(t, err, "invalid value for x")
}

func TestListPagination(t *testing.T) {
	pages := map[string]string{
		"":   `{"data":[{"id":"a"},{"id":"b"}],"nextCursor":"c1"}`,
		"c1": `{"data":[{"id":"c"}],"nextCursor":null}`,
	}
	var cursors, keys, limits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("X-N8N-API-KEY"))
		cursors = append(cursors, r.URL.Query().Get("cursor"))
		limits = append(limits, r.URL.Query().Get("limit"))
		io.WriteString(w, pages[r.URL.Query().Get("cursor")])
	}))
	defer srv.Close()

	c := NewClient(&Config{APIKey: "k", BaseURL: srv.URL})
	var ids []string
	err := c.list(context.Background(), "/tags", nil, func(page []json.RawMessage) error {
		objs, err := decodeObjects(page)
		require.NoError(t, err)
		for _, o := range objs {
			ids = append(ids, o.ID)
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c"}, ids)
	require.Equal(t, []string{"", "c1"}, cursors)
	require.Equal(t, []string{"k", "k"}, keys)
	require.Equal(t, []string{"250", "250"}, limits)
}

// TestRetry checks that transient failures are retried for GET but never for
// POST, whose replay could duplicate a write.
func TestRetry(t *testing.T) {
	var gets, posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		switch r.Method {
		case http.MethodGet:
			gets++
			if gets < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			io.WriteString(w, `{"data":[]}`)
		case http.MethodPost:
			posts++
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, `{"message":"upstream gone"}`)
		}
	}))
	defer srv.Close()

	c := NewClient(&Config{APIKey: "k", BaseURL: srv.URL})
	require.NoError(t, c.Ping(context.Background()))
	require.Equal(t, 3, gets)

	_, err := c.CreateTag(context.Background(), "x")
	require.ErrorContains(t, err, "HTTP 502: upstream gone")
	require.Equal(t, 1, posts)
}

func TestResponseTooLarge(t *testing.T) {
	old := maxResponse
	maxResponse = 64
	defer func() { maxResponse = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`)
	}))
	defer srv.Close()

	c := NewClient(&Config{APIKey: "k", BaseURL: srv.URL})
	require.ErrorContains(t, c.Ping(context.Background()), "answer larger than 64 bytes")
}

func TestIsUnlicensed(t *testing.T) {
	require.True(t, IsUnlicensed(&APIError{Status: 403, Message: "Your license does not allow for variables"}))
	require.False(t, IsUnlicensed(&APIError{Status: 403, Message: "Forbidden"}))
	require.False(t, IsUnlicensed(&APIError{Status: 401, Message: "license"}))
	require.False(t, IsUnlicensed(errors.New("license")))
}

func TestListRepeatedCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"a"}],"nextCursor":"same"}`)
	}))
	defer srv.Close()

	c := NewClient(&Config{APIKey: "k", BaseURL: srv.URL})
	err := c.list(context.Background(), "/tags", nil, func([]json.RawMessage) error { return nil })
	require.ErrorContains(t, err, "same cursor twice")
}

func TestAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message":"'X-N8N-API-KEY' header required"}`)
	}))
	defer srv.Close()

	c := NewClient(&Config{APIKey: "k", BaseURL: srv.URL})
	err := c.Ping(context.Background())
	require.ErrorContains(t, err, "HTTP 401: 'X-N8N-API-KEY' header required")
	require.True(t, IsStatus(err, http.StatusUnauthorized))
	require.False(t, IsStatus(err, http.StatusForbidden))
}

func TestDecodeObject(t *testing.T) {
	o, err := decodeObject(json.RawMessage(`{"id":42,"startedAt":"2026-01-02T03:04:05.678Z","workflowId":"w"}`))
	require.NoError(t, err)
	require.Equal(t, "42", o.ID)
	require.Equal(t, "2026-01-02T03:04:05.678Z", o.ModTime.UTC().Format("2006-01-02T15:04:05.000Z"))

	o, err = decodeObject(json.RawMessage(`{"id":"v1","key":"K","value":"x"}`))
	require.NoError(t, err)
	require.Equal(t, "K", o.Name)
	require.True(t, o.ModTime.IsZero())

	_, err = decodeObject(json.RawMessage(`{"name":"no id"}`))
	require.ErrorContains(t, err, "no id")
}
