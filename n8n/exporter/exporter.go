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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/poolpOrg/plakar-integrations/n8n/common"
)

func init() {
	exporter.Register("n8n", 0, NewExporter)
}

// maxDocument bounds how much of a record we are willing to read back: a
// workflow with pinned data can be large, but anything past this is not a
// document the API would accept anyway.
const maxDocument = 64 << 20

// Exporter restores a snapshot taken by the n8n importer into an n8n
// instance through its public REST API.
//
// Workflows are matched by id: an existing workflow with the same id is
// replaced, otherwise a new one is created, unless the target already has a
// workflow of that name, which is refused rather than duplicated. Tags are
// matched by name and created when missing. Variables and projects are
// created when missing and left alone when present. Executions and users
// cannot be written through the API and are skipped. Credentials are created
// only from documents carrying their secret data ({name, type, data}), which
// the importer cannot archive (the API never reads secrets back) but a
// decrypted CLI export can; the metadata-only ones from the importer are
// named in a notice as what to recreate by hand.
//
// Records are restored one at a time, in the order the SDK sends them.
type Exporter struct {
	opts   *integration.Options
	cfg    *common.Config
	client *common.Client

	// activate re-enables workflows that were active in the snapshot. Off by
	// default: a restore should not start firing triggers unasked.
	activate bool

	// State of the target, each fetched once on first need.
	names     map[string]string // workflow name -> id present before the restore
	tags      map[string]string // tag name -> id
	variables map[string]string // variable key -> value
	projects  map[string]bool   // project names
	warned    map[string]bool
}

// NewExporter builds an Exporter from the connector configuration.
func NewExporter(appCtx context.Context, opts *integration.Options, name string, config map[string]string) (exporter.Exporter, error) {
	cfg, err := common.ParseConfig(config)
	if err != nil {
		return nil, err
	}
	activate, err := common.ParseBool(config, "activate", false)
	if err != nil {
		return nil, err
	}

	return &Exporter{
		opts:     opts,
		cfg:      cfg,
		client:   common.NewClient(cfg),
		activate: activate,
		warned:   make(map[string]bool),
	}, nil
}

func (exp *Exporter) Type() string {
	return "n8n"
}

func (exp *Exporter) Origin() string {
	return exp.cfg.Origin()
}

func (exp *Exporter) Root() string {
	return "/"
}

func (exp *Exporter) Flags() location.Flags {
	return 0
}

func (exp *Exporter) Ping(ctx context.Context) error {
	if err := exp.client.Ping(ctx); err != nil {
		return fmt.Errorf("n8n %s: %w", exp.cfg.Host, err)
	}
	return nil
}

func (exp *Exporter) Export(ctx context.Context, records <-chan *integration.Record, results chan<- *integration.Result) error {
	defer close(results)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case record, ok := <-records:
			if !ok {
				return nil
			}
			var res *integration.Result
			if err := exp.restore(ctx, record); err != nil {
				res = record.Error(err)
			} else {
				res = record.Ok()
			}
			select {
			case results <- res:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func (exp *Exporter) Close(ctx context.Context) error {
	return nil
}

// kind is what a document of the backup describes.
type kind int

const (
	kindUnknown kind = iota
	kindWorkflow
	kindTag
	kindVariable
	kindProject
	kindCredential
	kindExecution
	kindUser
)

// classify tells the resource a document describes from its fields. The
// path is of no help: plakar rebases paths on the restored subtree, so the
// record for /workflows/x.json arrives as /x.json when only /workflows is
// restored.
func classify(doc map[string]json.RawMessage) kind {
	has := func(k string) bool { _, ok := doc[k]; return ok }
	switch {
	case has("nodes") && has("connections"):
		return kindWorkflow
	case has("workflowId"):
		return kindExecution
	case has("email"):
		return kindUser
	case has("key"):
		return kindVariable
	case has("data"):
		return kindCredential
	case has("type"):
		var typ string
		if json.Unmarshal(doc["type"], &typ) == nil && (typ == "team" || typ == "personal") {
			return kindProject
		}
		// A credential type is a node credential name such as "githubApi".
		return kindCredential
	case has("name"):
		return kindTag
	}
	return kindUnknown
}

// restore reads one record and restores what it describes.
func (exp *Exporter) restore(ctx context.Context, record *integration.Record) error {
	if record.Err != nil {
		return record.Err
	}
	if record.IsXattr || record.FileInfo.IsDir() {
		return nil
	}

	data, err := exp.readDocument(record)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: not a document of an n8n backup", record.Pathname)
	}

	switch classify(doc) {
	case kindWorkflow:
		return exp.restoreWorkflow(ctx, record.Pathname, data)
	case kindTag:
		return exp.restoreTag(ctx, record.Pathname, data)
	case kindVariable:
		return exp.restoreVariable(ctx, record.Pathname, data)
	case kindProject:
		return exp.restoreProject(ctx, record.Pathname, data)
	case kindCredential:
		return exp.restoreCredential(ctx, record.Pathname, data)
	case kindExecution:
		exp.warnOnce("executions", "n8n: executions are not restorable through the API, skipping them\n")
		return nil
	case kindUser:
		exp.warnOnce("users", "n8n: users are not restorable through the API, skipping them\n")
		return nil
	}
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return fmt.Errorf("%s: not a document of an n8n backup (fields: %s)", record.Pathname, strings.Join(keys, ", "))
}

func (exp *Exporter) readDocument(record *integration.Record) ([]byte, error) {
	if record.Reader == nil {
		return nil, fmt.Errorf("%s: no content", record.Pathname)
	}
	data, err := io.ReadAll(io.LimitReader(record.Reader, maxDocument+1))
	if err != nil {
		return nil, fmt.Errorf("%s: reading: %w", record.Pathname, err)
	}
	if len(data) > maxDocument {
		return nil, fmt.Errorf("%s: document larger than %d bytes", record.Pathname, maxDocument)
	}
	return data, nil
}

// workflowDocument is the part of an archived workflow the API accepts back.
// The schema rejects unknown and read-only properties (id, active,
// timestamps, tags, versionId, ...), so only these are sent. Description and
// pinned data are accepted by n8n 2.x and refused by 1.x: they are sent
// first and dropped on a 400, see sendWorkflow.
type workflowDocument struct {
	Name        string          `json:"name"`
	Nodes       json.RawMessage `json:"nodes"`
	Connections json.RawMessage `json:"connections"`
	Settings    json.RawMessage `json:"settings"`
	StaticData  json.RawMessage `json:"staticData,omitempty"`
	Description json.RawMessage `json:"description,omitempty"`
	PinData     json.RawMessage `json:"pinData,omitempty"`
}

// minimal returns the document without the fields n8n 1.x refuses.
func (d workflowDocument) minimal() workflowDocument {
	d.Description = nil
	d.PinData = nil
	return d
}

// sendWorkflow creates or updates a workflow, retrying once with the minimal
// body when the instance refuses the full one. A 400 is answered before
// anything is written, so the second attempt cannot duplicate.
func (exp *Exporter) sendWorkflow(ctx context.Context, id string, body workflowDocument) (common.Object, error) {
	send := func(body workflowDocument) (common.Object, error) {
		if id != "" {
			return exp.client.UpdateWorkflow(ctx, id, body)
		}
		return exp.client.CreateWorkflow(ctx, body)
	}
	restored, err := send(body)
	if err != nil && common.IsStatus(err, http.StatusBadRequest) && (body.Description != nil || body.PinData != nil) {
		restored, err = send(body.minimal())
	}
	return restored, err
}

func (exp *Exporter) restoreWorkflow(ctx context.Context, pathname string, data []byte) error {

	var archived struct {
		ID     string `json:"id"`
		Active bool   `json:"active"`
		Tags   []struct {
			Name string `json:"name"`
		} `json:"tags"`
		workflowDocument
	}
	if err := json.Unmarshal(data, &archived); err != nil {
		return fmt.Errorf("%s: %w", pathname, err)
	}
	if archived.Name == "" || archived.Nodes == nil || archived.Connections == nil {
		return fmt.Errorf("%s: not a workflow document", pathname)
	}
	body := archived.workflowDocument
	if body.Settings == nil {
		body.Settings = json.RawMessage(`{}`)
	}

	// Replace the workflow when the target already has this id, create it
	// otherwise. A new id is assigned on creation.
	var (
		restored common.Object
		exists   bool
		err      error
	)
	if archived.ID != "" {
		_, err := exp.client.GetWorkflow(ctx, archived.ID)
		switch {
		case err == nil:
			exists = true
		case common.IsStatus(err, http.StatusNotFound):
		default:
			return fmt.Errorf("%s: looking up workflow %s: %w", pathname, archived.ID, err)
		}
	}
	if exists {
		restored, err = exp.sendWorkflow(ctx, archived.ID, body)
		if err != nil {
			return fmt.Errorf("%s: updating workflow %s: %w", pathname, archived.ID, err)
		}
	} else {
		// Creating gets a new id, so nothing would tie a second run to this
		// one: a same-named workflow that was already on the target is the
		// sign of a previous restore, and is not duplicated.
		if err := exp.loadWorkflowNames(ctx); err != nil {
			return fmt.Errorf("%s: %w", pathname, err)
		}
		if id, ok := exp.names[archived.Name]; ok {
			return fmt.Errorf("%s: a workflow named %q already exists on the target as %s and the archived id %q does not match; delete or rename it first", pathname, archived.Name, id, archived.ID)
		}
		restored, err = exp.sendWorkflow(ctx, "", body)
		if err != nil {
			return fmt.Errorf("%s: creating workflow: %w", pathname, err)
		}
	}

	if len(archived.Tags) > 0 {
		ids := make([]string, 0, len(archived.Tags))
		for _, t := range archived.Tags {
			id, err := exp.tagID(ctx, t.Name)
			if err != nil {
				return fmt.Errorf("%s: tag %q: %w", pathname, t.Name, err)
			}
			ids = append(ids, id)
		}
		if err := exp.client.SetWorkflowTags(ctx, restored.ID, ids); err != nil {
			return fmt.Errorf("%s: tagging workflow %s: %w", pathname, restored.ID, err)
		}
	}

	if archived.Active && exp.activate {
		if err := exp.client.ActivateWorkflow(ctx, restored.ID); err != nil {
			return fmt.Errorf("%s: activating workflow %s: %w", pathname, restored.ID, err)
		}
	}
	return nil
}

// loadWorkflowNames records, once, the names of the workflows the target
// had before this restore started.
func (exp *Exporter) loadWorkflowNames(ctx context.Context) error {
	if exp.names != nil {
		return nil
	}
	workflows, err := exp.client.ListWorkflows(ctx)
	if err != nil {
		return fmt.Errorf("listing workflows: %w", err)
	}
	exp.names = make(map[string]string, len(workflows))
	for _, w := range workflows {
		exp.names[w.Name] = w.ID
	}
	return nil
}

func (exp *Exporter) restoreTag(ctx context.Context, pathname string, data []byte) error {
	var tag struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &tag); err != nil {
		return fmt.Errorf("%s: %w", pathname, err)
	}
	if tag.Name == "" {
		return fmt.Errorf("%s: not a tag document", pathname)
	}
	if _, err := exp.tagID(ctx, tag.Name); err != nil {
		return fmt.Errorf("%s: %w", pathname, err)
	}
	return nil
}

// tagID returns the id of the tag named name on the target, creating it when
// missing. The tag list is fetched once.
func (exp *Exporter) tagID(ctx context.Context, name string) (string, error) {
	if exp.tags == nil {
		tags, err := exp.client.ListTags(ctx)
		if err != nil {
			return "", fmt.Errorf("listing tags: %w", err)
		}
		exp.tags = make(map[string]string, len(tags))
		for _, t := range tags {
			exp.tags[t.Name] = t.ID
		}
	}
	if id, ok := exp.tags[name]; ok {
		return id, nil
	}
	created, err := exp.client.CreateTag(ctx, name)
	if err != nil {
		return "", fmt.Errorf("creating tag: %w", err)
	}
	exp.tags[name] = created.ID
	return created.ID, nil
}

func (exp *Exporter) restoreVariable(ctx context.Context, pathname string, data []byte) error {
	var v struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("%s: %w", pathname, err)
	}
	if v.Key == "" {
		return fmt.Errorf("%s: not a variable document", pathname)
	}

	if exp.variables == nil {
		vars, err := exp.client.ListVariables(ctx)
		if err != nil {
			return fmt.Errorf("%s: listing variables: %w", pathname, err)
		}
		exp.variables = make(map[string]string, len(vars))
		for _, o := range vars {
			var cur struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(o.Raw, &cur); err != nil {
				return fmt.Errorf("%s: listing variables: %w", pathname, err)
			}
			exp.variables[o.Name] = cur.Value
		}
	}
	// The API has no update call; an existing key is left alone, and a
	// differing value is reported rather than silently kept.
	if cur, ok := exp.variables[v.Key]; ok {
		if cur != v.Value {
			return fmt.Errorf("%s: variable %q already exists with a different value", pathname, v.Key)
		}
		return nil
	}
	if err := exp.client.CreateVariable(ctx, v.Key, v.Value); err != nil {
		return fmt.Errorf("%s: creating variable %q: %w", pathname, v.Key, err)
	}
	exp.variables[v.Key] = v.Value
	return nil
}

func (exp *Exporter) restoreProject(ctx context.Context, pathname string, data []byte) error {
	var p struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("%s: %w", pathname, err)
	}
	if p.Name == "" {
		return fmt.Errorf("%s: not a project document", pathname)
	}
	// Personal projects belong to users and cannot be created.
	if p.Type == "personal" {
		return nil
	}

	if exp.projects == nil {
		projects, err := exp.client.ListProjects(ctx)
		if err != nil {
			return fmt.Errorf("%s: listing projects: %w", pathname, err)
		}
		exp.projects = make(map[string]bool, len(projects))
		for _, o := range projects {
			exp.projects[o.Name] = true
		}
	}
	if exp.projects[p.Name] {
		return nil
	}
	if err := exp.client.CreateProject(ctx, p.Name); err != nil {
		return fmt.Errorf("%s: creating project %q: %w", pathname, p.Name, err)
	}
	exp.projects[p.Name] = true
	return nil
}

func (exp *Exporter) restoreCredential(ctx context.Context, pathname string, data []byte) error {
	var c struct {
		Name string          `json:"name"`
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("%s: %w", pathname, err)
	}
	if c.Name == "" || c.Type == "" {
		return fmt.Errorf("%s: not a credential document", pathname)
	}
	if len(c.Data) == 0 || string(c.Data) == "null" {
		// What the importer archives: the API never hands the secret out,
		// so there is nothing to restore but the name of what to recreate.
		exp.warnOnce("credential:"+c.Name+"/"+c.Type, "n8n: credential %q (%s) has no secret data in the backup and cannot be restored; recreate it by hand\n", c.Name, c.Type)
		return nil
	}
	body := map[string]any{"name": c.Name, "type": c.Type, "data": c.Data}
	if err := exp.client.CreateCredential(ctx, body); err != nil {
		return fmt.Errorf("%s: creating credential %q: %w", pathname, c.Name, err)
	}
	return nil
}

func (exp *Exporter) warnOnce(key, format string, args ...any) {
	if exp.warned[key] {
		return
	}
	exp.warned[key] = true
	w := io.Writer(os.Stderr)
	if exp.opts != nil && exp.opts.Stderr != nil {
		w = exp.opts.Stderr
	}
	fmt.Fprintf(w, format, args...)
}
