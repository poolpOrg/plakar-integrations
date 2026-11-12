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

package nextcloud

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/stretchr/testify/require"
)

const (
	testLogin = "alice@example.org"
	testUID   = "alice"
	testPass  = "app-password"
)

var (
	t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t1 = t0.Add(time.Hour)
	t2 = t0.Add(2 * time.Hour)
)

type fakeNode struct {
	dir   bool
	data  string
	mtime time.Time
	id    int

	// current marks the version entry Nextcloud lists for the file's
	// present content, which it refuses to serve
	current bool
}

// fakeNextcloud is the part of a Nextcloud server the connector talks to:
// basic auth, the OCS user lookup and WebDAV on the files, trashbin and
// versions trees.
type fakeNextcloud struct {
	mu       sync.Mutex
	prefix   string
	files    map[string]*fakeNode
	trash    map[string]*fakeNode
	versions map[int]map[string]*fakeNode
	failing  map[string]bool
	nextID   int
}

func newFakeNextcloud(prefix string) *fakeNextcloud {
	f := &fakeNextcloud{
		prefix:   prefix,
		files:    map[string]*fakeNode{"/": {dir: true, mtime: t0}},
		trash:    map[string]*fakeNode{"/": {dir: true, mtime: t0}},
		versions: map[int]map[string]*fakeNode{},
		failing:  map[string]bool{},
		nextID:   100,
	}
	return f
}

func (f *fakeNextcloud) addFile(tree map[string]*fakeNode, p, data string, mtime time.Time) *fakeNode {
	f.nextID++
	n := &fakeNode{data: data, mtime: mtime, id: f.nextID}
	tree[p] = n
	return n
}

func (f *fakeNextcloud) addDir(tree map[string]*fakeNode, p string) *fakeNode {
	f.nextID++
	n := &fakeNode{dir: true, mtime: t0, id: f.nextID}
	tree[p] = n
	return n
}

func (f *fakeNextcloud) addVersion(file *fakeNode, name, data string, mtime time.Time) {
	if f.versions[file.id] == nil {
		f.versions[file.id] = map[string]*fakeNode{}
		cur := strconv.FormatInt(file.mtime.Unix(), 10)
		f.versions[file.id][cur] = &fakeNode{data: file.data, mtime: file.mtime, current: true}
	}
	f.versions[file.id][name] = &fakeNode{data: data, mtime: mtime}
}

// route resolves a DAV path to the tree it lives in, the path inside that
// tree, and the href root of the tree.
func (f *fakeNextcloud) route(p string) (map[string]*fakeNode, string, string, bool) {
	for _, kind := range []string{"files", "trashbin", "versions"} {
		root := "/remote.php/dav/" + kind + "/" + testUID
		if kind == "trashbin" {
			root += "/trash"
		}
		rest, ok := strings.CutPrefix(p, root)
		if !ok || (rest != "" && rest[0] != '/') {
			continue
		}
		rest = path.Clean("/" + rest)
		switch kind {
		case "files":
			return f.files, rest, root, true
		case "trashbin":
			return f.trash, rest, root, true
		default:
			rest, ok = strings.CutPrefix(rest, "/versions/")
			if !ok {
				return nil, "", "", false
			}
			idstr, sub, _ := strings.Cut(rest, "/")
			id, err := strconv.Atoi(idstr)
			if err != nil {
				return nil, "", "", false
			}
			tree, ok := f.versions[id]
			if !ok {
				return nil, "", "", false
			}
			full := map[string]*fakeNode{"/": {dir: true, mtime: t0}}
			for name, n := range tree {
				full["/"+name] = n
			}
			return full, path.Clean("/" + sub), root + "/versions/" + idstr, true
		}
	}
	return nil, "", "", false
}

func (f *fakeNextcloud) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if u, p, ok := r.BasicAuth(); !ok || u != testLogin || p != testPass {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	p, ok := strings.CutPrefix(r.URL.Path, f.prefix)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if p == "/ocs/v2.php/cloud/user" {
		if r.Header.Get("Ocs-Apirequest") != "true" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, `{"ocs":{"meta":{"status":"ok"},"data":{"id":%q,"display-name":"Alice"}}}`, testUID)
		return
	}

	tree, rel, root, ok := f.route(p)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	switch r.Method {
	case "PROPFIND":
		f.propfind(w, r, tree, rel, root)
	case http.MethodGet:
		n, ok := tree[rel]
		if !ok || n.dir || n.current {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, n.data)
	case http.MethodPut:
		if tree[path.Dir(rel)] == nil {
			w.WriteHeader(http.StatusConflict)
			return
		}
		if r.ContentLength < 0 {
			http.Error(w, "chunked upload", http.StatusLengthRequired)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		n := f.addFile(tree, rel, string(data), time.Now())
		if ts := r.Header.Get("X-Oc-Mtime"); ts != "" {
			unix, err := strconv.ParseInt(ts, 10, 64)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			n.mtime = time.Unix(unix, 0).UTC()
		}
		w.WriteHeader(http.StatusCreated)
	case "MKCOL":
		if tree[rel] != nil {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if tree[path.Dir(rel)] == nil {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.addDir(tree, rel)
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeNextcloud) propfind(w http.ResponseWriter, r *http.Request, tree map[string]*fakeNode, rel, root string) {
	if f.failing[rel] {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	self, ok := tree[rel]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	type item struct {
		rel string
		n   *fakeNode
	}
	items := []item{{rel, self}}
	if r.Header.Get("Depth") == "1" && self.dir {
		for p, n := range tree {
			if p != "/" && path.Dir(p) == rel {
				items = append(items, item{p, n})
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })

	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns" xmlns:nc="http://nextcloud.org/ns">`)
	for _, it := range items {
		href := &url.URL{Path: f.prefix + path.Join(root, it.rel)}
		if it.n.dir && !strings.HasSuffix(href.Path, "/") {
			href.Path += "/"
		}
		var escaped strings.Builder
		_ = xml.EscapeText(&escaped, []byte(href.EscapedPath()))
		b.WriteString("<d:response><d:href>" + escaped.String() + "</d:href>")
		b.WriteString(`<d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop>`)
		b.WriteString("<d:getlastmodified>" + it.n.mtime.UTC().Format(http.TimeFormat) + "</d:getlastmodified>")
		if it.n.id != 0 {
			fmt.Fprintf(&b, "<oc:fileid>%d</oc:fileid>", it.n.id)
		}
		if it.n.dir {
			b.WriteString("<d:resourcetype><d:collection/></d:resourcetype>")
		} else {
			b.WriteString("<d:resourcetype/>")
			fmt.Fprintf(&b, "<d:getcontentlength>%d</d:getcontentlength>", len(it.n.data))
		}
		b.WriteString(`</d:prop></d:propstat>`)
		b.WriteString(`<d:propstat><d:status>HTTP/1.1 404 Not Found</d:status><d:prop><oc:missing/></d:prop></d:propstat>`)
		b.WriteString("</d:response>")
	}
	b.WriteString("</d:multistatus>")

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, b.String())
}

// populated is a server holding a small tree, a deleted folder and two
// versions of one file.
func populated(t *testing.T, prefix string) (*fakeNextcloud, *httptest.Server) {
	t.Helper()
	f := newFakeNextcloud(prefix)
	f.addFile(f.files, "/alpha", "alpha\n", t1)
	f.addDir(f.files, "/gamma")
	f.addFile(f.files, "/gamma/delta", "delta\n", t2)
	f.addFile(f.files, "/gamma/odd name & #1.txt", "odd\n", t2)
	f.addFile(f.files, "/gamma/what?.txt", "what\n", t2)
	notes := f.addFile(f.files, "/gamma/notes.txt", "v3\n", t2)
	f.addVersion(notes, "1700000000", "v1\n", t0)
	f.addVersion(notes, "1700003600", "v2\n", t1)
	f.addFile(f.files, "/empty", "", t1)

	f.addDir(f.trash, "/old.d1700000000")
	f.addFile(f.trash, "/old.d1700000000/kept", "kept\n", t0)
	f.addFile(f.trash, "/gone.txt.d1700003600", "gone\n", t0)

	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func config(srv *httptest.Server, folder string, extra map[string]string) map[string]string {
	u, _ := url.Parse(srv.URL)
	c := map[string]string{
		"location": "nextcloud://" + u.Host + folder,
		"username": testLogin,
		"password": testPass,
		"insecure": "true",
	}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

func options() *integration.Options {
	return &integration.Options{Hostname: "localhost", MaxConcurrency: 4}
}

type imported struct {
	content string
	mtime   time.Time
	dir     bool
	err     error
}

// runImport drains Import into a map keyed by pathname.
func runImport(t *testing.T, n *Nextcloud) map[string]imported {
	t.Helper()
	records := make(chan *integration.Record)
	errc := make(chan error, 1)
	go func() {
		errc <- n.Import(context.Background(), records, nil)
	}()

	got := map[string]imported{}
	for r := range records {
		if r.Err != nil {
			got[r.Pathname] = imported{err: r.Err}
			continue
		}
		it := imported{mtime: r.FileInfo.ModTime(), dir: r.FileInfo.IsDir()}
		if !it.dir {
			data, err := io.ReadAll(r.Reader)
			require.NoError(t, err, r.Pathname)
			require.NoError(t, r.Reader.Close())
			require.EqualValues(t, len(data), r.FileInfo.Size(), r.Pathname)
			it.content = string(data)
		}
		_, dup := got[r.Pathname]
		require.False(t, dup, "%s reported twice", r.Pathname)
		got[r.Pathname] = it
	}
	require.NoError(t, <-errc)
	return got
}

func TestNew(t *testing.T) {
	cases := []struct {
		name   string
		proto  string
		config map[string]string
		err    string
		root   string
		origin string
	}{
		{
			name:   "defaults",
			proto:  "nextcloud",
			config: map[string]string{"location": "nextcloud://cloud.example.org", "username": "u", "password": "p"},
			root:   "/", origin: "cloud.example.org",
		},
		{
			name:   "folder prefix and insecure",
			proto:  "nextcloud",
			config: map[string]string{"location": "nextcloud://host:8080/Documents/", "username": "u", "password": "p", "prefix": "nextcloud/", "insecure": "true"},
			root:   "/Documents", origin: "host:8080",
		},
		{
			name:   "wrong protocol",
			proto:  "dav",
			config: map[string]string{"location": "dav://host", "username": "u", "password": "p"},
			err:    "unsupported protocol",
		},
		{
			name:   "no host",
			proto:  "nextcloud",
			config: map[string]string{"location": "nextcloud:///x", "username": "u", "password": "p"},
			err:    "want nextcloud://host",
		},
		{
			name:   "missing password",
			proto:  "nextcloud",
			config: map[string]string{"location": "nextcloud://host", "username": "u"},
			err:    "username and password are required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := New(context.Background(), options(), tc.proto, tc.config)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.root, n.Root())
			require.Equal(t, tc.origin, n.Origin())
			require.Equal(t, "nextcloud", n.Type())
		})
	}
}

func TestPing(t *testing.T) {
	_, srv := populated(t, "")

	cases := []struct {
		name   string
		folder string
		extra  map[string]string
		err    string
	}{
		{name: "root", folder: ""},
		{name: "folder", folder: "/gamma"},
		{name: "missing folder", folder: "/nope", err: "404"},
		{name: "bad password", extra: map[string]string{"password": "x"}, err: "401"},
		{name: "wrong prefix", extra: map[string]string{"prefix": "/nc"}, err: "404"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := New(context.Background(), options(), "nextcloud", config(srv, tc.folder, tc.extra))
			require.NoError(t, err)
			err = n.Ping(context.Background())
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestImport(t *testing.T) {
	base := map[string]imported{
		"/files":                         {dir: true},
		"/files/alpha":                   {content: "alpha\n", mtime: t1},
		"/files/empty":                   {content: "", mtime: t1},
		"/files/gamma":                   {dir: true, mtime: t0},
		"/files/gamma/delta":             {content: "delta\n", mtime: t2},
		"/files/gamma/odd name & #1.txt": {content: "odd\n", mtime: t2},
		"/files/gamma/what?.txt":         {content: "what\n", mtime: t2},
		"/files/gamma/notes.txt":         {content: "v3\n", mtime: t2},
	}
	gammaOnly := map[string]imported{
		"/files":                         {dir: true},
		"/files/gamma":                   {dir: true, mtime: t0},
		"/files/gamma/delta":             {content: "delta\n", mtime: t2},
		"/files/gamma/odd name & #1.txt": {content: "odd\n", mtime: t2},
		"/files/gamma/what?.txt":         {content: "what\n", mtime: t2},
		"/files/gamma/notes.txt":         {content: "v3\n", mtime: t2},
	}
	trash := map[string]imported{
		"/trashbin":                      {dir: true},
		"/trashbin/old.d1700000000":      {dir: true, mtime: t0},
		"/trashbin/old.d1700000000/kept": {content: "kept\n", mtime: t0},
		"/trashbin/gone.txt.d1700003600": {content: "gone\n", mtime: t0},
	}
	versions := map[string]imported{
		"/versions":                            {dir: true},
		"/versions/gamma/notes.txt":            {dir: true},
		"/versions/gamma/notes.txt/1700000000": {content: "v1\n", mtime: t0},
		"/versions/gamma/notes.txt/1700003600": {content: "v2\n", mtime: t1},
	}
	merge := func(ms ...map[string]imported) map[string]imported {
		out := map[string]imported{}
		for _, m := range ms {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}

	cases := []struct {
		name   string
		prefix string
		folder string
		extra  map[string]string
		want   map[string]imported
	}{
		{name: "files", want: base},
		{name: "subfolder", folder: "/gamma", want: gammaOnly},
		{name: "install prefix", prefix: "/nc", extra: map[string]string{"prefix": "/nc"}, want: base},
		{name: "trash", extra: map[string]string{"trash": "true"}, want: merge(base, trash)},
		{name: "versions", extra: map[string]string{"versions": "true"}, want: merge(base, versions)},
		{name: "everything", extra: map[string]string{"trash": "true", "versions": "true"}, want: merge(base, trash, versions)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := populated(t, tc.prefix)
			n, err := New(context.Background(), options(), "nextcloud", config(srv, tc.folder, tc.extra))
			require.NoError(t, err)

			got := runImport(t, n)
			for pathname, want := range tc.want {
				g, ok := got[pathname]
				require.True(t, ok, "%s missing, got %v", pathname, keys(got))
				require.NoError(t, g.err, pathname)
				require.Equal(t, want.dir, g.dir, pathname)
				require.Equal(t, want.content, g.content, pathname)
				if !want.mtime.IsZero() {
					require.True(t, want.mtime.Equal(g.mtime), "%s: mtime %s, want %s", pathname, g.mtime, want.mtime)
				}
			}
			require.Len(t, got, len(tc.want), "extra records: %v", keys(got))
		})
	}
}

func TestImportListingError(t *testing.T) {
	f, srv := populated(t, "")
	f.failing["/gamma"] = true

	n, err := New(context.Background(), options(), "nextcloud", config(srv, "", nil))
	require.NoError(t, err)

	got := runImport(t, n)
	require.ErrorContains(t, got["/files/gamma"].err, "500")
	require.NoError(t, got["/files/alpha"].err)
	require.NotContains(t, got, "/files/gamma/delta")

	f.failing["/"] = true
	records := make(chan *integration.Record, 16)
	require.ErrorContains(t, n.Import(context.Background(), records, nil), "500")
}

func TestImportCancel(t *testing.T) {
	_, srv := populated(t, "")
	n, err := New(context.Background(), options(), "nextcloud", config(srv, "", map[string]string{"trash": "true", "versions": "true"}))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	records := make(chan *integration.Record)
	done := make(chan error, 1)
	go func() { done <- n.Import(ctx, records, nil) }()

	<-records
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func keys(m map[string]imported) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fileRecord(pathname, content string, mtime time.Time) *integration.Record {
	fi := integration.FileInfo{Lname: path.Base(pathname), Lsize: int64(len(content)), Lmode: 0644, LmodTime: mtime}
	return integration.NewRecord(pathname, "", fi, nil, func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(content)), nil
	})
}

func symlinkRecord(pathname string) *integration.Record {
	fi := integration.FileInfo{Lname: path.Base(pathname), Lmode: fs.ModeSymlink | 0777, LmodTime: t0}
	return integration.NewRecord(pathname, "target", fi, nil, nil)
}

func runExport(t *testing.T, n *Nextcloud, in []*integration.Record) map[string]error {
	t.Helper()
	records := make(chan *integration.Record)
	results := make(chan *integration.Result)
	errc := make(chan error, 1)
	go func() { errc <- n.Export(context.Background(), records, results) }()
	go func() {
		for _, r := range in {
			records <- r
		}
		close(records)
	}()

	got := map[string]error{}
	for res := range results {
		got[res.Record.Pathname] = res.Err
	}
	require.NoError(t, <-errc)
	return got
}

func TestExport(t *testing.T) {
	// records carry a reader that can be consumed once, hence a fresh set
	// per case
	input := func() []*integration.Record {
		return []*integration.Record{
			dirRecord("/"),
			dirRecord("/docs"),
			fileRecord("/docs/a.txt", "aaa\n", t1),
			fileRecord("/docs/odd name & #1.txt", "odd\n", t2),
			fileRecord("/docs/what?.txt", "what\n", t2),
			fileRecord("/empty", "", t1),
			fileRecord("/../escape", "no\n", t1),
			symlinkRecord("/link"),
			integration.NewError("/broken", errors.New("unreadable at backup time")),
		}
	}

	cases := []struct {
		name   string
		folder string
		dest   string
	}{
		{name: "into root", folder: "", dest: ""},
		{name: "into new nested folder", folder: "/Restore/2026", dest: "/Restore/2026"},
		{name: "into existing folder", folder: "/gamma", dest: "/gamma"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := populated(t, "")
			n, err := New(context.Background(), options(), "nextcloud", config(srv, tc.folder, nil))
			require.NoError(t, err)

			in := input()
			got := runExport(t, n, in)
			require.Len(t, got, len(in))
			for _, p := range []string{"/", "/docs", "/docs/a.txt", "/docs/odd name & #1.txt", "/docs/what?.txt", "/empty", "/broken"} {
				require.NoError(t, got[p], p)
			}
			if tc.dest == "" {
				// nothing is above the user's files: "/../escape" cleans
				// to "/escape" and lands inside them
				require.NoError(t, got["/../escape"])
				require.Equal(t, "no\n", f.files["/escape"].data)
			} else {
				require.ErrorContains(t, got["/../escape"], "escapes the restore root")
				require.Nil(t, f.files["/escape"])
			}
			require.ErrorIs(t, got["/link"], errNotRegular)

			require.True(t, f.files[tc.dest+"/docs"].dir)
			require.Equal(t, "aaa\n", f.files[tc.dest+"/docs/a.txt"].data)
			require.True(t, t1.Equal(f.files[tc.dest+"/docs/a.txt"].mtime))
			require.Equal(t, "odd\n", f.files[tc.dest+"/docs/odd name & #1.txt"].data)
			require.Equal(t, "what\n", f.files[tc.dest+"/docs/what?.txt"].data)
			require.Nil(t, f.files[tc.dest+"/docs/what"])
			require.Empty(t, f.files[tc.dest+"/empty"].data)
			require.Nil(t, f.files[tc.dest+"/link"])
		})
	}
}

func TestRoundTrip(t *testing.T) {
	_, src := populated(t, "")
	source, err := New(context.Background(), options(), "nextcloud", config(src, "", nil))
	require.NoError(t, err)

	records := make(chan *integration.Record)
	importErr := make(chan error, 1)
	go func() { importErr <- source.Import(context.Background(), records, nil) }()

	dstServer := newFakeNextcloud("")
	dst := httptest.NewServer(dstServer)
	t.Cleanup(dst.Close)
	target, err := New(context.Background(), options(), "nextcloud", config(dst, "/copy", nil))
	require.NoError(t, err)

	// restoring <snapshot>:/files strips /files off the pathnames
	stripped := make(chan *integration.Record)
	go func() {
		for r := range records {
			r.Pathname = strings.TrimPrefix(r.Pathname, filesPrefix)
			if r.Pathname == "" {
				r.Pathname = "/"
			}
			stripped <- r
		}
		close(stripped)
	}()
	results := make(chan *integration.Result)
	exportErr := make(chan error, 1)
	go func() { exportErr <- target.Export(context.Background(), stripped, results) }()
	for res := range results {
		require.NoError(t, res.Err, res.Record.Pathname)
	}
	require.NoError(t, <-importErr)
	require.NoError(t, <-exportErr)

	copied, err := New(context.Background(), options(), "nextcloud", config(dst, "/copy", nil))
	require.NoError(t, err)
	got := runImport(t, copied)
	for p, want := range map[string]string{
		"/files/copy/alpha":                   "alpha\n",
		"/files/copy/gamma/delta":             "delta\n",
		"/files/copy/gamma/notes.txt":         "v3\n",
		"/files/copy/gamma/odd name & #1.txt": "odd\n",
		"/files/copy/gamma/what?.txt":         "what\n",
		"/files/copy/empty":                   "",
	} {
		require.Equal(t, want, got[p].content, p)
	}
	require.True(t, t2.Equal(got["/files/copy/gamma/delta"].mtime))
}
