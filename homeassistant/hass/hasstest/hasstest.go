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

// Package hasstest is a fake Home Assistant backup API for tests: the
// websocket commands and HTTP endpoints the integration uses, and a
// builder for backup archives.
package hasstest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/poolpOrg/plakar-integrations/homeassistant/hass"
)

// Fixture describes a backup archive to build.
type Fixture struct {
	Slug       string
	Name       string
	Date       string
	Compressed bool
	Protected  bool
	Addons     []string
	Folders    []string

	// Members maps an inner tar stem to its entries: a path ending
	// with "/" is a directory, a content starting with "-> " a symlink.
	Members map[string]map[string]string
	// Blobs are inner tars stored as is, encrypted ones in practice.
	Blobs map[string][]byte
}

// Metadata renders the backup.json of the fixture.
func (f Fixture) Metadata(t testing.TB) []byte {
	t.Helper()
	addons := []map[string]any{}
	for _, a := range f.Addons {
		addons = append(addons, map[string]any{"slug": a, "name": a, "version": "1.0", "size": 1.0})
	}
	folders := f.Folders
	if folders == nil {
		folders = []string{}
	}
	meta := map[string]any{
		"version":       2,
		"slug":          f.Slug,
		"name":          f.Name,
		"date":          f.Date,
		"type":          "partial",
		"protected":     f.Protected,
		"compressed":    f.Compressed,
		"homeassistant": map[string]any{"version": "2025.10.1", "exclude_database": false},
		"addons":        addons,
		"folders":       folders,
		"extra":         map[string]any{},
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Build renders the fixture as a backup tar.
func (f Fixture) Build(t testing.TB) []byte {
	t.Helper()

	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	when := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	write := func(name string, data []byte) {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "./" + name, Mode: 0644, Size: int64(len(data)), ModTime: when}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}

	write("backup.json", f.Metadata(t))

	stems := make([]string, 0, len(f.Members))
	for stem := range f.Members {
		stems = append(stems, stem)
	}
	sort.Strings(stems)
	for _, stem := range stems {
		name := stem + ".tar"
		if f.Compressed {
			name += ".gz"
		}
		write(name, InnerTar(t, f.Members[stem], f.Compressed))
	}

	names := make([]string, 0, len(f.Blobs))
	for name := range f.Blobs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		write(name, f.Blobs[name])
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// InnerTar renders one inner tar, gzipped when asked.
func InnerTar(t testing.TB, entries map[string]string, gz bool) []byte {
	t.Helper()

	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	when := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)

	paths := make([]string, 0, len(entries))
	for p := range entries {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		content := entries[p]
		hdr := &tar.Header{Name: "./" + p, Mode: 0644, ModTime: when, Uid: 1000, Gid: 1000}
		switch {
		case strings.HasSuffix(p, "/"):
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0755
		case strings.HasPrefix(content, "-> "):
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = strings.TrimPrefix(content, "-> ")
			hdr.Mode = 0777
			content = ""
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.WriteString(tw, content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if !gz {
		return raw.Bytes()
	}

	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// Entry is a backup the fake instance holds.
type Entry struct {
	Backup   hass.Backup
	Data     []byte // what a download returns, decrypted
	Password string // set when the backup is protected
}

// Upload is a backup the fake instance received.
type Upload struct {
	Agent    string
	Filename string
	Data     []byte
}

// Server is the fake instance.
type Server struct {
	*httptest.Server

	Token  string
	Agents []string

	// Generate makes the backup for a backup/generate command; it
	// gets the name requested and returns the entry and its job id.
	Generate func(name string, cmd map[string]any) (*Entry, string)
	// DropOnRestore closes the websocket instead of answering a
	// backup/restore, like a restarting instance does.
	DropOnRestore bool

	mu       sync.Mutex
	backups  map[string]*Entry
	Uploads  []Upload
	Restores []map[string]any
	Deleted  []string
	Commands []string
}

// New starts a fake instance accepting token.
func New(t testing.TB, token string) *Server {
	t.Helper()

	s := &Server{
		Token:   token,
		Agents:  []string{"backup.local"},
		backups: make(map[string]*Entry),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", s.handlePing)
	mux.HandleFunc("/api/websocket", s.handleWebsocket)
	mux.HandleFunc("/api/backup/download/", s.handleDownload)
	mux.HandleFunc("/api/backup/upload", s.handleUpload)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// Location is the plakar location of the fake instance.
func (s *Server) Location() string {
	return "homeassistant://" + strings.TrimPrefix(s.URL, "http://")
}

// Add registers a backup on the instance.
func (s *Server) Add(e *Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Backup.Agents == nil {
		e.Backup.Agents = map[string]hass.AgentState{}
		for _, a := range s.Agents {
			e.Backup.Agents[a] = hass.AgentState{Protected: e.Password != "", Size: int64(len(e.Data))}
		}
	}
	s.backups[e.Backup.ID] = e
}

// Has reports whether the instance holds a backup.
func (s *Server) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.backups[id]
	return ok
}

func (s *Server) authorized(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+s.Token
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "401: Unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"message": "API running."}`)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "401: Unauthorized", http.StatusUnauthorized)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/backup/download/")
	agent := r.URL.Query().Get("agent_id")

	s.mu.Lock()
	e, ok := s.backups[id]
	s.mu.Unlock()

	switch {
	case agent == "":
		http.Error(w, "agent_id missing", http.StatusBadRequest)
	case !ok:
		http.Error(w, "backup not found", http.StatusNotFound)
	case e.Password != "" && r.URL.Query().Get("password") != e.Password:
		http.Error(w, "password incorrect", http.StatusBadRequest)
	default:
		w.Header().Set("Content-Type", "application/x-tar")
		w.Write(e.Data)
	}
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "401: Unauthorized", http.StatusUnauthorized)
		return
	}
	agent := r.URL.Query().Get("agent_id")
	if agent == "" {
		http.Error(w, "agent_id missing", http.StatusBadRequest)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	part, err := mr.NextPart()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(part)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	id := strings.TrimSuffix(part.FileName(), ".tar")
	s.mu.Lock()
	s.Uploads = append(s.Uploads, Upload{Agent: agent, Filename: part.FileName(), Data: data})
	s.backups[id] = &Entry{Backup: hass.Backup{ID: id, Name: id, Agents: map[string]hass.AgentState{agent: {Size: int64(len(data))}}}, Data: data}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"backup_id": id})
}

func (s *Server) handleWebsocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 20)
	ctx := r.Context()

	send := func(v any) bool { return wsjson.Write(ctx, conn, v) == nil }

	if !send(map[string]any{"type": "auth_required", "ha_version": "2025.10.1"}) {
		return
	}
	var auth struct {
		Type  string `json:"type"`
		Token string `json:"access_token"`
	}
	if err := wsjson.Read(ctx, conn, &auth); err != nil {
		return
	}
	if auth.Type != "auth" || auth.Token != s.Token {
		send(map[string]any{"type": "auth_invalid", "message": "Invalid access token or password"})
		return
	}
	if !send(map[string]any{"type": "auth_ok", "ha_version": "2025.10.1"}) {
		return
	}

	subID := 0
	result := func(id int, res any) bool {
		return send(map[string]any{"id": id, "type": "result", "success": true, "result": res})
	}
	fail := func(id int, code, msg string) bool {
		return send(map[string]any{"id": id, "type": "result", "success": false, "error": map[string]string{"code": code, "message": msg}})
	}
	event := func(ev map[string]any) bool {
		if subID == 0 {
			return true
		}
		return send(map[string]any{"id": subID, "type": "event", "event": ev})
	}

	for {
		var cmd map[string]any
		if err := wsjson.Read(ctx, conn, &cmd); err != nil {
			return
		}
		id := int(cmd["id"].(float64))
		kind, _ := cmd["type"].(string)

		s.mu.Lock()
		s.Commands = append(s.Commands, kind)
		s.mu.Unlock()

		switch kind {
		case "backup/agents/info":
			agents := []map[string]string{}
			for _, a := range s.Agents {
				agents = append(agents, map[string]string{"agent_id": a, "name": a})
			}
			result(id, map[string]any{"agents": agents})

		case "backup/info":
			s.mu.Lock()
			backups := []hass.Backup{}
			for _, e := range s.backups {
				backups = append(backups, e.Backup)
			}
			s.mu.Unlock()
			result(id, map[string]any{"backups": backups, "agent_errors": map[string]string{}})

		case "backup/details":
			s.mu.Lock()
			e := s.backups[cmd["backup_id"].(string)]
			s.mu.Unlock()
			var b *hass.Backup
			if e != nil {
				b = &e.Backup
			}
			result(id, map[string]any{"backup": b, "agent_errors": map[string]string{}})

		case "backup/delete":
			bid := cmd["backup_id"].(string)
			s.mu.Lock()
			delete(s.backups, bid)
			s.Deleted = append(s.Deleted, bid)
			s.mu.Unlock()
			result(id, map[string]any{"agent_errors": map[string]string{}})

		case "backup/subscribe_events":
			subID = id
			event(map[string]any{"manager_state": "idle"})
			result(id, nil)

		case "backup/generate":
			if s.Generate == nil {
				fail(id, "unknown_error", "no generator")
				continue
			}
			name, _ := cmd["name"].(string)
			e, job := s.Generate(name, cmd)
			result(id, map[string]any{"backup_job_id": job})
			event(map[string]any{"manager_state": "create_backup", "stage": "home_assistant", "state": "in_progress", "reason": nil})
			if e == nil {
				event(map[string]any{"manager_state": "create_backup", "stage": nil, "state": "failed", "reason": "backup_manager_error"})
			} else {
				s.Add(e)
				event(map[string]any{"manager_state": "create_backup", "stage": nil, "state": "completed", "reason": nil})
			}
			event(map[string]any{"manager_state": "idle"})

		case "backup/restore":
			s.mu.Lock()
			s.Restores = append(s.Restores, cmd)
			s.mu.Unlock()
			if s.DropOnRestore {
				conn.Close(websocket.StatusGoingAway, "restarting")
				return
			}
			result(id, nil)

		default:
			fail(id, "unknown_command", "Unknown command.")
		}
	}
}
