package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PlakarKorp/integration-sdk"
)

// fakePicker mimics the Google Photos Picker API plus the media download
// endpoints for the items it serves.
type fakePicker struct {
	t *testing.T

	mu       sync.Mutex
	sessions map[string]int // id -> number of polls so far
	deleted  []string
	polls    int // polls before a session reports mediaItemsSet
	items    []mediaItem

	listings atomic.Int32 // mediaItems.list calls
	gen      atomic.Int32 // base URL generation; bump to expire previous URLs
	token    string

	srv *httptest.Server
}

func newFakePicker(t *testing.T, items []mediaItem) *fakePicker {
	f := &fakePicker{t: t, sessions: make(map[string]int), polls: 2, items: items, token: "test-token"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePicker) handle(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+f.token {
		http.Error(w, "unauthorized: "+got, http.StatusUnauthorized)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
		var body struct {
			PickingConfig struct {
				MaxItemCount string `json:"maxItemCount"`
			} `json:"pickingConfig"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.PickingConfig.MaxItemCount != "2000" {
			f.t.Errorf("maxItemCount = %q, want 2000", body.PickingConfig.MaxItemCount)
		}
		id := fmt.Sprintf("sess-%d", len(f.sessions)+len(f.deleted)+1)
		f.sessions[id] = 0
		json.NewEncoder(w).Encode(f.session(id))

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		if _, ok := f.sessions[id]; !ok {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		f.sessions[id]++
		json.NewEncoder(w).Encode(f.session(id))

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		delete(f.sessions, id)
		f.deleted = append(f.deleted, id)
		w.Write([]byte("{}"))

	case r.Method == http.MethodGet && r.URL.Path == "/v1/mediaItems":
		id := r.URL.Query().Get("sessionId")
		if polls, ok := f.sessions[id]; !ok || polls < f.polls {
			http.Error(w, "FAILED_PRECONDITION", http.StatusBadRequest)
			return
		}
		f.listings.Add(1)
		// Two pages: first item alone, then the rest.
		resp := map[string]any{}
		switch r.URL.Query().Get("pageToken") {
		case "":
			resp["mediaItems"] = f.withBaseURLs(f.items[:1])
			if len(f.items) > 1 {
				resp["nextPageToken"] = "p2"
			}
		case "p2":
			resp["mediaItems"] = f.withBaseURLs(f.items[1:])
		}
		json.NewEncoder(w).Encode(resp)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/media/"):
		// /media/<id>/g<gen>=d or =dv
		rest := strings.TrimPrefix(r.URL.Path, "/media/")
		id, tail, _ := strings.Cut(rest, "/")
		genStr, param, ok := strings.Cut(tail, "=")
		if !ok {
			http.Error(w, "missing download parameter", http.StatusBadRequest)
			return
		}
		if genStr != fmt.Sprintf("g%d", f.gen.Load()) {
			http.Error(w, "base url expired", http.StatusForbidden)
			return
		}
		for _, it := range f.items {
			if it.ID != id {
				continue
			}
			want := "d"
			if it.isVideo() {
				want = "dv"
			}
			if param != want {
				http.Error(w, "wrong download parameter "+param, http.StatusBadRequest)
				return
			}
			w.Write([]byte("bytes of " + id))
			return
		}
		http.Error(w, "unknown item", http.StatusNotFound)

	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// withBaseURLs stamps the items with base URLs of the current generation.
func (f *fakePicker) withBaseURLs(items []mediaItem) []mediaItem {
	out := make([]mediaItem, len(items))
	for i, it := range items {
		it.MediaFile.BaseURL = fmt.Sprintf("%s/media/%s/g%d", f.srv.URL, it.ID, f.gen.Load())
		out[i] = it
	}
	return out
}

func (f *fakePicker) session(id string) *session {
	s := &session{ID: id, PickerURI: "https://photos.google.com/picker/" + id}
	if f.sessions[id] >= f.polls {
		s.MediaItemsSet = true
	} else {
		s.PollingConfig = &struct {
			PollInterval string `json:"pollInterval"`
			TimeoutIn    string `json:"timeoutIn"`
		}{PollInterval: "0.001s", TimeoutIn: "5s"}
	}
	return s
}

func item(id, filename, mime, created string) mediaItem {
	var m mediaItem
	m.ID = id
	m.CreateTime = created
	m.MediaFile.Filename = filename
	m.MediaFile.MimeType = mime
	if strings.HasPrefix(mime, "video/") {
		m.Type = "VIDEO"
	} else {
		m.Type = "PHOTO"
	}
	return m
}

func newTestImporter(t *testing.T, f *fakePicker, extra map[string]string) (*Importer, *bytes.Buffer) {
	config := map[string]string{
		"location":     "gphotos://",
		"access_token": f.token,
		"endpoint":     f.srv.URL,
	}
	for k, v := range extra {
		config[k] = v
	}
	out := &bytes.Buffer{}
	opts := &integration.Options{MaxConcurrency: 2, Stderr: out}
	imp, err := NewImporter(context.Background(), opts, "gphotos", config)
	if err != nil {
		t.Fatal(err)
	}
	return imp.(*Importer), out
}

// drain runs Import and collects every record.
func drain(t *testing.T, imp *Importer) []*integration.Record {
	t.Helper()
	records := make(chan *integration.Record, 64)
	var got []*integration.Record
	done := make(chan struct{})
	go func() {
		for r := range records {
			got = append(got, r)
		}
		close(done)
	}()
	if _, err := imp.Import(context.Background(), records, nil); err != nil {
		t.Fatal(err)
	}
	<-done
	return got
}

func TestImportDateLayout(t *testing.T) {
	f := newFakePicker(t, []mediaItem{
		item("a", "IMG_1.jpg", "image/jpeg", "2024-03-10T12:00:00Z"),
		item("b", "img_1.jpg", "image/jpeg", "2024-03-11T12:00:00Z"), // collides, case-insensitively
		item("c", "clip.mp4", "video/mp4", "2023-12-31T23:59:59Z"),
		item("d", "", "image/png", ""), // no filename, no date
	})
	imp, out := newTestImporter(t, f, nil)
	records := drain(t, imp)

	if !strings.Contains(out.String(), "https://photos.google.com/picker/sess-1") {
		t.Errorf("picker URL not shown to the user:\n%s", out.String())
	}

	var paths []string
	byPath := map[string]*integration.Record{}
	for _, r := range records {
		if r.Err != nil {
			t.Errorf("unexpected error record %s: %v", r.Pathname, r.Err)
		}
		paths = append(paths, r.Pathname)
		byPath[r.Pathname] = r
	}
	sort.Strings(paths)
	want := []string{
		"/", "/2023", "/2023/12", "/2023/12/clip.mp4",
		"/2024", "/2024/03", "/2024/03/IMG_1.jpg", "/2024/03/img_1 (1).jpg",
		"/undated", "/undated/d",
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v\nwant %v", paths, want)
	}

	for _, p := range []string{"/", "/2024", "/2024/03", "/undated"} {
		if !byPath[p].FileInfo.IsDir() {
			t.Errorf("%s should be a directory", p)
		}
	}

	fi := byPath["/2024/03/IMG_1.jpg"].FileInfo
	if fi.Size() != -1 || !fi.Mode().IsRegular() || fi.Name() != "IMG_1.jpg" {
		t.Errorf("unexpected file info %+v", fi)
	}
	if !fi.ModTime().Equal(time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("mod time = %v", fi.ModTime())
	}
	if fi.Ino() == 0 || fi.Ino() == byPath["/2024/03/img_1 (1).jpg"].FileInfo.Ino() {
		t.Errorf("inodes should be stable and distinct")
	}

	for p, id := range map[string]string{"/2024/03/IMG_1.jpg": "a", "/2023/12/clip.mp4": "c", "/undated/d": "d"} {
		data, err := io.ReadAll(byPath[p].Reader)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if string(data) != "bytes of "+id {
			t.Errorf("%s = %q", p, data)
		}
		byPath[p].Reader.Close()
	}

	if err := imp.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "sess-1" {
		t.Errorf("sessions deleted on Close = %v", f.deleted)
	}
}

func TestImportFlatLayout(t *testing.T) {
	f := newFakePicker(t, []mediaItem{
		item("a", "one.jpg", "image/jpeg", "2024-03-10T12:00:00Z"),
		item("b", "two.jpg", "image/jpeg", "2021-01-01T00:00:00Z"),
	})
	imp, _ := newTestImporter(t, f, map[string]string{"layout": "flat"})
	var paths []string
	for _, r := range drain(t, imp) {
		paths = append(paths, r.Pathname)
	}
	sort.Strings(paths)
	if got := strings.Join(paths, ","); got != "/,/one.jpg,/two.jpg" {
		t.Errorf("paths = %s", got)
	}
}

func TestDuplicateNamesAreSuffixedInCreationOrder(t *testing.T) {
	// Listed newest first by the API; suffixes must still follow creation time.
	f := newFakePicker(t, []mediaItem{
		item("later", "x.jpg", "image/jpeg", "2024-03-20T12:00:00Z"),
		item("earlier", "x.jpg", "image/jpeg", "2024-03-01T12:00:00Z"),
	})
	imp, _ := newTestImporter(t, f, nil)
	names := map[string]string{}
	for _, r := range drain(t, imp) {
		if r.FileInfo.IsDir() {
			continue
		}
		data, _ := io.ReadAll(r.Reader)
		names[r.Pathname] = string(data)
	}
	if names["/2024/03/x.jpg"] != "bytes of earlier" || names["/2024/03/x (1).jpg"] != "bytes of later" {
		t.Errorf("names = %v", names)
	}
}

func TestExpiredBaseURLIsRefreshed(t *testing.T) {
	f := newFakePicker(t, []mediaItem{
		item("a", "one.jpg", "image/jpeg", "2024-03-10T12:00:00Z"),
	})
	imp, _ := newTestImporter(t, f, nil)
	imp.refreshAfter = 0 // a failed download may re-list straight away
	records := drain(t, imp)

	// Expire every base URL handed out so far, as Google does after an hour.
	f.gen.Add(1)
	before := f.listings.Load()

	var file *integration.Record
	for _, r := range records {
		if r.Pathname == "/2024/03/one.jpg" {
			file = r
		}
	}
	data, err := io.ReadAll(file.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "bytes of a" {
		t.Errorf("got %q", data)
	}
	if f.listings.Load() != before+1 {
		t.Errorf("expected one re-listing, got %d", f.listings.Load()-before)
	}
}

func TestDownloadsOutliveImportContext(t *testing.T) {
	f := newFakePicker(t, []mediaItem{
		item("a", "one.jpg", "image/jpeg", "2024-03-10T12:00:00Z"),
	})
	imp, _ := newTestImporter(t, f, nil)

	ctx, cancel := context.WithCancel(context.Background())
	records := make(chan *integration.Record, 64)
	if _, err := imp.Import(ctx, records, nil); err != nil {
		t.Fatal(err)
	}
	cancel() // kloset is done with the Import RPC but still opens readers

	var file *integration.Record
	for r := range records {
		if !r.FileInfo.IsDir() {
			file = r
		}
	}
	data, err := io.ReadAll(file.Reader)
	if err != nil {
		t.Fatalf("download after Import returned: %v", err)
	}
	if string(data) != "bytes of a" {
		t.Errorf("got %q", data)
	}
}

func TestImportStopsWhenCancelled(t *testing.T) {
	var items []mediaItem
	for i := 0; i < 50; i++ {
		items = append(items, item(fmt.Sprintf("i%02d", i), fmt.Sprintf("%d.jpg", i), "image/jpeg", "2024-03-10T12:00:00Z"))
	}
	f := newFakePicker(t, items)
	imp, _ := newTestImporter(t, f, nil)

	ctx, cancel := context.WithCancel(context.Background())
	records := make(chan *integration.Record) // nobody reads: kloset went away
	errc := make(chan error, 1)
	go func() {
		_, err := imp.Import(ctx, records, nil)
		errc <- err
	}()
	<-records // the root directory
	cancel()

	select {
	case err := <-errc:
		if err == nil {
			t.Error("cancelled Import should report the context error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Import blocked on a channel nobody drains")
	}
}

func TestVideoStillProcessing(t *testing.T) {
	v := item("v", "clip.mp4", "video/mp4", "2024-03-10T12:00:00Z")
	v.MediaFile.MediaFileMetadata.VideoMetadata = &struct {
		ProcessingStatus string `json:"processingStatus"`
	}{ProcessingStatus: "PROCESSING"}
	f := newFakePicker(t, []mediaItem{v})
	imp, _ := newTestImporter(t, f, nil)

	var errs int
	for _, r := range drain(t, imp) {
		if r.Err != nil {
			errs++
			if r.Pathname != "/2024/03/clip.mp4" {
				t.Errorf("error on %s", r.Pathname)
			}
		}
	}
	if errs != 1 {
		t.Errorf("expected one error record, got %d", errs)
	}
}

func TestMultipleRoundsDedupById(t *testing.T) {
	f := newFakePicker(t, []mediaItem{
		item("a", "one.jpg", "image/jpeg", "2024-03-10T12:00:00Z"),
	})
	imp, out := newTestImporter(t, f, map[string]string{"sessions": "2"})
	var files int
	for _, r := range drain(t, imp) {
		if !r.FileInfo.IsDir() {
			files++
		}
	}
	if files != 1 {
		t.Errorf("item picked in both rounds should be stored once, got %d", files)
	}
	if !strings.Contains(out.String(), "round 2 of 2") {
		t.Errorf("rounds not announced:\n%s", out.String())
	}
	imp.Close(context.Background())
	if len(f.deleted) != 2 {
		t.Errorf("both sessions should be deleted, got %v", f.deleted)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := map[string]map[string]string{
		"album in location":    {"location": "gphotos://album", "access_token": "x"},
		"no credentials":       {"location": "gphotos://"},
		"zero sessions":        {"location": "gphotos://", "access_token": "x", "sessions": "0"},
		"too many items":       {"location": "gphotos://", "access_token": "x", "max_items": "5000"},
		"unknown layout":       {"location": "gphotos://", "access_token": "x", "layout": "tree"},
		"token without client": {"location": "gphotos://", "token_json": `{"refresh_token":"r"}`},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewImporter(context.Background(), &integration.Options{}, "gphotos", cfg); err == nil {
				t.Errorf("config %v should be rejected", cfg)
			}
		})
	}
}

func TestTokenFileRoundTrip(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(tokenFile, []byte(`{"access_token":"at","refresh_token":"rt","expiry":"2099-01-01T00:00:00Z"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]string{
		"credentials_json": clientJSON("https://oauth2.googleapis.com/token"),
		"token_file":       tokenFile,
	}
	ts, err := tokenSource(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at" {
		t.Errorf("access token = %q", tok.AccessToken)
	}
}

func TestUniqueName(t *testing.T) {
	seen := map[string]int{}
	cases := []struct {
		name          string
		dir, file, id string
		want          string
	}{
		{"first", "/d", "IMG.jpg", "1", "IMG.jpg"},
		{"case-insensitive clash", "/d", "img.jpg", "2", "img (1).jpg"},
		{"third copy", "/d", "IMG.jpg", "3", "IMG (2).jpg"},
		{"other directory", "/e", "IMG.jpg", "4", "IMG.jpg"},
		{"no filename", "/d", "", "xyz", "xyz"},
		{"slash in filename", "/d", "a/b.jpg", "5", "a_b.jpg"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := uniqueName(seen, c.dir, c.file, c.id); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestDownloadURL(t *testing.T) {
	if got := downloadURL("https://x/abc", false); got != "https://x/abc=d" {
		t.Errorf("photo url = %q", got)
	}
	if got := downloadURL("https://x/abc", true); got != "https://x/abc=dv" {
		t.Errorf("video url = %q", got)
	}
}
