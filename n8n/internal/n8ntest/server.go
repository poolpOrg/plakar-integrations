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

// Package n8ntest is an in-memory stand-in for the n8n public API, enough of
// it for the importer and exporter tests: cursor pagination, the API-key
// header, and the handful of read and write calls the connector makes.
package n8ntest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// APIKey is the key the fake accepts.
const APIKey = "n8n-test-key"

// Unlicensed is the 403 message of a feature the license does not include.
const Unlicensed = "Your license does not allow for this feature. To enable it, please upgrade to a license that supports this feature."

// Server is a fake n8n instance. Exported maps hold the instance state and
// may be seeded before use; every document is a JSON object whose "id" is
// the map key.
type Server struct {
	*httptest.Server

	mu          sync.Mutex
	Workflows   map[string]map[string]any
	Tags        map[string]map[string]any
	Variables   map[string]map[string]any
	Projects    map[string]map[string]any
	Users       map[string]map[string]any
	Credentials map[string]map[string]any   // metadata only, as n8n 2.x lists them
	Executions  map[string][]map[string]any // workflow id -> executions
	Activated   []string

	// AcceptsPinData makes workflow writes accept description and pinData
	// like n8n 2.x; off, they are refused like n8n 1.x.
	AcceptsPinData bool
	// NoCredentialsEndpoint answers 404 on /credentials like n8n 1.x.
	NoCredentialsEndpoint bool

	// PageSize is the number of items per list page; 0 means everything in
	// one page.
	PageSize int
	// Forbidden maps a collection ("variables", "projects", "users", ...)
	// to the message of the 403 it is answered with. Unlicensed is the one
	// a real instance sends for a feature its license lacks.
	Forbidden map[string]string

	nextID int
}

// New starts a fake instance. Close it with Close.
func New() *Server {
	s := &Server{
		Workflows:   map[string]map[string]any{},
		Tags:        map[string]map[string]any{},
		Variables:   map[string]map[string]any{},
		Projects:    map[string]map[string]any{},
		Users:       map[string]map[string]any{},
		Credentials: map[string]map[string]any{},
		Executions:  map[string][]map[string]any{},
		Forbidden:   map[string]string{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Host returns host:port of the fake, for an n8n:// location.
func (s *Server) Host() string {
	return strings.TrimPrefix(s.URL, "http://")
}

func (s *Server) newID(prefix string) string {
	s.nextID++
	return fmt.Sprintf("%s%04d", prefix, s.nextID)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-N8N-API-KEY") != APIKey {
		fail(w, http.StatusUnauthorized, "'X-N8N-API-KEY' header required")
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	parts := strings.Split(strings.Trim(p, "/"), "/")

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		s.list(w, r, parts[0])
	case parts[0] == "workflows":
		s.workflows(w, r, parts[1:])
	case parts[0] == "tags" && r.Method == http.MethodPost:
		var body map[string]any
		if !decode(w, r, &body) {
			return
		}
		for _, t := range s.Tags {
			if t["name"] == body["name"] {
				fail(w, http.StatusConflict, "Tag already exists")
				return
			}
		}
		body["id"] = s.newID("tag")
		s.Tags[body["id"].(string)] = body
		reply(w, http.StatusCreated, body)
	case parts[0] == "variables" && r.Method == http.MethodPost:
		var body map[string]any
		if !decode(w, r, &body) {
			return
		}
		body["id"] = s.newID("var")
		s.Variables[body["id"].(string)] = body
		w.WriteHeader(http.StatusCreated)
	case parts[0] == "projects" && r.Method == http.MethodPost:
		var body map[string]any
		if !decode(w, r, &body) {
			return
		}
		body["id"] = s.newID("prj")
		body["type"] = "team"
		s.Projects[body["id"].(string)] = body
		w.WriteHeader(http.StatusCreated)
	case parts[0] == "credentials" && r.Method == http.MethodPost:
		var body map[string]any
		if !decode(w, r, &body) {
			return
		}
		body["id"] = s.newID("cred")
		delete(body, "data")
		s.Credentials[body["id"].(string)] = body
		reply(w, http.StatusOK, body)
	default:
		fail(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, collection string) {
	if msg, ok := s.Forbidden[collection]; ok {
		fail(w, http.StatusForbidden, msg)
		return
	}
	var items []map[string]any
	switch collection {
	case "workflows":
		items = s.sorted(s.Workflows)
		if r.URL.Query().Get("excludePinnedData") == "true" {
			stripped := make([]map[string]any, 0, len(items))
			for _, it := range items {
				c := map[string]any{}
				for k, v := range it {
					if k != "pinData" {
						c[k] = v
					}
				}
				stripped = append(stripped, c)
			}
			items = stripped
		}
	case "tags":
		items = s.sorted(s.Tags)
	case "variables":
		items = s.sorted(s.Variables)
	case "projects":
		items = s.sorted(s.Projects)
	case "users":
		items = s.sorted(s.Users)
	case "credentials":
		if s.NoCredentialsEndpoint {
			fail(w, http.StatusNotFound, "not found")
			return
		}
		items = s.sorted(s.Credentials)
	case "executions":
		wid := r.URL.Query().Get("workflowId")
		for _, e := range s.Executions[wid] {
			if r.URL.Query().Get("includeData") != "true" {
				c := map[string]any{}
				for k, v := range e {
					if k != "data" {
						c[k] = v
					}
				}
				e = c
			}
			items = append(items, e)
		}
	default:
		fail(w, http.StatusNotFound, "not found")
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if s.PageSize > 0 && (limit == 0 || limit > s.PageSize) {
		limit = s.PageSize
	}
	start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	if start > len(items) {
		start = len(items)
	}
	page := items[start:]
	var next any
	if limit > 0 && len(page) > limit {
		page = page[:limit]
		next = strconv.Itoa(start + limit)
	}
	if page == nil {
		page = []map[string]any{}
	}
	reply(w, http.StatusOK, map[string]any{"data": page, "nextCursor": next})
}

func (s *Server) workflows(w http.ResponseWriter, r *http.Request, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodPost:
		var body map[string]any
		if !decode(w, r, &body) {
			return
		}
		if !s.validWorkflow(w, body) {
			return
		}
		body["id"] = s.newID("wf")
		body["active"] = false
		body["tags"] = []any{}
		s.Workflows[body["id"].(string)] = body
		reply(w, http.StatusOK, body)
	case len(rest) == 1 && r.Method == http.MethodGet:
		wf, ok := s.Workflows[rest[0]]
		if !ok {
			fail(w, http.StatusNotFound, "Could not find workflow with id: "+rest[0])
			return
		}
		reply(w, http.StatusOK, wf)
	case len(rest) == 1 && r.Method == http.MethodPut:
		wf, ok := s.Workflows[rest[0]]
		if !ok {
			fail(w, http.StatusNotFound, "Could not find workflow with id: "+rest[0])
			return
		}
		var body map[string]any
		if !decode(w, r, &body) {
			return
		}
		if !s.validWorkflow(w, body) {
			return
		}
		for k, v := range body {
			wf[k] = v
		}
		reply(w, http.StatusOK, wf)
	case len(rest) == 2 && rest[1] == "activate" && r.Method == http.MethodPost:
		wf, ok := s.Workflows[rest[0]]
		if !ok {
			fail(w, http.StatusNotFound, "Could not find workflow with id: "+rest[0])
			return
		}
		wf["active"] = true
		s.Activated = append(s.Activated, rest[0])
		reply(w, http.StatusOK, wf)
	case len(rest) == 2 && rest[1] == "tags" && r.Method == http.MethodPut:
		wf, ok := s.Workflows[rest[0]]
		if !ok {
			fail(w, http.StatusNotFound, "Could not find workflow with id: "+rest[0])
			return
		}
		var body []map[string]string
		if !decode(w, r, &body) {
			return
		}
		tags := make([]any, 0, len(body))
		for _, t := range body {
			tag, ok := s.Tags[t["id"]]
			if !ok {
				fail(w, http.StatusNotFound, "Some tags not found")
				return
			}
			tags = append(tags, tag)
		}
		wf["tags"] = tags
		reply(w, http.StatusOK, tags)
	default:
		fail(w, http.StatusNotFound, "not found")
	}
}

// validWorkflow enforces the public schema: required fields present and no
// read-only or unknown ones.
func (s *Server) validWorkflow(w http.ResponseWriter, body map[string]any) bool {
	for _, k := range []string{"name", "nodes", "connections", "settings"} {
		if _, ok := body[k]; !ok {
			fail(w, http.StatusBadRequest, "request/body must have required property '"+k+"'")
			return false
		}
	}
	for k := range body {
		switch k {
		case "name", "nodes", "connections", "settings", "staticData":
		case "description", "pinData":
			if !s.AcceptsPinData {
				fail(w, http.StatusBadRequest, "request/body must NOT have additional properties")
				return false
			}
		default:
			fail(w, http.StatusBadRequest, "request/body/"+k+" is read-only")
			return false
		}
	}
	return true
}

func (s *Server) sorted(m map[string]map[string]any) []map[string]any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]string{"message": msg})
}
