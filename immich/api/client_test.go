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

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLocation(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name      string
		location  string
		plaintext *bool
		want      string
		wantErr   string
	}{
		{name: "loopback is http", location: "immich://127.0.0.1:2283", want: "http://127.0.0.1:2283/api"},
		{name: "localhost is http", location: "immich://localhost", want: "http://localhost/api"},
		{name: "host is https", location: "immich://photos.example.org", want: "https://photos.example.org/api"},
		{name: "prefix kept", location: "immich://photos.example.org/immich/", want: "https://photos.example.org/immich/api"},
		{name: "plaintext forced", location: "immich://photos.example.org", plaintext: &yes, want: "http://photos.example.org/api"},
		{name: "tls forced on loopback", location: "immich://127.0.0.1", plaintext: &no, want: "https://127.0.0.1/api"},
		{name: "wrong scheme", location: "https://photos.example.org", wantErr: "scheme must be immich://"},
		{name: "no host", location: "immich:///x", wantErr: "missing host"},
		{name: "credentials", location: "immich://key@photos.example.org", wantErr: "credentials"},
		{name: "query", location: "immich://photos.example.org?x=1", wantErr: "query"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseLocation(tc.location, tc.plaintext)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got.String())
		})
	}
}

func testClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Config{Location: "immich://" + strings.TrimPrefix(srv.URL, "http://"), APIKey: "k"})
	require.NoError(t, err)
	return c
}

func TestClientErrorsAndHeaders(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "k", r.Header.Get("X-Api-Key"))
		switch r.URL.Path {
		case "/api/users/me":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"u1","email":"a@b","name":"A"}`)
		case "/api/albums":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"Invalid API key","error":"Unauthorized","statusCode":401}`)
		case "/api/stacks":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":["assetIds must be an array","x"],"error":"Bad Request","statusCode":400}`)
		default:
			http.NotFound(w, r)
		}
	}))

	me, err := c.Me(context.Background())
	require.NoError(t, err)
	require.Equal(t, "u1", me.ID)

	_, err = c.Albums(context.Background())
	require.ErrorContains(t, err, "HTTP 401: Invalid API key")
	require.True(t, IsStatus(err, 401))

	_, err = c.Stacks(context.Background())
	require.ErrorContains(t, err, "assetIds must be an array; x")

	_, err = c.Tags(context.Background())
	require.True(t, IsStatus(err, 404))
}

func TestClientRefusesForeignRedirect(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://elsewhere.invalid/api/users/me", http.StatusFound)
	}))
	_, err := c.Me(context.Background())
	require.ErrorContains(t, err, "refusing redirect")
}

func TestDownloadBounds(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/short":
			w.Header().Set("Content-Length", "5")
			_, _ = io.WriteString(w, "hello")
		case "/api/toolong":
			w.Header().Set("Content-Length", "10")
			_, _ = io.WriteString(w, "0123456789")
		default:
			http.NotFound(w, r)
		}
	}))

	rc, err := c.Download(context.Background(), "/short", 0)
	require.NoError(t, err)
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, "hello", string(b))
	require.NoError(t, rc.Close())

	_, err = c.Download(context.Background(), "/toolong", 5)
	require.ErrorContains(t, err, "exceeds the limit")
}

func TestSearchPaginates(t *testing.T) {
	var pages []int
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/search/metadata", r.URL.Path)
		var req SearchRequest
		if !assert.NoError(t, decode(&http.Response{Body: r.Body}, "POST", r.URL.Path, &req)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		pages = append(pages, req.Page)
		assert.True(t, req.WithExif)
		switch req.Page {
		case 1:
			_, _ = io.WriteString(w, `{"assets":{"total":3,"count":2,"items":[{"id":"a"},{"id":"b"}],"nextPage":"2"}}`)
		case 2:
			_, _ = io.WriteString(w, `{"assets":{"total":3,"count":1,"items":[{"id":"c"}],"nextPage":null}}`)
		default:
			t.Fatalf("unexpected page %d", req.Page)
		}
	}))

	var ids []string
	err := c.Search(context.Background(), SearchRequest{WithExif: true}, func(a *Asset) error {
		ids = append(ids, a.ID)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c"}, ids)
	require.Equal(t, []int{1, 2}, pages)
}

func TestUploadAssetMultipart(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/assets", r.URL.Path)
		assert.Equal(t, "abc", r.Header.Get("X-Immich-Checksum"))
		if !assert.NoError(t, r.ParseMultipartForm(1<<20)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, "plakar", r.FormValue("deviceId"))
		assert.Equal(t, "true", r.FormValue("isFavorite"))
		assert.Equal(t, "archive", r.FormValue("visibility"))
		assert.Equal(t, "vid-1", r.FormValue("livePhotoVideoId"))
		assert.Empty(t, r.FormValue("duration"))

		f, hdr, err := r.FormFile("assetData")
		if assert.NoError(t, err) {
			assert.Equal(t, "IMG_0001.jpg", hdr.Filename)
			b, _ := io.ReadAll(f)
			assert.Equal(t, "original bytes", string(b))
		}

		s, shdr, err := r.FormFile("sidecarData")
		if assert.NoError(t, err) {
			assert.Equal(t, "IMG_0001.jpg.xmp", shdr.Filename)
			b, _ := io.ReadAll(s)
			assert.Equal(t, "<xmp/>", string(b))
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"new-1","status":"created"}`)
	}))

	res, err := c.UploadAsset(context.Background(), Upload{
		Filename:         "IMG_0001.jpg",
		Open:             func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("original bytes")), nil },
		OpenSidecar:      func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("<xmp/>")), nil },
		ChecksumHex:      "abc",
		DeviceAssetID:    "abc",
		DeviceID:         "plakar",
		FileCreatedAt:    "2024-07-09T13:00:00.000Z",
		FileModifiedAt:   "2024-07-01T15:00:00.000Z",
		IsFavorite:       true,
		Visibility:       "archive",
		LivePhotoVideoID: "vid-1",
	})
	require.NoError(t, err)
	require.Equal(t, "new-1", res.ID)
	require.Equal(t, "created", res.Status)
}

func TestDurationJSON(t *testing.T) {
	tests := []struct {
		in   string
		want Duration
		out  string
	}{
		{in: `3000`, want: "3000", out: `3000`},
		{in: `"00:00:02.000"`, want: "00:00:02.000", out: `"00:00:02.000"`},
		{in: `null`, want: "", out: `null`},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			var a Asset
			require.NoError(t, json.Unmarshal([]byte(`{"duration":`+tc.in+`}`), &a))
			require.Equal(t, tc.want, a.Duration)
			b, err := json.Marshal(a.Duration)
			require.NoError(t, err)
			require.Equal(t, tc.out, string(b))
		})
	}
	var a Asset
	require.Error(t, json.Unmarshal([]byte(`{"duration":[1]}`), &a))
}

func TestUploadAssetOpenFailure(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"x","status":"created"}`)
	}))
	_, err := c.UploadAsset(context.Background(), Upload{
		Filename: "f",
		Open:     func() (io.ReadCloser, error) { return nil, io.ErrUnexpectedEOF },
	})
	require.Error(t, err)
}
