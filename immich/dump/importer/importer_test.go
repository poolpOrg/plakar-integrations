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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/poolpOrg/plakar-integrations/immich/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	oldDump = "immich-db-backup-20261111T020000-v3.4.0-pg14.19.sql.gz"
	newDump = "immich-db-backup-20261112T101500-v3.4.0-pg14.19.sql.gz"
)

// fakeServer is the admin side of Immich: a backups list that grows a few
// polls after the job is queued, and the download of each dump.
type fakeServer struct {
	t *testing.T

	mu       sync.Mutex
	backups  []api.Backup
	content  map[string][]byte
	jobs     []string
	lists    int
	appearAt int // the list call after which the new dump shows up; 0 never
}

func newFakeServer(t *testing.T) (*fakeServer, *httptest.Server) {
	f := &fakeServer{
		t:       t,
		backups: []api.Backup{{Filename: oldDump, Filesize: 3, Timezone: "Europe/Paris"}},
		content: map[string][]byte{oldDump: []byte("old"), newDump: []byte("new dump bytes")},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.Header.Get("X-Api-Key") != "admin-key" {
		http.Error(w, `{"message":"Invalid API key"}`, http.StatusUnauthorized)
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/admin/database-backups":
		f.lists++
		if f.appearAt > 0 && f.lists >= f.appearAt && len(f.jobs) > 0 {
			found := false
			for _, b := range f.backups {
				found = found || b.Filename == newDump
			}
			if !found {
				f.backups = append([]api.Backup{{Filename: newDump, Filesize: int64(len(f.content[newDump])), Timezone: "Europe/Paris"}}, f.backups...)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(f.t, json.NewEncoder(w).Encode(map[string]any{"backups": f.backups}))

	case r.Method == http.MethodPost && r.URL.Path == "/api/jobs":
		var in struct {
			Name string `json:"name"`
		}
		assert.NoError(f.t, json.NewDecoder(r.Body).Decode(&in))
		f.jobs = append(f.jobs, in.Name)
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && len(r.URL.Path) > len("/api/admin/database-backups/"):
		name := r.URL.Path[len("/api/admin/database-backups/"):]
		b, ok := f.content[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(b)

	default:
		http.NotFound(w, r)
	}
}

func newTestImporter(t *testing.T, srv *httptest.Server, config map[string]string) *Importer {
	t.Setenv("PLAKAR_IMMICH_API_KEY", "admin-key")
	cfg := map[string]string{"location": "immich+dump://" + srv.Listener.Addr().String()}
	for k, v := range config {
		cfg[k] = v
	}
	imp, err := NewImporter(context.Background(), &connectors.Options{MaxConcurrency: 2}, "immich+dump", cfg)
	require.NoError(t, err)
	i := imp.(*Importer)
	i.interval = 5 * time.Millisecond
	return i
}

func TestTriggerWaitsForTheNewDump(t *testing.T) {
	f, srv := newFakeServer(t)
	f.appearAt = 4
	i := newTestImporter(t, srv, nil)

	b, err := Trigger(context.Background(), i.client, time.Second, i.interval)
	require.NoError(t, err)
	require.Equal(t, newDump, b.Filename)
	require.Equal(t, int64(len("new dump bytes")), b.Filesize)
	require.Equal(t, []string{api.JobBackupDatabase}, f.jobs)
	require.GreaterOrEqual(t, f.lists, 4)
}

func TestTriggerTimesOut(t *testing.T) {
	f, srv := newFakeServer(t)
	f.appearAt = 0
	i := newTestImporter(t, srv, nil)

	_, err := Trigger(context.Background(), i.client, 30*time.Millisecond, i.interval)
	require.ErrorContains(t, err, "produced no dump within 30ms")
	require.Equal(t, []string{api.JobBackupDatabase}, f.jobs)
}

func TestTriggerStopsOnCancel(t *testing.T) {
	f, srv := newFakeServer(t)
	f.appearAt = 0
	i := newTestImporter(t, srv, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := Trigger(ctx, i.client, time.Minute, i.interval)
	require.ErrorIs(t, err, context.Canceled)
}

func TestTriggerRefusesWithoutAdmin(t *testing.T) {
	_, srv := newFakeServer(t)
	t.Setenv("PLAKAR_IMMICH_API_KEY", "user-key")
	imp, err := NewImporter(context.Background(), nil, "immich+dump", map[string]string{"location": "immich+dump://" + srv.Listener.Addr().String()})
	require.NoError(t, err)
	i := imp.(*Importer)

	require.ErrorContains(t, i.Ping(context.Background()), "HTTP 401")
	_, err = Trigger(context.Background(), i.client, time.Second, time.Millisecond)
	require.ErrorContains(t, err, "list dumps")
}

func TestNewImporterConfig(t *testing.T) {
	t.Setenv("PLAKAR_IMMICH_API_KEY", "admin-key")
	loc := "immich+dump://127.0.0.1:2283"

	_, err := NewImporter(context.Background(), nil, "immich+dump", map[string]string{"location": loc, "dump_timeout": "never"})
	require.ErrorContains(t, err, "invalid value for dump_timeout")

	_, err = NewImporter(context.Background(), nil, "immich+dump", map[string]string{"location": loc, "upload_location": filepath.Join(t.TempDir(), "missing")})
	require.ErrorContains(t, err, "upload_location")

	_, err = NewImporter(context.Background(), nil, "immich+dump", map[string]string{"location": "immich://127.0.0.1:2283"})
	require.NoError(t, err, "the scheme is the plugin's business, both parse")

	imp, err := NewImporter(context.Background(), nil, "immich+dump", map[string]string{"location": loc, "dump_timeout": "1m"})
	require.NoError(t, err)
	i := imp.(*Importer)
	require.Equal(t, time.Minute, i.timeout)
	require.Equal(t, "immich+dump", i.Type())
	require.Equal(t, "/", i.Root())
	require.Equal(t, "127.0.0.1:2283", i.Origin())
}

// collect runs Import and returns the records by pathname, reading the
// regular files through their readers.
func collect(t *testing.T, i *Importer) (map[string]*connectors.Record, map[string][]byte) {
	records := make(chan *connectors.Record, 64)
	errc := make(chan error, 1)
	go func() {
		_, err := i.Import(context.Background(), records, nil)
		errc <- err
	}()

	got := make(map[string]*connectors.Record)
	data := make(map[string][]byte)
	for r := range records {
		require.NoError(t, r.Err, r.Pathname)
		if r.IsXattr {
			continue
		}
		got[r.Pathname] = r
		if r.FileInfo.Lmode.IsRegular() {
			b, err := io.ReadAll(r.Reader)
			require.NoError(t, err, r.Pathname)
			require.NoError(t, r.Reader.Close())
			data[r.Pathname] = b
		}
	}
	require.NoError(t, <-errc)
	return got, data
}

func TestImportDumpOnly(t *testing.T) {
	f, srv := newFakeServer(t)
	f.appearAt = 2
	i := newTestImporter(t, srv, nil)

	got, data := collect(t, i)

	paths := make([]string, 0, len(got))
	for p := range got {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	require.Equal(t, []string{"/", "/backups", "/backups/" + newDump}, paths)

	r := got["/backups/"+newDump]
	require.Equal(t, int64(len("new dump bytes")), r.FileInfo.Lsize)
	require.Equal(t, newDump, r.FileInfo.Lname)
	require.Equal(t, "new dump bytes", string(data["/backups/"+newDump]))
	paris, _ := time.LoadLocation("Europe/Paris")
	require.True(t, r.FileInfo.LmodTime.Equal(time.Date(2026, 11, 12, 10, 15, 0, 0, paris)))
}

func TestImportWithUploadLocation(t *testing.T) {
	f, srv := newFakeServer(t)
	f.appearAt = 1

	root := t.TempDir()
	for _, d := range []string{"upload/u1", "library/u1", "profile", "thumbs/u1", "encoded-video/u1", "backups"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	files := map[string]string{
		"upload/u1/photo.jpg":        "original",
		"upload/u1/photo.jpg.xmp":    "sidecar",
		"library/u1/2024/IMG.HEIC":   "library original",
		"profile/avatar.jpg":         "avatar",
		"thumbs/u1/thumb.webp":       "regenerable",
		"encoded-video/u1/video.mp4": "regenerable",
		"backups/" + oldDump:         "old",
		".immich":                    "",
	}
	for p, c := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, p), []byte(c), 0o644))
	}

	i := newTestImporter(t, srv, map[string]string{"upload_location": root})
	got, data := collect(t, i)

	for _, p := range []string{
		"/", "/backups", "/backups/" + newDump,
		"/upload_location", "/upload_location/upload", "/upload_location/upload/u1",
		"/upload_location/upload/u1/photo.jpg", "/upload_location/upload/u1/photo.jpg.xmp",
		"/upload_location/library/u1/2024/IMG.HEIC", "/upload_location/profile/avatar.jpg",
		"/upload_location/backups/" + oldDump, "/upload_location/.immich",
	} {
		require.Contains(t, got, p)
	}
	for p := range got {
		require.NotContains(t, p, "/thumbs", p)
		require.NotContains(t, p, "/encoded-video", p)
		require.NotEqual(t, filepath.ToSlash(root), p, "the real root must not leak")
	}
	require.Equal(t, "original", string(data["/upload_location/upload/u1/photo.jpg"]))
	require.Equal(t, "library original", string(data["/upload_location/library/u1/2024/IMG.HEIC"]))
	require.Equal(t, "upload_location", got["/upload_location"].FileInfo.Lname)
	require.True(t, got["/upload_location"].FileInfo.Lmode.IsDir())
	require.Equal(t, "u1", got["/upload_location/upload/u1"].FileInfo.Lname)
}

func TestRewrite(t *testing.T) {
	root := "/srv/immich/library"
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/srv/immich/library", "/upload_location", true},
		{"/srv/immich/library/upload/x.jpg", "/upload_location/upload/x.jpg", true},
		{"/srv/immich/library/thumbs", "", false},
		{"/srv/immich/library/thumbs/u/x.webp", "", false},
		{"/srv/immich/library/encoded-video/u/x.mp4", "", false},
		{"/srv/immich/library/thumbsup/x", "/upload_location/thumbsup/x", true},
		{"/srv/immich", "", false},
		{"/", "", false},
		{"/srv/immich/library2/x", "", false},
	}
	for _, tt := range tests {
		got, ok := rewrite(root, tt.in)
		require.Equal(t, tt.ok, ok, tt.in)
		require.Equal(t, tt.want, got, tt.in)
	}
}
