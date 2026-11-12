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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/poolpOrg/plakar-integrations/n8n/common"
)

// walk lays the instance out as a virtual filesystem and emits one record per
// node. The tree is:
//
//	/
//	├── workflows/<workflow_id>.json              (nodes, connections, settings, pinned data, tags)
//	├── tags/<tag_id>.json
//	├── variables/<variable_id>.json             (license-gated)
//	├── projects/<project_id>.json               (license-gated)
//	├── users/<user_id>.json                     (instance owner key only)
//	├── credentials/<credential_id>.json         (metadata only; n8n 2.x, credential:list scope)
//	└── executions/<workflow_id>/<execution_id>.json   (opt-in)
//
// Sections are independent: a failure listing one is reported as an error
// record for that directory and does not abort the others. The one failure
// that is not an error is n8n refusing a section the instance's license does
// not include (variables, projects): that section is left out with a notice.
// Any other refusal, such as an API key lacking the scope, is recorded.
//
// Credential secrets are not in the tree: the public API never reads them
// back. Their metadata is, on the n8n versions that list it, so a restore
// knows what to recreate by hand.
//
// The walk stops at the first send the SDK no longer takes, which is how a
// cancelled backup reaches it; it then returns the context's error.
func (imp *Importer) walk(ctx context.Context, records chan<- *integration.Record) error {
	if !imp.emitDir(ctx, records, "/") {
		return ctx.Err()
	}

	imp.walkWorkflows(ctx, records)
	imp.walkSimple(ctx, records, "/tags", imp.client.ListTags)
	imp.walkSimple(ctx, records, "/variables", imp.client.ListVariables)
	imp.walkSimple(ctx, records, "/projects", imp.client.ListProjects)
	imp.walkSimple(ctx, records, "/users", imp.client.ListUsers)
	imp.walkCredentials(ctx, records)

	return ctx.Err()
}

func (imp *Importer) walkWorkflows(ctx context.Context, records chan<- *integration.Record) {
	if !imp.emitDir(ctx, records, "/workflows") {
		return
	}
	if imp.executions && !imp.emitDir(ctx, records, "/executions") {
		return
	}

	workflows, err := imp.client.ListWorkflows(ctx)
	if err != nil {
		imp.send(ctx, records, integration.NewError("/workflows", err))
		return
	}
	for _, w := range workflows {
		if ctx.Err() != nil {
			return
		}
		p := path.Join("/workflows", fileName(w.ID))

		// The list leaves pinned data out; fetch the complete object.
		full, err := imp.client.GetWorkflow(ctx, w.ID)
		if err != nil {
			if !imp.send(ctx, records, integration.NewError(p, err)) {
				return
			}
			continue
		}
		if !imp.emitJSON(ctx, records, p, full.Raw, full.ModTime) {
			return
		}

		if imp.executions && !imp.walkExecutions(ctx, records, w.ID) {
			return
		}
	}
}

// walkExecutions streams the executions of one workflow page by page; the
// per-workflow directory is emitted with the first non-empty page.
func (imp *Importer) walkExecutions(ctx context.Context, records chan<- *integration.Record, workflowID string) bool {
	dir := path.Join("/executions", safeName(workflowID))
	started := false
	err := imp.client.ListExecutions(ctx, workflowID, true, func(page []common.Object) error {
		if len(page) > 0 && !started {
			started = true
			if !imp.emitDir(ctx, records, dir) {
				return ctx.Err()
			}
		}
		for _, e := range page {
			if !imp.emitJSON(ctx, records, path.Join(dir, fileName(e.ID)), e.Raw, e.ModTime) {
				return ctx.Err()
			}
		}
		return nil
	})
	if err != nil {
		return imp.send(ctx, records, integration.NewError(dir, err))
	}
	return true
}

// walkCredentials is walkSimple for /credentials, which older instances do
// not serve at all: a 404 there means an n8n 1.x and the section is left out.
func (imp *Importer) walkCredentials(ctx context.Context, records chan<- *integration.Record) {
	if ctx.Err() != nil {
		return
	}
	if _, err := imp.client.ListCredentials(ctx); common.IsStatus(err, http.StatusNotFound) {
		fmt.Fprintf(imp.stderr(), "n8n: leaving out /credentials: this n8n does not list credentials\n")
		return
	}
	imp.walkSimple(ctx, records, "/credentials", imp.client.ListCredentials)
}

// walkSimple emits one file per object of a flat collection.
func (imp *Importer) walkSimple(ctx context.Context, records chan<- *integration.Record, dir string, list func(context.Context) ([]common.Object, error)) {
	if ctx.Err() != nil {
		return
	}
	objs, err := list(ctx)
	if err != nil {
		if common.IsUnlicensed(err) {
			fmt.Fprintf(imp.stderr(), "n8n: leaving out %s: %v\n", dir, err)
			return
		}
		if imp.emitDir(ctx, records, dir) {
			imp.send(ctx, records, integration.NewError(dir, err))
		}
		return
	}
	if !imp.emitDir(ctx, records, dir) {
		return
	}
	for _, o := range objs {
		if !imp.emitJSON(ctx, records, path.Join(dir, fileName(o.ID)), o.Raw, o.ModTime) {
			return
		}
	}
}

func (imp *Importer) stderr() io.Writer {
	if imp.opts != nil && imp.opts.Stderr != nil {
		return imp.opts.Stderr
	}
	return os.Stderr
}

// fileName turns a resource id into the name of its document.
func fileName(id string) string {
	return safeName(id) + ".json"
}

// safeName turns a resource id into a path component. Ids are opaque strings
// chosen by n8n; the ones it generates are URL-safe, but a path separator or
// a dot-only name would break the tree, so anything unusual is escaped.
func safeName(id string) string {
	safe := true
	for _, r := range id {
		if r == '/' || r == '\\' || r == 0 {
			safe = false
			break
		}
	}
	if !safe || id == "." || id == ".." {
		return escape(id)
	}
	return id
}

func escape(id string) string {
	var buf bytes.Buffer
	for _, b := range []byte(id) {
		if b == '/' || b == '\\' || b == 0 || b == '%' || b == '.' {
			fmt.Fprintf(&buf, "%%%02X", b)
		} else {
			buf.WriteByte(b)
		}
	}
	return buf.String()
}

// --- record helpers -------------------------------------------------------

// send hands a record to the SDK unless the backup is cancelled first. It
// reports whether the record was taken; the walk stops on false.
func (imp *Importer) send(ctx context.Context, records chan<- *integration.Record, rec *integration.Record) bool {
	select {
	case records <- rec:
		return true
	case <-ctx.Done():
		return false
	}
}

func (imp *Importer) emitDir(ctx context.Context, records chan<- *integration.Record, p string) bool {
	fi := objects.NewFileInfo(path.Base(p), 0, 0755|os.ModeDir, time.Unix(0, 0), 0, 0, 0, 0, 0)
	return imp.send(ctx, records, integration.NewRecord(p, "", fi, []string{}, nil))
}

// emitJSON emits a regular file holding the pretty-printed document, so that
// snapshots diff and read well. The content is the API's own JSON.
func (imp *Importer) emitJSON(ctx context.Context, records chan<- *integration.Record, p string, raw json.RawMessage, mtime time.Time) bool {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return imp.send(ctx, records, integration.NewError(p, fmt.Errorf("malformed JSON from the API: %w", err)))
	}
	buf.WriteByte('\n')
	content := buf.Bytes()

	if mtime.IsZero() {
		mtime = time.Unix(0, 0)
	}
	fi := objects.NewFileInfo(path.Base(p), int64(len(content)), 0644, mtime, 0, 0, 0, 0, 0)
	return imp.send(ctx, records, integration.NewRecord(p, "", fi, []string{}, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(content)), nil
	}))
}
