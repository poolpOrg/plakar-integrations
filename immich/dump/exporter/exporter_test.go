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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/poolpOrg/plakar-integrations/immich/api"
	"github.com/poolpOrg/plakar-integrations/immich/dump"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	dumpName = "immich-db-backup-20261112T101500-v3.4.0-pg14.19.sql.gz"
	ownDump  = "immich-db-backup-20261111T020000-v3.4.0-pg14.19.sql.gz"
	token    = "eyJ.maintenance.token"
)

// fakeServer plays an Immich server and its maintenance worker.  After a
// restore is started it walks the script of statuses below, dropping the
// connection on a gap entry.
type fakeServer struct {
	t *testing.T

	mu          sync.Mutex
	initialized bool
	version     string
	ownDumps    []string
	inWorker    bool // the maintenance worker answers, the API key is no good
	loggedIn    bool

	script  []step
	pos     int
	started bool

	uploads     map[string][]byte
	restored    []string
	logins      []string
	jobCommands []string
}

func newFakeServer(t *testing.T) (*fakeServer, *httptest.Server) {
	f := &fakeServer{
		t:           t,
		initialized: true,
		version:     "v3.4.0",
		ownDumps:    []string{ownDump},
		uploads:     make(map[string][]byte),
		script: []step{
			inactive,
			gap,
			active(api.ActionRestoreDatabase, "restore", 0.5),
			active(api.ActionEnd, "", 0),
			gap,
			inactive,
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeServer) authed(r *http.Request) bool {
	if f.inWorker {
		c, err := r.Cookie(api.MaintenanceCookie)
		return err == nil && c.Value == "cookie-for-"+token
	}
	return r.Header.Get("X-Api-Key") == "admin-key"
}

func (f *fakeServer) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	assert.NoError(f.t, json.NewEncoder(w).Encode(v))
}

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	p := strings.TrimPrefix(r.URL.Path, "/api")
	switch {
	case r.Method == http.MethodGet && p == "/server/config":
		f.writeJSON(w, map[string]any{"isInitialized": f.initialized, "maintenanceMode": f.inWorker})

	case r.Method == http.MethodGet && p == "/server/version":
		var major, minor, patch int
		assert.Equal(f.t, 3, parseVersion(f.version, &major, &minor, &patch))
		f.writeJSON(w, map[string]any{"major": major, "minor": minor, "patch": patch})

	case r.Method == http.MethodGet && p == "/server/about":
		if f.inWorker {
			http.NotFound(w, r)
			return
		}
		if !f.authed(r) {
			http.Error(w, `{"message":"Authentication required"}`, http.StatusUnauthorized)
			return
		}
		f.writeJSON(w, map[string]any{"version": f.version})

	case r.Method == http.MethodGet && p == "/admin/maintenance/status":
		if !f.started {
			if f.inWorker {
				f.writeJSON(w, map[string]any{"active": true, "action": api.ActionSelectDatabaseRestore})
			} else {
				f.writeJSON(w, map[string]any{"active": false, "action": api.ActionEnd})
			}
			return
		}
		if !assert.Less(f.t, f.pos, len(f.script), "polled past the end of the script") {
			http.Error(w, "script exhausted", http.StatusTeapot)
			return
		}
		s := f.script[f.pos]
		f.pos++
		if s.err != nil {
			hj, ok := w.(http.Hijacker)
			if !assert.True(f.t, ok) {
				return
			}
			conn, _, err := hj.Hijack()
			if !assert.NoError(f.t, err) {
				return
			}
			_ = conn.Close()
			return
		}
		f.writeJSON(w, s.st)

	case r.Method == http.MethodPost && p == "/admin/maintenance/login":
		var in struct {
			Token string `json:"token"`
		}
		assert.NoError(f.t, json.NewDecoder(r.Body).Decode(&in))
		f.logins = append(f.logins, in.Token)
		if !f.inWorker || in.Token != token {
			http.Error(w, `{"message":"Invalid JWT Token"}`, http.StatusUnauthorized)
			return
		}
		f.loggedIn = true
		http.SetCookie(w, &http.Cookie{Name: api.MaintenanceCookie, Value: "cookie-for-" + token, Path: "/"})
		f.writeJSON(w, map[string]any{"username": "immich-admin"})

	case !f.authed(r):
		http.Error(w, `{"message":"Missing JWT Token"}`, http.StatusUnauthorized)

	case r.Method == http.MethodGet && p == "/admin/database-backups":
		var backups []api.Backup
		for _, d := range f.ownDumps {
			backups = append(backups, api.Backup{Filename: d, Filesize: 3, Timezone: "Europe/Paris"})
		}
		for name, b := range f.uploads {
			backups = append(backups, api.Backup{Filename: name, Filesize: int64(len(b))})
		}
		f.writeJSON(w, map[string]any{"backups": backups})

	case r.Method == http.MethodPost && p == "/admin/database-backups/upload":
		file, hdr, err := r.FormFile("file")
		if !assert.NoError(f.t, err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b, err := io.ReadAll(file)
		if !assert.NoError(f.t, err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.uploads[dump.UploadedPrefix+hdr.Filename] = b
		w.WriteHeader(http.StatusCreated)

	case r.Method == http.MethodPost && p == "/admin/maintenance":
		var in struct {
			Action                string `json:"action"`
			RestoreBackupFilename string `json:"restoreBackupFilename"`
		}
		assert.NoError(f.t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(f.t, api.ActionRestoreDatabase, in.Action)
		if _, ok := f.uploads[in.RestoreBackupFilename]; !ok {
			http.Error(w, `{"message":"ENOENT"}`, http.StatusBadRequest)
			return
		}
		f.restored = append(f.restored, in.RestoreBackupFilename)
		f.started = true
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodPut && strings.HasPrefix(p, "/jobs/"):
		var in struct {
			Command string `json:"command"`
			Force   bool   `json:"force"`
		}
		assert.NoError(f.t, json.NewDecoder(r.Body).Decode(&in))
		f.jobCommands = append(f.jobCommands, strings.TrimPrefix(p, "/jobs/")+":"+in.Command+":"+boolString(in.Force))
		f.writeJSON(w, map[string]any{"queueStatus": map[string]any{"isActive": true}})

	default:
		http.NotFound(w, r)
	}
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// parseVersion splits v3.4.0 into its numbers and says how many it found.
func parseVersion(v string, major, minor, patch *int) int {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	n := 0
	for i, dst := range []*int{major, minor, patch} {
		if i >= len(parts) {
			break
		}
		x := 0
		for _, c := range parts[i] {
			if c < '0' || c > '9' {
				break
			}
			x = x*10 + int(c-'0')
		}
		*dst = x
		n++
	}
	return n
}

func newTestExporter(t *testing.T, srv *httptest.Server, config map[string]string) *Exporter {
	t.Setenv("PLAKAR_IMMICH_API_KEY", "admin-key")
	t.Setenv(dump.EnvMaintenanceToken, "")
	cfg := map[string]string{"location": "immich+dump://" + srv.Listener.Addr().String()}
	for k, v := range config {
		cfg[k] = v
	}
	exp, err := NewExporter(context.Background(), &connectors.Options{MaxConcurrency: 2}, "immich+dump", cfg)
	require.NoError(t, err)
	e := exp.(*Exporter)
	e.interval = time.Millisecond
	e.log = io.Discard
	t.Cleanup(func() { require.NoError(t, e.Close(context.Background())) })
	return e
}

func fileRecord(p, content string) *connectors.Record {
	fi := objects.FileInfo{Lname: filepath.Base(p), Lsize: int64(len(content)), Lmode: 0o644, LmodTime: time.Unix(1700000000, 0), Lnlink: 1}
	r := connectors.NewRecord(p, "", fi, nil, func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(content)), nil
	})
	r.Reader = io.NopCloser(strings.NewReader(content))
	return r
}

func dirRecord(p string) *connectors.Record {
	name := filepath.Base(p)
	if p == "/" {
		name = "/"
	}
	fi := objects.FileInfo{Lname: name, Lmode: os.ModeDir | 0o755, LmodTime: time.Unix(1700000000, 0), Lnlink: 1}
	return connectors.NewRecord(p, "", fi, nil, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(nil)), nil
	})
}

// run pushes the records through Export and returns the results by path.
func run(t *testing.T, e *Exporter, recs ...*connectors.Record) (map[string]error, error) {
	records := make(chan *connectors.Record)
	results := make(chan *connectors.Result, 64)
	errc := make(chan error, 1)
	go func() {
		errc <- e.Export(context.Background(), nil, records, results)
	}()
	for _, r := range recs {
		records <- r
	}
	close(records)

	got := make(map[string]error)
	for res := range results {
		key := res.Record.Pathname
		if res.Record.IsXattr {
			key += "@" + res.Record.XattrName
		}
		_, dup := got[key]
		require.False(t, dup, "two results for %s", key)
		got[key] = res.Err
	}
	return got, <-errc
}

func TestRestoreOnConfiguredServer(t *testing.T) {
	f, srv := newFakeServer(t)
	e := newTestExporter(t, srv, nil)

	got, err := run(t, e, dirRecord("/"), dirRecord("/backups"), fileRecord("/backups/"+dumpName, "dump bytes"))
	require.NoError(t, err)
	require.Len(t, got, 3)
	for p, err := range got {
		require.NoError(t, err, p)
	}

	require.Equal(t, "dump bytes", string(f.uploads[dump.UploadedPrefix+dumpName]))
	require.Equal(t, []string{dump.UploadedPrefix + dumpName}, f.restored)
	require.Equal(t, len(f.script), f.pos, "every status of the script was polled")
	require.Empty(t, f.logins)
	require.Empty(t, f.jobCommands, "no files, no thumbnails to make")
	require.Equal(t, dumpName, e.restored)
}

func TestRestoreOnFreshServerWithToken(t *testing.T) {
	f, srv := newFakeServer(t)
	f.initialized = false
	f.inWorker = true
	f.ownDumps = nil
	f.script = []step{
		active(api.ActionRestoreDatabase, "ready", 0),
		active(api.ActionRestoreDatabase, "restore", 0.9),
		gap,
		inactive,
	}
	e := newTestExporter(t, srv, map[string]string{"maintenance_token": token})

	got, err := run(t, e, fileRecord("/backups/"+dumpName, "dump bytes"))
	require.NoError(t, err)
	require.NoError(t, got["/backups/"+dumpName])
	require.Equal(t, []string{token}, f.logins)
	require.Equal(t, []string{dump.UploadedPrefix + dumpName}, f.restored)
	require.Equal(t, "dump bytes", string(f.uploads[dump.UploadedPrefix+dumpName]))
}

func TestRestoreRefusesFreshServerWithoutToken(t *testing.T) {
	f, srv := newFakeServer(t)
	f.initialized = false
	e := newTestExporter(t, srv, nil)

	got, err := run(t, e, fileRecord("/backups/"+dumpName, "dump bytes"))
	require.NoError(t, err)
	require.ErrorContains(t, got["/backups/"+dumpName], "has no admin user yet and is not in maintenance mode")
	require.ErrorContains(t, got["/backups/"+dumpName], dump.EnvMaintenanceToken)
	require.Empty(t, f.uploads)
	require.Empty(t, f.restored)
}

func TestRestoreRefusesMaintenanceWithoutToken(t *testing.T) {
	f, srv := newFakeServer(t)
	f.initialized = false
	f.inWorker = true
	e := newTestExporter(t, srv, nil)

	got, err := run(t, e, fileRecord("/backups/"+dumpName, "dump bytes"))
	require.NoError(t, err)
	require.ErrorContains(t, got["/backups/"+dumpName], "in maintenance mode (select_database_restore)")
	require.Empty(t, f.uploads)
}

func TestRestoreRefusesBadToken(t *testing.T) {
	f, srv := newFakeServer(t)
	f.initialized = false
	f.inWorker = true
	e := newTestExporter(t, srv, map[string]string{"maintenance_token": "stale"})

	got, err := run(t, e, fileRecord("/backups/"+dumpName, "dump bytes"))
	require.NoError(t, err)
	require.ErrorContains(t, got["/backups/"+dumpName], "maintenance login: POST /admin/maintenance/login: HTTP 401: Invalid JWT Token")
	require.Equal(t, []string{"stale"}, f.logins)
	require.Empty(t, f.uploads)
}

func TestRestoreVersionCheck(t *testing.T) {
	tests := []struct {
		name    string
		version string
		own     []string
		file    string
		force   bool
		err     string
	}{
		{name: "immich mismatch", version: "v3.5.0", file: dumpName, err: "made by Immich v3.4.0, the server runs v3.5.0; pass force=true"},
		{name: "immich mismatch forced", version: "v3.5.0", file: dumpName, force: true},
		{name: "postgres mismatch", own: []string{"immich-db-backup-20261111T020000-v3.4.0-pg16.4.sql.gz"}, file: dumpName, err: "made by Postgres 14.19, the server's own dumps were made by Postgres 16.4"},
		{name: "postgres unknown", own: []string{}, file: dumpName},
		{name: "uploaded dumps do not count", own: []string{"uploaded-immich-db-backup-20261111T020000-v3.4.0-pg16.4.sql.gz"}, file: dumpName},
		{name: "no versions in the name", file: "custom.sql.gz", err: "does not carry the Immich and Postgres versions; pass force=true"},
		{name: "no versions in the name forced", file: "custom.sql.gz", force: true},
		{name: "not a dump", file: "notes.txt", err: "not a database dump"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, srv := newFakeServer(t)
			if tt.version != "" {
				f.version = tt.version
			}
			if tt.own != nil {
				f.ownDumps = tt.own
			}
			cfg := map[string]string{}
			if tt.force {
				cfg["force"] = "true"
			}
			e := newTestExporter(t, srv, cfg)

			got, err := run(t, e, fileRecord("/backups/"+tt.file, "dump bytes"))
			require.NoError(t, err)
			if tt.err != "" {
				require.ErrorContains(t, got["/backups/"+tt.file], tt.err)
				require.Empty(t, f.restored)
				return
			}
			require.NoError(t, got["/backups/"+tt.file])
			require.Equal(t, []string{dump.UploadedPrefix + tt.file}, f.restored)
		})
	}
}

func TestRestoreDumpAtTheRoot(t *testing.T) {
	f, srv := newFakeServer(t)
	e := newTestExporter(t, srv, nil)

	got, err := run(t, e, dirRecord("/"), fileRecord("/"+dumpName, "dump bytes"))
	require.NoError(t, err)
	require.NoError(t, got["/"])
	require.NoError(t, got["/"+dumpName])
	require.Equal(t, []string{dump.UploadedPrefix + dumpName}, f.restored)
}

func TestRestoreOneDumpPerRun(t *testing.T) {
	f, srv := newFakeServer(t)
	e := newTestExporter(t, srv, nil)
	second := "immich-db-backup-20261113T101500-v3.4.0-pg14.19.sql.gz"

	got, err := run(t, e, fileRecord("/backups/"+dumpName, "one"), fileRecord("/backups/"+second, "two"))
	require.NoError(t, err)
	require.NoError(t, got["/backups/"+dumpName])
	require.ErrorContains(t, got["/backups/"+second], dumpName+" was already restored in this run")
	require.Equal(t, []string{dump.UploadedPrefix + dumpName}, f.restored)
}

func TestRestoreReportsWorkerError(t *testing.T) {
	f, srv := newFakeServer(t)
	f.script = []step{
		gap,
		{st: &api.MaintenanceStatus{Active: true, Action: api.ActionRestoreDatabase, Task: api.TaskError, Error: "pg_restore failed"}},
	}
	e := newTestExporter(t, srv, nil)

	got, err := run(t, e, fileRecord("/backups/"+dumpName, "dump bytes"))
	require.NoError(t, err)
	require.ErrorContains(t, got["/backups/"+dumpName], "restore of uploaded-"+dumpName+": the server reported an error: pg_restore failed")
	require.Empty(t, e.restored)
}

func TestRestoreFilesThroughTheFSExporter(t *testing.T) {
	f, srv := newFakeServer(t)
	target := filepath.Join(t.TempDir(), "library")
	e := newTestExporter(t, srv, map[string]string{"upload_location": target})

	xattr := connectors.NewXattr("/upload_location/upload/u1/photo.jpg", "user.comment", objects.AttributeExtended, func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("x")), nil
	})
	got, err := run(t, e,
		dirRecord("/"), dirRecord("/backups"), fileRecord("/backups/"+dumpName, "dump bytes"),
		dirRecord("/upload_location"), dirRecord("/upload_location/upload"), dirRecord("/upload_location/upload/u1"),
		fileRecord("/upload_location/upload/u1/photo.jpg", "original"),
		fileRecord("/upload_location/upload/u1/photo.jpg.xmp", "sidecar"),
		xattr,
		dirRecord("/upload_location/profile"),
		fileRecord("/upload_location/profile/avatar.jpg", "avatar"),
	)
	require.NoError(t, err)
	for p, err := range got {
		require.NoError(t, err, p)
	}
	for _, p := range []string{
		"/", "/backups", "/backups/" + dumpName,
		"/upload_location", "/upload_location/upload", "/upload_location/upload/u1",
		"/upload_location/upload/u1/photo.jpg", "/upload_location/upload/u1/photo.jpg.xmp",
		"/upload_location/upload/u1/photo.jpg@user.comment",
		"/upload_location/profile", "/upload_location/profile/avatar.jpg",
	} {
		require.Contains(t, got, p, "every record gets its result back under the snapshot path")
	}

	b, err := os.ReadFile(filepath.Join(target, "upload", "u1", "photo.jpg"))
	require.NoError(t, err)
	require.Equal(t, "original", string(b))
	b, err = os.ReadFile(filepath.Join(target, "profile", "avatar.jpg"))
	require.NoError(t, err)
	require.Equal(t, "avatar", string(b))
	_, err = os.Stat(filepath.Join(target, "backups"))
	require.ErrorIs(t, err, os.ErrNotExist, "the dump is not written as a file")

	require.Equal(t, []string{dump.UploadedPrefix + dumpName}, f.restored)
	require.Equal(t, []string{api.QueueThumbnailGeneration + ":start:true"}, f.jobCommands)
}

func TestFilesWithoutUploadLocation(t *testing.T) {
	f, srv := newFakeServer(t)
	e := newTestExporter(t, srv, nil)

	got, err := run(t, e, dirRecord("/upload_location"), fileRecord("/upload_location/profile/avatar.jpg", "avatar"))
	require.NoError(t, err)
	require.ErrorContains(t, got["/upload_location"], "no upload_location configured")
	require.ErrorContains(t, got["/upload_location/profile/avatar.jpg"], "no upload_location configured")
	require.Empty(t, f.jobCommands)
}

func TestForeignRecords(t *testing.T) {
	_, srv := newFakeServer(t)
	e := newTestExporter(t, srv, nil)

	got, err := run(t, e, fileRecord("/manifest.json", "{}"), connectors.NewError("/backups/x", io.ErrUnexpectedEOF))
	require.NoError(t, err)
	require.ErrorContains(t, got["/manifest.json"], "not part of an immich+dump snapshot")
	require.NoError(t, got["/backups/x"], "a record the reader could not produce is acknowledged, not restored")
}

func TestNewExporterConfig(t *testing.T) {
	t.Setenv("PLAKAR_IMMICH_API_KEY", "admin-key")
	loc := "immich+dump://127.0.0.1:2283"

	_, err := NewExporter(context.Background(), nil, "immich+dump", map[string]string{"location": loc, "force": "sometimes"})
	require.ErrorContains(t, err, "invalid value for force")
	_, err = NewExporter(context.Background(), nil, "immich+dump", map[string]string{"location": loc, "restore_timeout": "-1s"})
	require.ErrorContains(t, err, "invalid value for restore_timeout")

	target := filepath.Join(t.TempDir(), "new", "library")
	exp, err := NewExporter(context.Background(), nil, "immich+dump", map[string]string{"location": loc, "upload_location": target, "restore_timeout": "5m", "maintenance_token": "cfg"})
	require.NoError(t, err)
	e := exp.(*Exporter)
	require.Equal(t, 5*time.Minute, e.timeout)
	require.Equal(t, "cfg", e.token)
	require.NotNil(t, e.fs)
	require.DirExists(t, target, "the fs exporter creates the target")
	require.Equal(t, "immich+dump", e.Type())
	require.NoError(t, e.Close(context.Background()))

	t.Setenv(dump.EnvMaintenanceToken, "env")
	exp, err = NewExporter(context.Background(), nil, "immich+dump", map[string]string{"location": loc, "maintenance_token": "cfg"})
	require.NoError(t, err)
	require.Equal(t, "env", exp.(*Exporter).token, "the environment wins")
}
