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

package importer

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/poolpOrg/plakar-integrations/n8n/internal/n8ntest"
	"github.com/stretchr/testify/require"
)

type captured struct {
	dirs  []string
	files map[string]map[string]any
	errs  map[string]error
}

func runImport(t *testing.T, srv *n8ntest.Server, extra map[string]string) captured {
	t.Helper()

	config := map[string]string{
		"location": "n8n://" + srv.Host(),
		"api_key":  n8ntest.APIKey,
		"scheme":   "http",
	}
	for k, v := range extra {
		config[k] = v
	}
	imp, err := NewImporter(context.Background(), &integration.Options{Stderr: io.Discard}, "n8n", config)
	require.NoError(t, err)
	require.Equal(t, srv.Host(), imp.Origin())

	records := make(chan *integration.Record)
	done := make(chan error, 1)
	go func() {
		done <- imp.Import(context.Background(), records, nil)
	}()

	got := captured{files: map[string]map[string]any{}, errs: map[string]error{}}
	for rec := range records {
		switch {
		case rec.Err != nil:
			got.errs[rec.Pathname] = rec.Err
		case rec.FileInfo.IsDir():
			got.dirs = append(got.dirs, rec.Pathname)
		default:
			data, err := io.ReadAll(rec.Reader)
			require.NoError(t, err)
			require.NoError(t, rec.Close())
			require.EqualValues(t, len(data), rec.FileInfo.Size())
			var doc map[string]any
			require.NoError(t, json.Unmarshal(data, &doc), rec.Pathname)
			got.files[rec.Pathname] = doc
		}
	}
	require.NoError(t, <-done)
	require.NoError(t, imp.Close(context.Background()))
	return got
}

func seed(srv *n8ntest.Server) {
	srv.Tags["t1"] = map[string]any{"id": "t1", "name": "prod", "updatedAt": "2026-03-01T10:00:00.000Z"}
	srv.Workflows["w1"] = map[string]any{
		"id": "w1", "name": "Hello", "active": true,
		"nodes":       []any{map[string]any{"name": "Start", "type": "n8n-nodes-base.manualTrigger"}},
		"connections": map[string]any{},
		"settings":    map[string]any{"executionOrder": "v1"},
		"pinData":     map[string]any{"Start": []any{map[string]any{"json": map[string]any{"x": 1}}}},
		"tags":        []any{srv.Tags["t1"]},
		"updatedAt":   "2026-03-02T10:00:00.000Z",
	}
	srv.Workflows["w2"] = map[string]any{
		"id": "w2", "name": "Other", "active": false,
		"nodes": []any{}, "connections": map[string]any{}, "settings": map[string]any{},
	}
	srv.Workflows["w3"] = map[string]any{
		"id": "w3", "name": "Third", "active": false,
		"nodes": []any{}, "connections": map[string]any{}, "settings": map[string]any{},
	}
	srv.Variables["v1"] = map[string]any{"id": "v1", "key": "API_URL", "value": "https://api"}
	srv.Projects["p1"] = map[string]any{"id": "p1", "name": "Team A", "type": "team"}
	srv.Users["u1"] = map[string]any{"id": "u1", "email": "owner@example.com"}
	srv.Credentials["c1"] = map[string]any{"id": "c1", "name": "GitHub", "type": "githubApi", "updatedAt": "2026-03-04T10:00:00.000Z"}
	srv.Executions["w1"] = []map[string]any{
		{"id": 7, "workflowId": "w1", "status": "success", "startedAt": "2026-03-03T10:00:00.000Z", "data": map[string]any{"big": true}},
		{"id": 8, "workflowId": "w1", "status": "error", "startedAt": "2026-03-03T11:00:00.000Z", "data": map[string]any{}},
	}
}

func TestImportTree(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()
	seed(srv)
	srv.PageSize = 2 // workflows span two pages

	imp, err := NewImporter(context.Background(), &integration.Options{}, "n8n",
		map[string]string{"location": "n8n://" + srv.Host(), "api_key": n8ntest.APIKey, "scheme": "http"})
	require.NoError(t, err)
	require.NoError(t, imp.Ping(context.Background()))

	got := runImport(t, srv, nil)

	require.Empty(t, got.errs)
	require.Equal(t, []string{"/", "/workflows", "/tags", "/variables", "/projects", "/users", "/credentials"}, got.dirs)

	var paths []string
	for p := range got.files {
		paths = append(paths, p)
	}
	require.ElementsMatch(t, []string{
		"/workflows/w1.json", "/workflows/w2.json", "/workflows/w3.json",
		"/tags/t1.json", "/variables/v1.json", "/projects/p1.json", "/users/u1.json",
		"/credentials/c1.json",
	}, paths)
	require.NotContains(t, got.files["/credentials/c1.json"], "data")

	// The complete workflow is archived, pinned data and tags included.
	w1 := got.files["/workflows/w1.json"]
	require.Equal(t, "Hello", w1["name"])
	require.Contains(t, w1, "pinData")
	require.Len(t, w1["tags"], 1)
	require.Equal(t, "https://api", got.files["/variables/v1.json"]["value"])
}

func TestImportModTime(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()
	seed(srv)

	config := map[string]string{"location": "n8n://" + srv.Host(), "api_key": n8ntest.APIKey, "scheme": "http"}
	imp, err := NewImporter(context.Background(), &integration.Options{}, "n8n", config)
	require.NoError(t, err)

	records := make(chan *integration.Record)
	go func() { _ = imp.Import(context.Background(), records, nil) }()
	mtimes := map[string]string{}
	for rec := range records {
		if rec.Err == nil && !rec.FileInfo.IsDir() {
			mtimes[rec.Pathname] = rec.FileInfo.ModTime().UTC().Format("2006-01-02T15:04:05Z")
			_ = rec.Close()
		}
	}
	require.Equal(t, "2026-03-02T10:00:00Z", mtimes["/workflows/w1.json"])
	require.Equal(t, "2026-03-01T10:00:00Z", mtimes["/tags/t1.json"])
	require.Equal(t, "1970-01-01T00:00:00Z", mtimes["/workflows/w2.json"])
}

func TestImportUnlicensedSectionsLeftOut(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()
	seed(srv)
	srv.Forbidden["variables"] = n8ntest.Unlicensed
	srv.Forbidden["projects"] = n8ntest.Unlicensed

	got := runImport(t, srv, nil)

	require.Empty(t, got.errs)
	require.Equal(t, []string{"/", "/workflows", "/tags", "/users", "/credentials"}, got.dirs)
	require.NotContains(t, got.files, "/variables/v1.json")
}

func TestImportOldInstanceWithoutCredentials(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()
	seed(srv)
	srv.NoCredentialsEndpoint = true

	got := runImport(t, srv, nil)

	require.Empty(t, got.errs)
	require.NotContains(t, got.dirs, "/credentials")
}

func TestImportForbiddenIsRecorded(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()
	seed(srv)
	// A scoped key lacking a permission, or a non-owner key on /users: not a
	// license matter, must show in the snapshot.
	srv.Forbidden["users"] = "Forbidden"
	srv.Forbidden["workflows"] = "Forbidden"
	srv.Forbidden["credentials"] = "Forbidden"

	got := runImport(t, srv, nil)

	require.Contains(t, got.dirs, "/users")
	require.ErrorContains(t, got.errs["/users"], "HTTP 403: Forbidden")
	require.ErrorContains(t, got.errs["/credentials"], "HTTP 403: Forbidden")
	require.ErrorContains(t, got.errs["/workflows"], "HTTP 403")
	require.Contains(t, got.files, "/tags/t1.json")
}

// TestImportCancelled checks that a backup whose consumer goes away does not
// leave Import blocked on a send nobody takes.
func TestImportCancelled(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()
	seed(srv)

	config := map[string]string{"location": "n8n://" + srv.Host(), "api_key": n8ntest.APIKey, "scheme": "http"}
	imp, err := NewImporter(context.Background(), &integration.Options{}, "n8n", config)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	records := make(chan *integration.Record)
	done := make(chan error, 1)
	go func() { done <- imp.Import(ctx, records, nil) }()

	// Take two records, then stop reading and cancel.
	<-records
	<-records
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Import did not return after cancellation")
	}
}

func TestImportExecutions(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()
	seed(srv)

	got := runImport(t, srv, map[string]string{"executions": "true"})

	require.Empty(t, got.errs)
	require.Contains(t, got.dirs, "/executions")
	require.Contains(t, got.dirs, "/executions/w1")
	require.NotContains(t, got.dirs, "/executions/w2")
	require.Contains(t, got.files, "/executions/w1/7.json")
	require.Contains(t, got.files, "/executions/w1/8.json")
	require.Equal(t, map[string]any{"big": true}, got.files["/executions/w1/7.json"]["data"])
}

func TestImportBadKey(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()

	config := map[string]string{"location": "n8n://" + srv.Host(), "api_key": "wrong", "scheme": "http"}
	imp, err := NewImporter(context.Background(), &integration.Options{}, "n8n", config)
	require.NoError(t, err)
	require.ErrorContains(t, imp.Ping(context.Background()), "HTTP 401")
}

func TestFileName(t *testing.T) {
	require.Equal(t, "2tUt1wbLX592XDdX.json", fileName("2tUt1wbLX592XDdX"))
	require.Equal(t, "42.json", fileName("42"))
	require.Equal(t, "a%2Fb.json", fileName("a/b"))
	require.Equal(t, "%2E%2E.json", fileName(".."))
}
