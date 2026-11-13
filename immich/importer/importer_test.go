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
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/poolpOrg/plakar-integrations/immich/api"
	"github.com/poolpOrg/plakar-integrations/immich/layout"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func b64sha1(b []byte) string {
	s := sha1.Sum(b)
	return base64.StdEncoding.EncodeToString(s[:])
}

func str(s string) *string { return &s }

// fixture is a small Immich: a live photo pair, an archived photo with a
// sidecar, a tag and a face, and a name collision in the same month.
type fixture struct {
	bytes    map[string][]byte // asset id -> original
	assets   []api.Asset
	sidecars map[string]string // asset id -> xmp
	corrupt  string            // asset id whose download is tampered with
}

func newFixture() *fixture {
	f := &fixture{
		bytes: map[string][]byte{
			"img": []byte("live photo image"),
			"vid": []byte("live photo video"),
			"arc": []byte("archived photo"),
			"dup": []byte("other photo same name"),
		},
		sidecars: map[string]string{"arc": "<x:xmpmeta/>"},
	}
	size := func(id string) *int64 { n := int64(len(f.bytes[id])); return &n }
	f.assets = []api.Asset{
		{
			ID: "img", Type: "IMAGE", OriginalFileName: "IMG_0001.jpg", Checksum: b64sha1(f.bytes["img"]),
			FileCreatedAt: "2024-07-09T13:00:00.000Z", FileModifiedAt: "2024-07-01T15:00:00.000Z", LocalDateTime: "2024-07-09T15:00:00.000Z",
			Visibility: "timeline", LivePhotoVideoID: str("vid"),
			ExifInfo: &api.Exif{FileSizeInByte: size("img"), DateTimeOriginal: str("2024-07-09T13:00:00+00:00"), TimeZone: str("UTC+2")},
		},
		{
			ID: "vid", Type: "VIDEO", OriginalFileName: "IMG_0001.mov", Checksum: b64sha1(f.bytes["vid"]),
			FileCreatedAt: "2024-07-09T13:00:00.000Z", FileModifiedAt: "2024-07-01T15:00:00.000Z", LocalDateTime: "2024-07-09T15:00:00.000Z",
			Visibility: "hidden", Duration: "2000",
			ExifInfo: &api.Exif{FileSizeInByte: size("vid")},
		},
		{
			ID: "arc", Type: "IMAGE", OriginalFileName: "beach.jpg", Checksum: b64sha1(f.bytes["arc"]),
			FileCreatedAt: "2023-08-01T10:00:00.000Z", FileModifiedAt: "2023-08-01T10:00:00.000Z", LocalDateTime: "2023-08-01T12:00:00.000Z",
			Visibility: "archive", IsFavorite: true,
			ExifInfo: &api.Exif{FileSizeInByte: size("arc"), Description: str("sea"), Rating: func() *int { r := 4; return &r }()},
			People:   []api.Person{{ID: "p-neil", Name: "Neil"}},
		},
		{
			ID: "dup", Type: "IMAGE", OriginalFileName: "IMG_0001.jpg", Checksum: b64sha1(f.bytes["dup"]),
			FileCreatedAt: "2024-07-10T13:00:00.000Z", FileModifiedAt: "2024-07-10T13:00:00.000Z", LocalDateTime: "2024-07-10T15:00:00.000Z",
			Visibility: "timeline",
			ExifInfo:   &api.Exif{FileSizeInByte: size("dup")},
		},
		{
			ID: "trash", Type: "IMAGE", OriginalFileName: "gone.jpg", Checksum: b64sha1([]byte("gone")),
			IsTrashed: true, Visibility: "timeline",
		},
	}
	return f
}

func (f *fixture) handler(t *testing.T) http.Handler {
	t.Helper()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(v))
	}
	assetByID := func(id string) *api.Asset {
		for i := range f.assets {
			if f.assets[i].ID == id {
				return &f.assets[i]
			}
		}
		return nil
	}
	page := func(w http.ResponseWriter, items []api.Asset) {
		writeJSON(w, map[string]any{"assets": map[string]any{
			"total": len(items), "count": len(items), "items": items, "nextPage": nil,
		}})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "secret", r.Header.Get("X-Api-Key"))
		p := strings.TrimPrefix(r.URL.Path, "/api")
		switch {
		case p == "/server/version":
			writeJSON(w, api.ServerVersion{Major: 3, Minor: 4, Patch: 0})
		case p == "/users/me":
			writeJSON(w, api.User{ID: "u1", Email: "alice@example.org", Name: "Alice"})
		case p == "/api-keys/me":
			writeJSON(w, api.APIKey{Name: "backup", Permissions: []string{"all"}})
		case p == "/search/metadata":
			var req api.SearchRequest
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			switch {
			case len(req.AlbumIDs) == 1 && req.AlbumIDs[0] == "al1":
				page(w, []api.Asset{*assetByID("img"), *assetByID("arc"), {ID: "foreign", Checksum: b64sha1([]byte("not mine"))}})
			case len(req.TagIDs) == 1 && req.TagIDs[0] == "t-parent":
				page(w, []api.Asset{*assetByID("img"), *assetByID("arc")}) // the parent search includes the child's assets
			case len(req.TagIDs) == 1 && req.TagIDs[0] == "t-child":
				page(w, []api.Asset{*assetByID("arc")})
			default:
				assert.True(t, req.WithExif)
				assert.True(t, req.WithPeople)
				page(w, f.assets)
			}
		case p == "/assets/img":
			a := *assetByID("img")
			a.Tags = []api.Tag{{ID: "t-parent", Value: "poc"}}
			writeJSON(w, a)
		case p == "/assets/arc":
			a := *assetByID("arc")
			a.Tags = []api.Tag{{ID: "t-child", Value: "poc/holiday"}}
			writeJSON(w, a)
		case p == "/albums":
			writeJSON(w, []api.Album{{ID: "al1", AlbumName: "Trip/2024", Description: "d", Order: "desc", AlbumThumbnailAssetID: str("arc")}})
		case p == "/stacks":
			writeJSON(w, []api.Stack{{ID: "s1", PrimaryAssetID: "img", Assets: []api.Asset{*assetByID("dup"), *assetByID("img")}}})
		case p == "/tags":
			writeJSON(w, []api.Tag{{ID: "t-parent", Value: "poc"}, {ID: "t-child", Value: "poc/holiday", ParentID: str("t-parent")}})
		case p == "/people":
			writeJSON(w, map[string]any{"people": []api.Person{
				{ID: "p-neil", Name: "Neil", BirthDate: str("1930-08-05")},
				{ID: "p-unnamed", Name: ""},
			}, "hasNextPage": false, "total": 2})
		case p == "/faces":
			assert.Equal(t, "arc", r.URL.Query().Get("id"))
			writeJSON(w, []api.Face{
				{ID: "f1", BoundingBoxX1: 10, BoundingBoxY1: 20, BoundingBoxX2: 110, BoundingBoxY2: 220, ImageWidth: 800, ImageHeight: 600, Person: &api.Person{ID: "p-neil", Name: "Neil"}},
				{ID: "f2", BoundingBoxX1: 1, BoundingBoxY1: 1, BoundingBoxX2: 2, BoundingBoxY2: 2, ImageWidth: 800, ImageHeight: 600, Person: &api.Person{ID: "p-unnamed"}},
			})
		case p == "/asset-files":
			assert.Equal(t, "sidecar", r.URL.Query().Get("type"))
			id := r.URL.Query().Get("assetId")
			if _, ok := f.sidecars[id]; ok {
				writeJSON(w, []api.AssetFile{{ID: "file-" + id, Type: "sidecar"}})
			} else {
				writeJSON(w, []api.AssetFile{})
			}
		case strings.HasPrefix(p, "/asset-files/") && strings.HasSuffix(p, "/download"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/asset-files/file-"), "/download")
			_, _ = io.WriteString(w, f.sidecars[id])
		case strings.HasPrefix(p, "/assets/") && strings.HasSuffix(p, "/original"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/assets/"), "/original")
			b := f.bytes[id]
			if id == f.corrupt {
				b = append([]byte("X"), b[1:]...)
			}
			_, _ = w.Write(b)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	})
}

func newTestImporter(t *testing.T, f *fixture) (*Importer, *api.Client) {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	t.Setenv(EnvAPIKey, "secret")

	imp, err := NewImporter(context.Background(), &connectors.Options{MaxConcurrency: 2}, "immich",
		map[string]string{"location": "immich://" + strings.TrimPrefix(srv.URL, "http://")})
	require.NoError(t, err)
	i := imp.(*Importer)
	return i, i.client
}

func TestFold(t *testing.T) {
	f := newFixture()
	_, c := newTestImporter(t, f)

	st, err := Fold(context.Background(), c, 2)
	require.NoError(t, err)

	require.Equal(t, "v3.4.0", st.Manifest.Server.Version)
	require.Equal(t, "alice@example.org", st.Manifest.User.Email)
	require.Equal(t, []string{"all"}, st.Manifest.APIKey.Permissions)
	require.Equal(t, layout.Counts{Assets: 4, Sidecars: 1, Albums: 1, Stacks: 1, Tags: 2, People: 1}, st.Manifest.Counts)

	byID := make(map[string]*Entry)
	var paths []string
	for _, e := range st.Assets {
		byID[e.Asset.ImmichID] = e
		paths = append(paths, e.Path)
	}
	require.Len(t, byID, 4, "the trashed asset is skipped")

	// Layout: year/month from the local date, collision suffixed with 8 hex of the checksum.
	dupHex, _ := layout.HexChecksum(byID["dup"].Asset.Checksum)
	require.Equal(t, []string{
		"/assets/2023/08/beach.jpg",
		"/assets/2024/07/IMG_0001.jpg",
		"/assets/2024/07/IMG_0001.mov",
		"/assets/2024/07/IMG_0001-" + dupHex[:8] + ".jpg",
	}, paths)

	img := byID["img"].Asset
	require.Equal(t, byID["vid"].Asset.Checksum, img.LivePhotoVideo, "live photo link is kept by checksum")
	require.Equal(t, []string{"poc"}, img.Tags, "the parent tag search does not leak the child tag")
	require.Equal(t, int64(len(f.bytes["img"])), byID["img"].Size)

	arc := byID["arc"]
	require.Equal(t, []string{"poc/holiday"}, arc.Asset.Tags)
	require.Equal(t, []string{"Neil"}, arc.Asset.People)
	require.Equal(t, api.Duration("2000"), byID["vid"].Asset.Duration)
	require.Equal(t, api.Duration(""), img.Duration)
	require.True(t, arc.Asset.Sidecar)
	require.Equal(t, "file-arc", arc.SidecarFileID)
	require.Equal(t, "archive", arc.Asset.Visibility)
	require.True(t, arc.Asset.IsFavorite)
	require.Equal(t, 4, *arc.Asset.Exif.Rating)

	require.Equal(t, []string{"/albums/Trip_2024.json"}, st.AlbumPaths)
	require.Equal(t, layout.Album{
		Name: "Trip/2024", Description: "d", Order: "desc", Thumbnail: arc.Asset.Checksum,
		Assets: []string{img.Checksum, arc.Asset.Checksum},
	}, st.Albums[0], "assets of other users are left out of the album")

	imgHex, _ := layout.HexChecksum(img.Checksum)
	require.Equal(t, []string{"/stacks/" + imgHex + ".json"}, st.StackPaths)
	require.Equal(t, layout.Stack{Primary: img.Checksum, Assets: []string{img.Checksum, byID["dup"].Asset.Checksum}}, st.Stacks[0])

	require.Equal(t, []string{"/people/Neil.json"}, st.PersonPaths)
	require.Equal(t, layout.Person{
		Name: "Neil", BirthDate: str("1930-08-05"),
		Faces: []layout.Face{{Asset: arc.Asset.Checksum, ImageWidth: 800, ImageHeight: 600, X1: 10, Y1: 20, X2: 110, Y2: 220}},
	}, st.People[0], "unnamed people and their faces are not kept")
}

func TestImportRecords(t *testing.T) {
	f := newFixture()
	f.corrupt = "dup"
	imp, _ := newTestImporter(t, f)

	records := make(chan *connectors.Record, 64)
	_, err := imp.Import(context.Background(), records, nil)
	require.NoError(t, err)

	var order []string
	byPath := make(map[string]*connectors.Record)
	for r := range records {
		require.NoError(t, r.Err)
		order = append(order, r.Pathname)
		byPath[r.Pathname] = r
	}

	require.Equal(t, "/", order[0])
	require.Equal(t, layout.ManifestPath, order[1], "the manifest comes right after the root")
	require.Equal(t, []string{"/albums", "/assets", "/assets/2023", "/assets/2023/08", "/assets/2024", "/assets/2024/07", "/people", "/stacks"}, order[2:10],
		"every directory is sent before any file, parents first")
	for _, d := range order[2:10] {
		require.True(t, byPath[d].FileInfo.IsDir(), d)
	}
	require.Contains(t, order, "/assets/2023/08/beach.jpg.xmp")
	require.Contains(t, order, "/assets/2023/08/beach.jpg.json")
	require.NotContains(t, order, "/assets/2024/07/IMG_0001.jpg.xmp")

	var manifest layout.Manifest
	b, err := io.ReadAll(byPath[layout.ManifestPath].Reader)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &manifest))
	require.Equal(t, layout.Format, manifest.Format)
	require.Equal(t, int64(len(b)), byPath[layout.ManifestPath].FileInfo.Size())

	b, err = io.ReadAll(byPath["/assets/2024/07/IMG_0001.jpg"].Reader)
	require.NoError(t, err)
	require.Equal(t, "live photo image", string(b), "originals are streamed and verified")

	b, err = io.ReadAll(byPath["/assets/2023/08/beach.jpg.xmp"].Reader)
	require.NoError(t, err)
	require.Equal(t, "<x:xmpmeta/>", string(b))
	require.Equal(t, int64(-1), byPath["/assets/2023/08/beach.jpg.xmp"].FileInfo.Size(), "sidecar size is unknown until read")

	var meta layout.Asset
	b, err = io.ReadAll(byPath["/assets/2023/08/beach.jpg.json"].Reader)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &meta))
	require.Equal(t, "arc", meta.ImmichID)
	require.True(t, meta.Sidecar)

	var dupPath string
	for _, p := range order {
		if strings.HasPrefix(p, "/assets/2024/07/IMG_0001-") && strings.HasSuffix(p, ".jpg") {
			dupPath = p
		}
	}
	require.NotEmpty(t, dupPath)
	_, err = io.ReadAll(byPath[dupPath].Reader)
	require.ErrorContains(t, err, "checksum mismatch", "tampered bytes do not land in the snapshot silently")
}

func TestNewImporterConfig(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	_, err := NewImporter(context.Background(), &connectors.Options{}, "immich", map[string]string{"location": "immich://h"})
	require.ErrorContains(t, err, "missing API key")

	_, err = NewImporter(context.Background(), &connectors.Options{}, "immich", map[string]string{"location": "immich://h", "api_key": "k", "insecure": "maybe"})
	require.ErrorContains(t, err, "invalid value for insecure")

	imp, err := NewImporter(context.Background(), &connectors.Options{}, "immich", map[string]string{"location": "immich://h:1234", "api_key": "k"})
	require.NoError(t, err)
	require.Equal(t, "h:1234", imp.Origin())
	require.Equal(t, "immich", imp.Type())
	require.Equal(t, "/", imp.Root())
}
