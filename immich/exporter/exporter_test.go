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
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/poolpOrg/plakar-integrations/immich/api"
	"github.com/poolpOrg/plakar-integrations/immich/importer"
	"github.com/poolpOrg/plakar-integrations/immich/layout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func b64sha1(b []byte) string {
	s := sha1.Sum(b)
	return base64.StdEncoding.EncodeToString(s[:])
}

func str(s string) *string { return &s }

// fakeServer is the target Immich: it records the calls in order and
// answers what the push needs.
type fakeServer struct {
	t  *testing.T
	mu sync.Mutex

	calls    []string          // "METHOD path"
	bodies   map[string][]byte // "METHOD path" -> last body
	uploads  []map[string]string
	existing map[string]string // checksum -> id already on the target
	nextID   int
	assets   map[string]*api.Asset // id -> what GET /assets/{id} answers
	exif     map[string]*api.Exif  // filename -> what the server "extracts" on upload
	people   []api.Person
	albums   []api.Album
}

func newFakeServer(t *testing.T) *fakeServer {
	return &fakeServer{t: t, bodies: map[string][]byte{}, existing: map[string]string{}, assets: map[string]*api.Asset{}, exif: map[string]*api.Exif{}}
}

func (s *fakeServer) record(r *http.Request) string {
	key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, key)
	return key
}

func (s *fakeServer) handler() http.Handler {
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(s.t, json.NewEncoder(w).Encode(v))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := s.record(r)
		p := strings.TrimPrefix(r.URL.Path, "/api")

		if r.Method == http.MethodPost && p == "/assets" {
			if !assert.NoError(s.t, r.ParseMultipartForm(1<<20)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f, hdr, err := r.FormFile("assetData")
			if !assert.NoError(s.t, err) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			b, _ := io.ReadAll(f)
			fields := map[string]string{"filename": hdr.Filename, "checksum": r.Header.Get("X-Immich-Checksum")}
			for k := range r.MultipartForm.Value {
				fields[k] = r.FormValue(k)
			}
			if sc, _, err := r.FormFile("sidecarData"); err == nil {
				sb, _ := io.ReadAll(sc)
				fields["sidecar"] = string(sb)
			}
			s.mu.Lock()
			s.nextID++
			id := "new-" + hdr.Filename
			s.uploads = append(s.uploads, fields)
			cur := &api.Asset{ID: id, IsFavorite: fields["isFavorite"] == "true", Visibility: fields["visibility"]}
			if cur.Visibility == "" {
				cur.Visibility = "timeline"
			}
			// Metadata extraction has run: the date is in, from the
			// file date when the test says nothing else.
			size := int64(len(b))
			cur.ExifInfo = &api.Exif{FileSizeInByte: &size, DateTimeOriginal: str(fields["fileCreatedAt"])}
			if x, ok := s.exif[hdr.Filename]; ok {
				cur.ExifInfo = x
			}
			s.assets[id] = cur
			s.mu.Unlock()
			assert.Equal(s.t, b64sha1(b), base64.StdEncoding.EncodeToString(mustHex(s.t, fields["checksum"])), "uploaded bytes match the announced checksum")
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, api.UploadResult{ID: id, Status: "created"})
			return
		}

		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies[key] = body
		s.mu.Unlock()

		switch {
		case key == "POST /assets/bulk-upload-check":
			var req struct {
				Assets []api.BulkCheckAsset `json:"assets"`
			}
			if !assert.NoError(s.t, json.Unmarshal(body, &req)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var results []api.BulkCheckResult
			for _, a := range req.Assets {
				if id, ok := s.existing[a.Checksum]; ok {
					results = append(results, api.BulkCheckResult{ID: a.ID, Action: "reject", Reason: str("duplicate"), AssetID: str(id)})
				} else {
					results = append(results, api.BulkCheckResult{ID: a.ID, Action: "accept"})
				}
			}
			writeJSON(w, map[string]any{"results": results})
		case key == "GET /albums":
			writeJSON(w, s.albums)
		case key == "POST /albums":
			writeJSON(w, api.Album{ID: "album-1"})
		case r.Method == http.MethodPatch && strings.HasPrefix(p, "/albums/"):
			writeJSON(w, api.Album{ID: path.Base(p)})
		case r.Method == http.MethodPut && strings.HasSuffix(p, "/assets") && strings.HasPrefix(p, "/albums/"):
			writeJSON(w, []api.BulkIDResult{})
		case key == "POST /stacks":
			writeJSON(w, api.Stack{ID: "stack-1"})
		case key == "PUT /tags":
			var req struct {
				Tags []string `json:"tags"`
			}
			if !assert.NoError(s.t, json.Unmarshal(body, &req)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var tags []api.Tag
			for _, v := range req.Tags {
				tags = append(tags, api.Tag{ID: "tag-" + v, Value: v})
			}
			writeJSON(w, tags)
		case key == "PUT /tags/assets":
			writeJSON(w, map[string]int{"count": 1})
		case key == "GET /people":
			writeJSON(w, map[string]any{"people": s.people, "hasNextPage": false, "total": len(s.people)})
		case key == "POST /people":
			writeJSON(w, api.Person{ID: "person-new"})
		case key == "GET /faces":
			writeJSON(w, []api.Face{{ID: "f", Person: &api.Person{ID: "person-existing"}}})
		case key == "POST /faces":
			writeJSON(w, map[string]string{"id": "face-1"})
		case key == "PUT /assets":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/assets/"):
			s.mu.Lock()
			a, ok := s.assets[path.Base(p)]
			s.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, a)
		default:
			s.t.Errorf("unexpected request %s", key)
			http.NotFound(w, r)
		}
	})
}

func mustHex(t *testing.T, h string) []byte {
	t.Helper()
	b := make([]byte, len(h)/2)
	for i := range b {
		var v byte
		for _, c := range h[2*i : 2*i+2] {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= byte(c - '0')
			case c >= 'a' && c <= 'f':
				v |= byte(c-'a') + 10
			default:
				t.Fatalf("bad hex %q", h)
			}
		}
		b[i] = v
	}
	return b
}

func newTestExporter(t *testing.T, s *fakeServer) *Exporter {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	t.Setenv(importer.EnvAPIKey, "secret")

	exp, err := NewExporter(context.Background(), &connectors.Options{MaxConcurrency: 2}, "immich",
		map[string]string{"location": "immich://" + strings.TrimPrefix(srv.URL, "http://")})
	require.NoError(t, err)
	e := exp.(*Exporter)
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

func fileRecord(p string, b []byte) *connectors.Record {
	fi := objects.FileInfo{Lname: path.Base(p), Lsize: int64(len(b)), Lmode: 0o644, LmodTime: time.Now(), Lnlink: 1}
	return connectors.NewRecord(p, "", fi, nil, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	})
}

func dirRecord(p string) *connectors.Record {
	fi := objects.FileInfo{Lname: path.Base(p), Lmode: os.ModeDir | 0o755, LmodTime: time.Now(), Lnlink: 1}
	return connectors.NewRecord(p, "", fi, nil, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(nil)), nil
	})
}

func jsonRecord(t *testing.T, p string, v any) *connectors.Record {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return fileRecord(p, b)
}

// snapshot is a small immich:// snapshot: a live photo pair, an archived
// photo with a sidecar, a tag, a face, an album and a stack, plus one asset
// the target already has.
type snapshot struct {
	img, vid, arc, old []byte
	records            []*connectors.Record
}

func newSnapshot(t *testing.T) *snapshot {
	s := &snapshot{
		img: []byte("live image"), vid: []byte("live video"), arc: []byte("archived"), old: []byte("already there"),
	}
	rating := 4
	assets := map[string]layout.Asset{
		"/assets/2024/07/IMG_0001.jpg": {
			Checksum: b64sha1(s.img), ImmichID: "src-img", Type: "IMAGE", OriginalFileName: "IMG_0001.jpg",
			FileCreatedAt: "2024-07-09T13:00:00.000Z", FileModifiedAt: "2024-07-01T15:00:00.000Z",
			Visibility: "timeline", LivePhotoVideo: b64sha1(s.vid), Tags: []string{"poc"},
			Exif: &api.Exif{DateTimeOriginal: str("2024-07-09T13:00:00+00:00"), TimeZone: str("UTC+2")},
		},
		"/assets/2024/07/IMG_0001.mov": {
			Checksum: b64sha1(s.vid), ImmichID: "src-vid", Type: "VIDEO", OriginalFileName: "IMG_0001.mov",
			FileCreatedAt: "2024-07-09T13:00:00.000Z", FileModifiedAt: "2024-07-01T15:00:00.000Z",
			Visibility: "hidden", Duration: "2000",
		},
		"/assets/2023/08/beach.jpg": {
			Checksum: b64sha1(s.arc), ImmichID: "src-arc", Type: "IMAGE", OriginalFileName: "beach.jpg",
			FileCreatedAt: "2023-08-01T10:00:00.000Z", FileModifiedAt: "2023-08-01T10:00:00.000Z",
			Visibility: "archive", IsFavorite: true, Sidecar: true, Tags: []string{"poc/holiday"}, People: []string{"Neil"},
			Exif: &api.Exif{DateTimeOriginal: str("2023-08-01T10:00:00+00:00"), TimeZone: str("UTC+2"), Description: str("sea"), Rating: &rating},
		},
		"/assets/2022/01/old.jpg": {
			Checksum: b64sha1(s.old), ImmichID: "src-old", Type: "IMAGE", OriginalFileName: "old.jpg",
			FileCreatedAt: "2022-01-01T10:00:00.000Z", FileModifiedAt: "2022-01-01T10:00:00.000Z",
			Visibility: "timeline", People: []string{"Neil"},
		},
	}
	bytesOf := map[string][]byte{
		"/assets/2024/07/IMG_0001.jpg": s.img, "/assets/2024/07/IMG_0001.mov": s.vid,
		"/assets/2023/08/beach.jpg": s.arc, "/assets/2022/01/old.jpg": s.old,
	}

	s.records = append(s.records, dirRecord("/"), jsonRecord(t, layout.ManifestPath, layout.Manifest{Format: 1}),
		dirRecord("/assets"), dirRecord("/assets/2024"), dirRecord("/assets/2024/07"), dirRecord("/assets/2023"), dirRecord("/assets/2023/08"),
		dirRecord("/assets/2022"), dirRecord("/assets/2022/01"), dirRecord("/albums"), dirRecord("/stacks"), dirRecord("/people"))
	for p, a := range assets {
		s.records = append(s.records, jsonRecord(t, p+".json", a), fileRecord(p, bytesOf[p]))
	}
	s.records = append(s.records,
		fileRecord("/assets/2023/08/beach.jpg.xmp", []byte("<x:xmpmeta/>")),
		jsonRecord(t, "/albums/Trip.json", layout.Album{Name: "Trip", Description: "d", Order: "desc", Thumbnail: b64sha1(s.arc), Assets: []string{b64sha1(s.img), b64sha1(s.arc), "missing"}}),
		jsonRecord(t, "/albums/Existing.json", layout.Album{Name: "Existing", Assets: []string{b64sha1(s.old)}}),
		jsonRecord(t, "/stacks/x.json", layout.Stack{Primary: b64sha1(s.img), Assets: []string{b64sha1(s.img), b64sha1(s.old)}}),
		jsonRecord(t, "/people/Neil.json", layout.Person{Name: "Neil", BirthDate: str("1930-08-05"), Faces: []layout.Face{
			{Asset: b64sha1(s.arc), ImageWidth: 800, ImageHeight: 600, X1: 10, Y1: 20, X2: 110, Y2: 220},
			{Asset: b64sha1(s.old), ImageWidth: 800, ImageHeight: 600, X1: 1, Y1: 2, X2: 3, Y2: 4},
		}}),
	)
	return s
}

func runExport(t *testing.T, e *Exporter, records []*connectors.Record) error {
	t.Helper()
	in := make(chan *connectors.Record)
	out := make(chan *connectors.Result, len(records)+1)
	go func() {
		for _, r := range records {
			in <- r
		}
		close(in)
	}()
	err := e.Export(context.Background(), nil, in, out)
	n := 0
	for res := range out {
		require.NoError(t, res.Err, res.Record.Pathname)
		n++
	}
	require.Equal(t, len(records), n, "every record is acknowledged")
	return err
}

func TestPushOrdering(t *testing.T) {
	s := newFakeServer(t)
	snap := newSnapshot(t)
	s.existing[b64sha1(snap.old)] = "old-id"
	s.assets["old-id"] = &api.Asset{ID: "old-id"}
	s.people = []api.Person{{ID: "person-existing", Name: "Neil"}}
	s.albums = []api.Album{{ID: "album-existing", AlbumName: "Existing"}}

	e := newTestExporter(t, s)
	require.NoError(t, runExport(t, e, snap.records))

	// The upload sequence: the live video before its image, nothing for the duplicate.
	var uploaded []string
	for _, u := range s.uploads {
		uploaded = append(uploaded, u["filename"])
	}
	require.Equal(t, []string{"IMG_0001.mov", "beach.jpg", "IMG_0001.jpg"}, uploaded)

	byName := map[string]map[string]string{}
	for _, u := range s.uploads {
		byName[u["filename"]] = u
	}
	require.Equal(t, "new-IMG_0001.mov", byName["IMG_0001.jpg"]["livePhotoVideoId"])
	require.Empty(t, byName["IMG_0001.mov"]["visibility"], "hidden is the server's to set")
	require.Equal(t, "2000", byName["IMG_0001.mov"]["duration"])
	require.Empty(t, byName["IMG_0001.jpg"]["duration"])
	require.Equal(t, "archive", byName["beach.jpg"]["visibility"])
	require.Equal(t, "true", byName["beach.jpg"]["isFavorite"])
	require.Equal(t, "<x:xmpmeta/>", byName["beach.jpg"]["sidecar"])
	require.Equal(t, "2023-08-01T10:00:00.000Z", byName["beach.jpg"]["fileCreatedAt"])
	require.Equal(t, "plakar", byName["beach.jpg"]["deviceId"])

	// The phases, in order: check, uploads, albums, stacks, tags, people, faces, fix-up.
	phases := []string{
		"POST /assets/bulk-upload-check",
		"POST /assets", "POST /assets", "POST /assets",
		"GET /albums", "PUT /albums/album-existing/assets", "POST /albums", "PATCH /albums/album-1",
		"POST /stacks",
		"PUT /tags", "PUT /tags/assets", "PUT /tags/assets",
		"GET /people", "POST /faces", "GET /faces",
		"GET /assets/new-beach.jpg", "PUT /assets", "GET /assets/new-IMG_0001.jpg", "PUT /assets", "GET /assets/new-IMG_0001.mov",
	}
	require.Equal(t, phases, s.calls)

	var album api.CreateAlbum
	require.NoError(t, json.Unmarshal(s.bodies["POST /albums"], &album))
	require.Equal(t, "Trip", album.AlbumName)
	require.Equal(t, []string{"new-IMG_0001.jpg", "new-beach.jpg"}, album.AssetIDs, "unknown checksums are dropped")
	var patch api.UpdateAlbum
	require.NoError(t, json.Unmarshal(s.bodies["PATCH /albums/album-1"], &patch))
	require.Equal(t, "new-beach.jpg", *patch.AlbumThumbnailAssetID)
	require.Equal(t, "desc", *patch.Order)

	var stack struct {
		AssetIDs []string `json:"assetIds"`
	}
	require.NoError(t, json.Unmarshal(s.bodies["POST /stacks"], &stack))
	require.Equal(t, []string{"new-IMG_0001.jpg", "old-id"}, stack.AssetIDs, "the primary comes first, duplicates join by their existing id")

	var tags struct {
		Tags []string `json:"tags"`
	}
	require.NoError(t, json.Unmarshal(s.bodies["PUT /tags"], &tags))
	require.Equal(t, []string{"poc", "poc/holiday"}, tags.Tags)

	var face api.CreateFace
	require.NoError(t, json.Unmarshal(s.bodies["POST /faces"], &face))
	require.Equal(t, api.CreateFace{AssetID: "new-beach.jpg", PersonID: "person-existing", ImageWidth: 800, ImageHeight: 600, X: 10, Y: 20, Width: 100, Height: 200}, face,
		"the existing person is reused and the box is converted; the existing asset already has the face")

	// The fix-up pass sends only what differs: the fake stored no EXIF, so
	// the date and zone go back; favorite and visibility were honoured.
	var upd api.BulkUpdate
	require.NoError(t, json.Unmarshal(s.bodies["PUT /assets"], &upd))
	require.Equal(t, []string{"new-IMG_0001.jpg"}, upd.IDs)
	require.Equal(t, "2024-07-09T13:00:00+00:00", *upd.DateTimeOriginal)
	require.Equal(t, "UTC+2", *upd.TimeZone)
	require.Nil(t, upd.IsFavorite)
	require.Nil(t, upd.Visibility)
}

func TestFixupLeavesMatchingAssetsAlone(t *testing.T) {
	s := newFakeServer(t)
	snap := newSnapshot(t)
	// The server extracts exactly what the snapshot carries, in its own
	// date spelling.
	rating := 4
	s.exif["beach.jpg"] = &api.Exif{DateTimeOriginal: str("2023-08-01T10:00:00.000Z"), TimeZone: str("UTC+2"), Description: str("sea"), Rating: &rating}
	e := newTestExporter(t, s)

	// Only the archived photo this time.
	var records []*connectors.Record
	for _, r := range snap.records {
		if strings.HasPrefix(r.Pathname, "/assets/2023/08/") || r.FileInfo.IsDir() {
			records = append(records, r)
		}
	}

	require.NoError(t, runExport(t, e, records))
	require.Contains(t, s.calls, "GET /assets/new-beach.jpg")
	require.NotContains(t, s.calls, "PUT /assets", "nothing differs after upload, so nothing is sent")
}

func TestFixupGivesUpWhenMetadataNeverComes(t *testing.T) {
	s := newFakeServer(t)
	snap := newSnapshot(t)
	// The upload row only, as the server has it before the job runs.
	size := int64(8)
	s.exif["beach.jpg"] = &api.Exif{FileSizeInByte: &size}
	e := newTestExporter(t, s)

	oldWait, oldPoll := metadataWait, metadataPoll
	metadataWait, metadataPoll = 50*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { metadataWait, metadataPoll = oldWait, oldPoll })

	var records []*connectors.Record
	for _, r := range snap.records {
		if strings.HasPrefix(r.Pathname, "/assets/2023/08/") || r.FileInfo.IsDir() {
			records = append(records, r)
		}
	}
	require.NoError(t, runExport(t, e, records))

	polls := 0
	for _, c := range s.calls {
		if c == "GET /assets/new-beach.jpg" {
			polls++
		}
	}
	require.Greater(t, polls, 1, "the asset is polled")
	require.NotContains(t, s.calls, "PUT /assets", "no blind update on an asset whose metadata never came")
}

func TestPushReportsFailuresAndKeepsGoing(t *testing.T) {
	s := newFakeServer(t)
	snap := newSnapshot(t)
	e := newTestExporter(t, s)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/stacks" {
			s.record(r)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"assets already in a stack"}`)
			return
		}
		s.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	client, err := api.New(api.Config{Location: "immich://" + strings.TrimPrefix(srv.URL, "http://"), APIKey: "k"})
	require.NoError(t, err)
	e.client = client

	err = runExport(t, e, snap.records)
	require.ErrorContains(t, err, "assets already in a stack")
	require.Contains(t, s.calls, "PUT /tags", "the tag phase still ran after the stack failure")
	require.Contains(t, s.calls, "POST /faces")
}

func TestDiffAsset(t *testing.T) {
	lat, lon := 48.85, 2.35
	rating := 3
	tests := []struct {
		name string
		src  layout.Asset
		cur  api.Asset
		want api.BulkUpdate
	}{
		{
			name: "identical",
			src:  layout.Asset{Visibility: "timeline", Exif: &api.Exif{DateTimeOriginal: str("2024-07-09T13:00:00+00:00"), TimeZone: str("UTC+2")}},
			cur:  api.Asset{Visibility: "timeline", ExifInfo: &api.Exif{DateTimeOriginal: str("2024-07-09T13:00:00.000Z"), TimeZone: str("UTC+2")}},
			want: api.BulkUpdate{},
		},
		{
			name: "zone lost without gps",
			src:  layout.Asset{Exif: &api.Exif{DateTimeOriginal: str("2024-07-09T13:00:00+00:00"), TimeZone: str("UTC+2")}},
			cur:  api.Asset{ExifInfo: &api.Exif{DateTimeOriginal: str("2024-07-09T13:00:00+00:00"), TimeZone: str("UTC+0")}},
			want: api.BulkUpdate{DateTimeOriginal: str("2024-07-09T13:00:00+00:00"), TimeZone: str("UTC+2")},
		},
		{
			name: "zone differs with gps is left alone, coordinates sent",
			src:  layout.Asset{Exif: &api.Exif{DateTimeOriginal: str("2024-07-09T13:00:00+00:00"), TimeZone: str("UTC+2"), Latitude: &lat, Longitude: &lon}},
			cur:  api.Asset{ExifInfo: &api.Exif{DateTimeOriginal: str("2024-07-09T13:00:00+00:00"), TimeZone: str("Europe/Paris")}},
			want: api.BulkUpdate{Latitude: &lat, Longitude: &lon},
		},
		{
			name: "date differs with gps sends the date without a zone",
			src:  layout.Asset{Exif: &api.Exif{DateTimeOriginal: str("2024-07-09T13:00:00+00:00"), TimeZone: str("UTC+2"), Latitude: &lat, Longitude: &lon}},
			cur:  api.Asset{ExifInfo: &api.Exif{DateTimeOriginal: str("2024-07-09T14:00:00+00:00"), TimeZone: str("Europe/Paris"), Latitude: &lat, Longitude: &lon}},
			want: api.BulkUpdate{DateTimeOriginal: str("2024-07-09T13:00:00+00:00")},
		},
		{
			name: "favorite, visibility, description and rating",
			src:  layout.Asset{IsFavorite: true, Visibility: "archive", Exif: &api.Exif{Description: str("sea"), Rating: &rating}},
			cur:  api.Asset{Visibility: "timeline", ExifInfo: &api.Exif{Description: str("")}},
			want: api.BulkUpdate{IsFavorite: func() *bool { b := true; return &b }(), Visibility: str("archive"), Description: str("sea"), Rating: &rating},
		},
		{
			name: "hidden and locked are never sent",
			src:  layout.Asset{Visibility: "hidden"},
			cur:  api.Asset{Visibility: "timeline"},
			want: api.BulkUpdate{},
		},
		{
			name: "no exif on the source",
			src:  layout.Asset{},
			cur:  api.Asset{ExifInfo: &api.Exif{DateTimeOriginal: str("2024-07-09T13:00:00+00:00")}},
			want: api.BulkUpdate{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, diffAsset(&tc.src, &tc.cur))
		})
	}
}

func TestRelative(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "/", want: "."},
		{in: "/assets/a.jpg", want: "assets/a.jpg"},
		{in: "/../../etc/passwd", want: "etc/passwd"},
		{in: "assets/../x", want: "x"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := relative(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestCloseRemovesStaging(t *testing.T) {
	s := newFakeServer(t)
	e := newTestExporter(t, s)
	dir := e.tempDir
	_, err := os.Stat(dir)
	require.NoError(t, err)
	require.NoError(t, e.Close(context.Background()))
	_, err = os.Stat(dir)
	require.True(t, os.IsNotExist(err))
}
