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

package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/poolpOrg/plakar-integrations/n8n/internal/n8ntest"
	"github.com/stretchr/testify/require"
)

func doc(t *testing.T, pathname string, v any) *integration.Record {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	fi := objects.NewFileInfo(pathname, int64(len(data)), 0644, time.Now(), 0, 0, 0, 0, 0)
	return integration.NewRecord(pathname, "", fi, nil, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
}

func dir(pathname string) *integration.Record {
	fi := objects.NewFileInfo(pathname, 0, 0755|os.ModeDir, time.Now(), 0, 0, 0, 0, 0)
	return integration.NewRecord(pathname, "", fi, nil, nil)
}

func runExport(t *testing.T, srv *n8ntest.Server, extra map[string]string, recs ...*integration.Record) map[string]error {
	t.Helper()

	config := map[string]string{
		"location": "n8n://" + srv.Host(),
		"api_key":  n8ntest.APIKey,
		"scheme":   "http",
	}
	for k, v := range extra {
		config[k] = v
	}
	exp, err := NewExporter(context.Background(), &integration.Options{Stderr: io.Discard}, "n8n", config)
	require.NoError(t, err)
	require.NoError(t, exp.Ping(context.Background()))

	records := make(chan *integration.Record)
	results := make(chan *integration.Result)
	done := make(chan error, 1)
	go func() {
		done <- exp.Export(context.Background(), records, results)
	}()
	go func() {
		for _, r := range recs {
			records <- r
		}
		close(records)
	}()

	errs := map[string]error{}
	for res := range results {
		errs[res.Record.Pathname] = res.Err
	}
	require.NoError(t, <-done)
	require.NoError(t, exp.Close(context.Background()))
	return errs
}

func workflow(id, name string, active bool, tags ...string) map[string]any {
	tagObjs := make([]any, 0, len(tags))
	for _, tg := range tags {
		tagObjs = append(tagObjs, map[string]any{"id": "old-" + tg, "name": tg})
	}
	return map[string]any{
		"id": id, "name": name, "active": active,
		"createdAt": "2026-01-01T00:00:00.000Z", "updatedAt": "2026-01-02T00:00:00.000Z",
		"versionId":   "abc",
		"nodes":       []any{map[string]any{"name": "Start", "type": "n8n-nodes-base.manualTrigger"}},
		"connections": map[string]any{},
		"settings":    map[string]any{"executionOrder": "v1"},
		"staticData":  map[string]any{"lastId": 1},
		"pinData":     map[string]any{},
		"tags":        tagObjs,
	}
}

func TestExportWorkflows(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()
	srv.Tags["t1"] = map[string]any{"id": "t1", "name": "prod"}
	srv.Workflows["w1"] = map[string]any{
		"id": "w1", "name": "Stale", "active": false,
		"nodes": []any{}, "connections": map[string]any{}, "settings": map[string]any{}, "tags": []any{},
	}

	errs := runExport(t, srv, nil,
		dir("/"), dir("/workflows"),
		doc(t, "/workflows/w1.json", workflow("w1", "Hello", true, "prod", "new-tag")),
		doc(t, "/workflows/w9.json", workflow("w9", "Fresh", true)),
	)
	for p, err := range errs {
		require.NoError(t, err, p)
	}

	// w1 existed: replaced in place, read-only fields dropped, tags mapped
	// by name with the missing one created.
	w1 := srv.Workflows["w1"]
	require.Equal(t, "Hello", w1["name"])
	require.Equal(t, map[string]any{"lastId": float64(1)}, w1["staticData"])
	require.Len(t, w1["tags"], 2)
	require.Len(t, srv.Tags, 2)

	// w9 did not exist: created under a new id.
	require.NotContains(t, srv.Workflows, "w9")
	require.Len(t, srv.Workflows, 2)

	// Not re-activated unless asked.
	require.Empty(t, srv.Activated)
}

// TestExportNoDuplicateByName checks that a workflow whose id is unknown to
// the target is not created when a same-named one already lives there, while
// two same-named workflows of one snapshot still both land on an empty target.
func TestExportNoDuplicateByName(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()
	srv.Workflows["w1"] = map[string]any{
		"id": "w1", "name": "Hello", "active": false,
		"nodes": []any{}, "connections": map[string]any{}, "settings": map[string]any{}, "tags": []any{},
	}

	errs := runExport(t, srv, nil,
		doc(t, "/workflows/w1.json", workflow("w1", "Hello", false)),
		doc(t, "/workflows/w2.json", workflow("w2", "Hello", false)),
		doc(t, "/workflows/w3.json", workflow("w3", "Twin", false)),
		doc(t, "/workflows/w4.json", workflow("w4", "Twin", false)),
	)
	require.NoError(t, errs["/workflows/w1.json"])
	require.ErrorContains(t, errs["/workflows/w2.json"], `a workflow named "Hello" already exists on the target as w1`)
	require.NoError(t, errs["/workflows/w3.json"])
	require.NoError(t, errs["/workflows/w4.json"])
	require.Len(t, srv.Workflows, 3)
}

// TestExportCancelled checks that a cancelled restore returns instead of
// pushing results nobody reads.
func TestExportCancelled(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()

	config := map[string]string{"location": "n8n://" + srv.Host(), "api_key": n8ntest.APIKey, "scheme": "http"}
	exp, err := NewExporter(context.Background(), &integration.Options{}, "n8n", config)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	records := make(chan *integration.Record)
	results := make(chan *integration.Result)
	done := make(chan error, 1)
	go func() { done <- exp.Export(ctx, records, results) }()

	records <- doc(t, "/tags/a.json", map[string]any{"id": "a", "name": "prod"})
	<-results
	records <- doc(t, "/tags/b.json", map[string]any{"id": "b", "name": "staging"})
	cancel() // the result of b is never read

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Export did not return after cancellation")
	}
}

func TestExportActivate(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()

	errs := runExport(t, srv, map[string]string{"activate": "true"},
		doc(t, "/workflows/w1.json", workflow("w1", "On", true)),
		doc(t, "/workflows/w2.json", workflow("w2", "Off", false)),
	)
	for p, err := range errs {
		require.NoError(t, err, p)
	}
	require.Len(t, srv.Activated, 1)
	require.Equal(t, "On", srv.Workflows[srv.Activated[0]]["name"])
}

func TestExportTagsVariablesProjects(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()
	srv.Tags["t1"] = map[string]any{"id": "t1", "name": "prod"}
	srv.Variables["v1"] = map[string]any{"id": "v1", "key": "SAME", "value": "x"}
	srv.Variables["v2"] = map[string]any{"id": "v2", "key": "DIFF", "value": "x"}
	srv.Projects["p1"] = map[string]any{"id": "p1", "name": "Team A", "type": "team"}

	errs := runExport(t, srv, nil,
		doc(t, "/tags/a.json", map[string]any{"id": "a", "name": "prod"}),
		doc(t, "/tags/b.json", map[string]any{"id": "b", "name": "staging"}),
		doc(t, "/variables/1.json", map[string]any{"id": "1", "key": "SAME", "value": "x"}),
		doc(t, "/variables/2.json", map[string]any{"id": "2", "key": "DIFF", "value": "y"}),
		doc(t, "/variables/3.json", map[string]any{"id": "3", "key": "NEW", "value": "z"}),
		doc(t, "/projects/1.json", map[string]any{"id": "1", "name": "Team A", "type": "team"}),
		doc(t, "/projects/2.json", map[string]any{"id": "2", "name": "Team B", "type": "team"}),
		doc(t, "/projects/3.json", map[string]any{"id": "3", "name": "Owner", "type": "personal"}),
	)

	require.NoError(t, errs["/tags/a.json"])
	require.NoError(t, errs["/tags/b.json"])
	require.Len(t, srv.Tags, 2)

	require.NoError(t, errs["/variables/1.json"])
	require.ErrorContains(t, errs["/variables/2.json"], "already exists with a different value")
	require.NoError(t, errs["/variables/3.json"])
	require.Len(t, srv.Variables, 3)
	require.Equal(t, "x", srv.Variables["v2"]["value"])

	require.NoError(t, errs["/projects/1.json"])
	require.NoError(t, errs["/projects/2.json"])
	require.NoError(t, errs["/projects/3.json"])
	require.Len(t, srv.Projects, 2)
}

func TestExportCredentials(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()

	errs := runExport(t, srv, nil,
		doc(t, "/credentials/1.json", map[string]any{"name": "GitHub", "type": "githubApi", "data": map[string]any{"accessToken": "x"}}),
		doc(t, "/credentials/2.json", map[string]any{"id": "c2", "name": "Nodata", "type": "githubApi", "createdAt": "2026-01-01T00:00:00.000Z"}),
	)
	require.NoError(t, errs["/credentials/1.json"])
	require.NoError(t, errs["/credentials/2.json"]) // metadata only: noticed, not restored
	require.Len(t, srv.Credentials, 1)
}

// TestExportPinDataFallback checks that description and pinned data are
// restored on an instance that takes them and dropped on one that refuses.
func TestExportPinDataFallback(t *testing.T) {
	wf := workflow("w1", "Pinned", false)
	wf["description"] = "a workflow"
	wf["pinData"] = map[string]any{"Start": []any{map[string]any{"json": map[string]any{"a": 1}}}}

	for _, accepts := range []bool{true, false} {
		srv := n8ntest.New()
		srv.AcceptsPinData = accepts
		errs := runExport(t, srv, nil, doc(t, "/workflows/w1.json", wf))
		require.NoError(t, errs["/workflows/w1.json"])
		require.Len(t, srv.Workflows, 1)
		for _, got := range srv.Workflows {
			_, hasPin := got["pinData"]
			require.Equal(t, accepts, hasPin)
		}
		srv.Close()
	}
}

func TestExportSkipsAndRejects(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()

	errs := runExport(t, srv, nil,
		dir("/executions"), dir("/executions/w1"),
		doc(t, "/executions/w1/7.json", map[string]any{"id": 7, "workflowId": "w1", "status": "success"}),
		doc(t, "/users/u1.json", map[string]any{"id": "u1", "email": "a@b"}),
		doc(t, "/notes.txt", "hello"),
		doc(t, "/workflows/bad.json", map[string]any{"foo": "bar"}),
	)
	require.NoError(t, errs["/executions/w1/7.json"])
	require.NoError(t, errs["/users/u1.json"])
	require.ErrorContains(t, errs["/notes.txt"], "not a document of an n8n backup")
	require.ErrorContains(t, errs["/workflows/bad.json"], "not a document of an n8n backup")
	require.Empty(t, srv.Workflows)
}

// TestExportRebasedPaths checks that documents are recognised by content:
// restoring <snap>:/workflows hands the exporter /x.json, not
// /workflows/x.json.
func TestExportRebasedPaths(t *testing.T) {
	srv := n8ntest.New()
	defer srv.Close()

	errs := runExport(t, srv, nil,
		doc(t, "/w1.json", workflow("w1", "Hello", false, "prod")),
		doc(t, "/t.json", map[string]any{"id": "t", "name": "staging", "createdAt": "2026-01-01T00:00:00.000Z"}),
		doc(t, "/v.json", map[string]any{"id": "v", "key": "K", "value": "x", "type": "string"}),
		doc(t, "/p.json", map[string]any{"id": "p", "name": "Team", "type": "team"}),
		doc(t, "/c.json", map[string]any{"id": "c", "name": "GitHub", "type": "githubApi", "data": map[string]any{"token": "x"}}),
		doc(t, "/e.json", map[string]any{"id": 7, "workflowId": "w1", "status": "success"}),
		doc(t, "/u.json", map[string]any{"id": "u", "email": "a@b", "firstName": "A"}),
	)
	for p, err := range errs {
		require.NoError(t, err, p)
	}
	require.Len(t, srv.Workflows, 1)
	require.Len(t, srv.Tags, 2)
	require.Len(t, srv.Variables, 1)
	require.Len(t, srv.Projects, 1)
}

func TestClassify(t *testing.T) {
	cases := map[string]kind{
		`{"name":"x","nodes":[],"connections":{}}`:     kindWorkflow,
		`{"id":7,"workflowId":"w","status":"success"}`: kindExecution,
		`{"id":"u","email":"a@b"}`:                     kindUser,
		`{"id":"v","key":"K","value":"x"}`:             kindVariable,
		`{"name":"c","type":"githubApi","data":{}}`:    kindCredential,
		`{"name":"c","type":"githubApi"}`:              kindCredential,
		`{"id":"p","name":"Team","type":"team"}`:       kindProject,
		`{"id":"p","name":"Me","type":"personal"}`:     kindProject,
		`{"id":"t","name":"prod","updatedAt":"2026"}`:  kindTag,
		`{"foo":"bar"}`: kindUnknown,
	}
	for raw, want := range cases {
		var doc map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(raw), &doc))
		require.Equal(t, want, classify(doc), raw)
	}
}
