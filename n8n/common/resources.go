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
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Object is one item from an n8n list or get endpoint. Raw is the verbatim
// JSON as returned by the API (this is what gets archived); the typed fields
// are the handful needed to lay out the backup tree.
type Object struct {
	ID string
	// Name is the workflow/tag/project name, the variable key or the user
	// email, whichever the resource carries.
	Name string
	// ModTime is the best "last modified" the resource exposes: updatedAt,
	// else stoppedAt/startedAt for executions, else zero.
	ModTime time.Time
	Raw     json.RawMessage
}

// decodeObject captures the common metadata of one raw item while keeping the
// full JSON in Raw.
func decodeObject(r json.RawMessage) (Object, error) {
	var meta struct {
		ID        any    `json:"id"`
		Name      string `json:"name"`
		Key       string `json:"key"`
		Email     string `json:"email"`
		UpdatedAt string `json:"updatedAt"`
		StoppedAt string `json:"stoppedAt"`
		StartedAt string `json:"startedAt"`
	}
	if err := json.Unmarshal(r, &meta); err != nil {
		return Object{}, err
	}

	var id string
	switch v := meta.ID.(type) {
	case string:
		id = v
	case float64:
		// Execution ids are integers.
		id = strconv.FormatFloat(v, 'f', -1, 64)
	case nil:
		return Object{}, fmt.Errorf("object has no id")
	default:
		return Object{}, fmt.Errorf("object has an id of unexpected type %T", v)
	}
	if id == "" {
		return Object{}, fmt.Errorf("object has an empty id")
	}

	name := meta.Name
	if name == "" {
		name = meta.Key
	}
	if name == "" {
		name = meta.Email
	}

	var mtime time.Time
	for _, ts := range []string{meta.UpdatedAt, meta.StoppedAt, meta.StartedAt} {
		if ts == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			mtime = t
			break
		}
	}

	return Object{ID: id, Name: name, ModTime: mtime, Raw: r}, nil
}

func decodeObjects(raw []json.RawMessage) ([]Object, error) {
	out := make([]Object, 0, len(raw))
	for _, r := range raw {
		o, err := decodeObject(r)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// collect drains a paginated list endpoint into a slice of Objects.
func (c *Client) collect(ctx context.Context, path string, extra url.Values) ([]Object, error) {
	var all []Object
	err := c.list(ctx, path, extra, func(page []json.RawMessage) error {
		objs, err := decodeObjects(page)
		if err != nil {
			return fmt.Errorf("list %s: %w", path, err)
		}
		all = append(all, objs...)
		return nil
	})
	return all, err
}

// ListWorkflows returns every workflow. Pinned data is left out of the list
// answer; GetWorkflow fetches the complete object.
func (c *Client) ListWorkflows(ctx context.Context) ([]Object, error) {
	q := url.Values{}
	q.Set("excludePinnedData", "true")
	return c.collect(ctx, "/workflows", q)
}

// GetWorkflow returns one workflow in full, pinned data included.
func (c *Client) GetWorkflow(ctx context.Context, id string) (Object, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "/workflows/"+url.PathEscape(id), nil, &raw); err != nil {
		return Object{}, err
	}
	return decodeObject(raw)
}

// ListTags returns every tag.
func (c *Client) ListTags(ctx context.Context) ([]Object, error) {
	return c.collect(ctx, "/tags", nil)
}

// ListVariables returns every variable. The endpoint is license-gated: an
// unlicensed instance answers 403.
func (c *Client) ListVariables(ctx context.Context) ([]Object, error) {
	return c.collect(ctx, "/variables", nil)
}

// ListProjects returns every project. License-gated like variables.
func (c *Client) ListProjects(ctx context.Context) ([]Object, error) {
	return c.collect(ctx, "/projects", nil)
}

// ListUsers returns every user. Only the instance owner's key may call it;
// other keys get 403.
func (c *Client) ListUsers(ctx context.Context) ([]Object, error) {
	return c.collect(ctx, "/users", nil)
}

// ListCredentials returns the metadata (name, type, owner) of every
// credential, never their secret data. n8n 2.x serves it to keys holding the
// credential:list scope; n8n 1.x has no such endpoint and answers 404.
func (c *Client) ListCredentials(ctx context.Context) ([]Object, error) {
	return c.collect(ctx, "/credentials", nil)
}

// ListExecutions walks the executions of one workflow page by page, with
// their run data when includeData is set, calling fn for each page. Pages are
// not accumulated: executions with data are the bulk of an instance.
func (c *Client) ListExecutions(ctx context.Context, workflowID string, includeData bool, fn func([]Object) error) error {
	q := url.Values{}
	q.Set("workflowId", workflowID)
	if includeData {
		q.Set("includeData", "true")
	}
	return c.list(ctx, "/executions", q, func(page []json.RawMessage) error {
		objs, err := decodeObjects(page)
		if err != nil {
			return fmt.Errorf("list /executions: %w", err)
		}
		return fn(objs)
	})
}

// CreateWorkflow creates a workflow from a body shaped like the public API's
// workflow schema and returns the created object (with its new id).
func (c *Client) CreateWorkflow(ctx context.Context, body any) (Object, error) {
	var raw json.RawMessage
	if err := c.send(ctx, http.MethodPost, "/workflows", body, &raw); err != nil {
		return Object{}, err
	}
	return decodeObject(raw)
}

// UpdateWorkflow replaces an existing workflow.
func (c *Client) UpdateWorkflow(ctx context.Context, id string, body any) (Object, error) {
	var raw json.RawMessage
	if err := c.send(ctx, http.MethodPut, "/workflows/"+url.PathEscape(id), body, &raw); err != nil {
		return Object{}, err
	}
	return decodeObject(raw)
}

// ActivateWorkflow turns a workflow's triggers on.
func (c *Client) ActivateWorkflow(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/workflows/"+url.PathEscape(id)+"/activate", nil, nil, nil)
}

// SetWorkflowTags replaces the set of tags attached to a workflow.
func (c *Client) SetWorkflowTags(ctx context.Context, id string, tagIDs []string) error {
	body := make([]map[string]string, 0, len(tagIDs))
	for _, t := range tagIDs {
		body = append(body, map[string]string{"id": t})
	}
	return c.send(ctx, http.MethodPut, "/workflows/"+url.PathEscape(id)+"/tags", body, nil)
}

// CreateTag creates a tag and returns it.
func (c *Client) CreateTag(ctx context.Context, name string) (Object, error) {
	var raw json.RawMessage
	if err := c.send(ctx, http.MethodPost, "/tags", map[string]string{"name": name}, &raw); err != nil {
		return Object{}, err
	}
	return decodeObject(raw)
}

// CreateVariable creates a variable.
func (c *Client) CreateVariable(ctx context.Context, key, value string) error {
	return c.send(ctx, http.MethodPost, "/variables", map[string]string{"key": key, "value": value}, nil)
}

// CreateProject creates a team project.
func (c *Client) CreateProject(ctx context.Context, name string) error {
	return c.send(ctx, http.MethodPost, "/projects", map[string]string{"name": name}, nil)
}

// CreateCredential creates a credential from {name, type, data}.
func (c *Client) CreateCredential(ctx context.Context, body any) error {
	return c.send(ctx, http.MethodPost, "/credentials", body, nil)
}
