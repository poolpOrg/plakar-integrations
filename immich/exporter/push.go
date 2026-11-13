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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/poolpOrg/plakar-integrations/immich/api"
	"github.com/poolpOrg/plakar-integrations/immich/layout"
)

const (
	// maxSideFile bounds the side files we decode from the staging area.
	maxSideFile = 16 << 20

	// deviceID is what the uploads claim to come from.
	deviceID = "plakar"
)

// metadataWait is how long the fix-up pass waits for the server to extract
// an uploaded asset's metadata before giving up on it; variables so tests
// can shorten them.
var (
	metadataWait = 2 * time.Minute
	metadataPoll = 500 * time.Millisecond
)

// stagedAsset is one asset read back from the staging area.
type stagedAsset struct {
	Meta     layout.Asset
	Original string // staging-relative path of the original
	Sidecar  string // staging-relative path of the sidecar, "" if none

	ID       string // id on the target once known
	Existing bool   // already on the target before this restore
}

// staged is the snapshot as read back from the staging area.
type staged struct {
	assets []*stagedAsset
	albums []layout.Album
	stacks []layout.Stack
	people []layout.Person
}

// push sends the staged snapshot to the server, in the order the relations
// need: assets get ids first, everything else joins on them, and a last
// pass fixes what the upload did not carry.  Steps keep going past
// per-item failures and the errors come back joined.
func (e *Exporter) push(ctx context.Context) error {
	st, err := e.load()
	if err != nil {
		return err
	}
	if len(st.assets) == 0 && len(st.albums) == 0 && len(st.people) == 0 {
		return nil
	}

	var errs []error
	fail := func(err error) { errs = append(errs, err) }

	if err := e.checkDuplicates(ctx, st); err != nil {
		return err
	}
	e.uploadAll(ctx, st, fail)

	ids := make(map[string]string, len(st.assets))
	existing := make(map[string]bool, len(st.assets))
	for _, a := range st.assets {
		if a.ID != "" {
			ids[a.Meta.Checksum] = a.ID
			existing[a.ID] = a.Existing
		}
	}

	e.pushAlbums(ctx, st, ids, fail)
	e.pushStacks(ctx, st, ids, fail)
	e.pushTags(ctx, st, fail)
	e.pushPeople(ctx, st, ids, existing, fail)
	e.fixup(ctx, st, fail)

	return errors.Join(errs...)
}

// load walks the staging area and decodes the side files.  Anything that
// is not part of the layout is ignored.
func (e *Exporter) load() (*staged, error) {
	st := &staged{}
	fsys := e.root.FS()

	readJSON := func(p string, v any) error {
		f, err := fsys.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(io.LimitReader(f, maxSideFile+1))
		if err != nil {
			return err
		}
		if len(b) > maxSideFile {
			return fmt.Errorf("%s: larger than %d bytes", p, maxSideFile)
		}
		if err := json.Unmarshal(b, v); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		return nil
	}

	exists := func(p string) bool {
		_, err := fs.Stat(fsys, p)
		return err == nil
	}

	walk := func(dir string, fn func(p string) error) error {
		if !exists(dir) {
			return nil
		}
		return fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".json") {
				return nil
			}
			return fn(p)
		})
	}

	err := walk(strings.TrimPrefix(layout.AssetsDir, "/"), func(p string) error {
		var meta layout.Asset
		if err := readJSON(p, &meta); err != nil {
			return err
		}
		if meta.Checksum == "" {
			return fmt.Errorf("%s: no checksum", p)
		}
		original := strings.TrimSuffix(p, ".json")
		if !exists(original) {
			return fmt.Errorf("%s: original %s is not in the snapshot", p, path.Base(original))
		}
		a := &stagedAsset{Meta: meta, Original: original}
		if meta.Sidecar && exists(original+".xmp") {
			a.Sidecar = original + ".xmp"
		}
		st.assets = append(st.assets, a)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read assets: %w", err)
	}
	sort.SliceStable(st.assets, func(i, j int) bool { return st.assets[i].Original < st.assets[j].Original })

	err = walk(strings.TrimPrefix(layout.AlbumsDir, "/"), func(p string) error {
		var al layout.Album
		if err := readJSON(p, &al); err != nil {
			return err
		}
		st.albums = append(st.albums, al)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read albums: %w", err)
	}

	err = walk(strings.TrimPrefix(layout.StacksDir, "/"), func(p string) error {
		var s layout.Stack
		if err := readJSON(p, &s); err != nil {
			return err
		}
		st.stacks = append(st.stacks, s)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read stacks: %w", err)
	}

	err = walk(strings.TrimPrefix(layout.PeopleDir, "/"), func(p string) error {
		var person layout.Person
		if err := readJSON(p, &person); err != nil {
			return err
		}
		st.people = append(st.people, person)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read people: %w", err)
	}
	return st, nil
}

// checkDuplicates asks the server which checksums it already holds, so
// those assets keep their existing id and are not uploaded again.
func (e *Exporter) checkDuplicates(ctx context.Context, st *staged) error {
	const batch = 500
	byChecksum := make(map[string]*stagedAsset, len(st.assets))
	req := make([]api.BulkCheckAsset, 0, len(st.assets))
	for _, a := range st.assets {
		byChecksum[a.Meta.Checksum] = a
		req = append(req, api.BulkCheckAsset{ID: a.Meta.Checksum, Checksum: a.Meta.Checksum})
	}

	for start := 0; start < len(req); start += batch {
		end := min(start+batch, len(req))
		results, err := e.client.BulkUploadCheck(ctx, req[start:end])
		if err != nil {
			return fmt.Errorf("bulk upload check: %w", err)
		}
		for _, r := range results {
			a, ok := byChecksum[r.ID]
			if !ok {
				continue
			}
			if r.Action == "reject" && r.Reason != nil && *r.Reason == "duplicate" && r.AssetID != nil {
				a.ID = *r.AssetID
				a.Existing = true
			}
		}
	}
	return nil
}

// uploadAll uploads what the server does not have: the videos of live
// photos first, so the images can point at them.
func (e *Exporter) uploadAll(ctx context.Context, st *staged, fail func(error)) {
	liveVideos := make(map[string]bool)
	for _, a := range st.assets {
		if a.Meta.LivePhotoVideo != "" {
			liveVideos[a.Meta.LivePhotoVideo] = true
		}
	}

	ids := make(map[string]string)
	for _, a := range st.assets {
		if a.ID != "" {
			ids[a.Meta.Checksum] = a.ID
		}
	}

	order := make([]*stagedAsset, 0, len(st.assets))
	for _, a := range st.assets {
		if liveVideos[a.Meta.Checksum] {
			order = append(order, a)
		}
	}
	for _, a := range st.assets {
		if !liveVideos[a.Meta.Checksum] {
			order = append(order, a)
		}
	}

	for _, a := range order {
		if a.ID != "" {
			continue
		}
		res, err := e.upload(ctx, a, ids[a.Meta.LivePhotoVideo])
		if err != nil {
			fail(err)
			continue
		}
		a.ID = res.ID
		a.Existing = res.Status == "duplicate"
		ids[a.Meta.Checksum] = res.ID
	}
}

func (e *Exporter) upload(ctx context.Context, a *stagedAsset, liveVideoID string) (*api.UploadResult, error) {
	hexsum, err := layout.HexChecksum(a.Meta.Checksum)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", a.Meta.OriginalFileName, err)
	}

	up := api.Upload{
		Filename:       a.Meta.OriginalFileName,
		Open:           func() (io.ReadCloser, error) { return e.root.Open(a.Original) },
		ChecksumHex:    hexsum,
		DeviceAssetID:  hexsum,
		DeviceID:       deviceID,
		FileCreatedAt:  a.Meta.FileCreatedAt,
		FileModifiedAt: a.Meta.FileModifiedAt,
		IsFavorite:     a.Meta.IsFavorite,
	}
	if a.Sidecar != "" {
		up.OpenSidecar = func() (io.ReadCloser, error) { return e.root.Open(a.Sidecar) }
	}
	switch a.Meta.Visibility {
	case "archive", "timeline", "locked":
		up.Visibility = a.Meta.Visibility
	}
	up.Duration = string(a.Meta.Duration)
	if a.Meta.LivePhotoVideo != "" {
		if liveVideoID == "" {
			return nil, fmt.Errorf("%s: its live photo video is not on the target", a.Meta.OriginalFileName)
		}
		up.LivePhotoVideoID = liveVideoID
	}

	res, err := e.client.UploadAsset(ctx, up)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// pushAlbums creates each album, or adds to an album of the same name the
// user already has, then sets the thumbnail and the order.
func (e *Exporter) pushAlbums(ctx context.Context, st *staged, ids map[string]string, fail func(error)) {
	if len(st.albums) == 0 {
		return
	}
	existing, err := e.client.Albums(ctx)
	if err != nil {
		fail(fmt.Errorf("list albums: %w", err))
		return
	}
	byName := make(map[string]string, len(existing))
	for _, al := range existing {
		if _, dup := byName[al.AlbumName]; !dup {
			byName[al.AlbumName] = al.ID
		}
	}

	for _, al := range st.albums {
		assetIDs := make([]string, 0, len(al.Assets))
		for _, sum := range al.Assets {
			if id, ok := ids[sum]; ok {
				assetIDs = append(assetIDs, id)
			}
		}

		albumID, ok := byName[al.Name]
		if ok {
			if len(assetIDs) > 0 {
				if _, err := e.client.AddAlbumAssets(ctx, albumID, assetIDs); err != nil {
					fail(fmt.Errorf("album %q: add assets: %w", al.Name, err))
					continue
				}
			}
		} else {
			created, err := e.client.CreateAlbum(ctx, api.CreateAlbum{
				AlbumName:   al.Name,
				Description: al.Description,
				AssetIDs:    assetIDs,
			})
			if err != nil {
				fail(fmt.Errorf("album %q: %w", al.Name, err))
				continue
			}
			albumID = created.ID
			byName[al.Name] = albumID
		}

		upd := api.UpdateAlbum{}
		if al.Order != "" {
			order := al.Order
			upd.Order = &order
		}
		if id, ok := ids[al.Thumbnail]; ok && al.Thumbnail != "" {
			upd.AlbumThumbnailAssetID = &id
		}
		if upd.Order == nil && upd.AlbumThumbnailAssetID == nil {
			continue
		}
		if err := e.client.UpdateAlbum(ctx, albumID, upd); err != nil {
			fail(fmt.Errorf("album %q: update: %w", al.Name, err))
		}
	}
}

func (e *Exporter) pushStacks(ctx context.Context, st *staged, ids map[string]string, fail func(error)) {
	for _, s := range st.stacks {
		assetIDs := make([]string, 0, len(s.Assets))
		if id, ok := ids[s.Primary]; ok {
			assetIDs = append(assetIDs, id)
		}
		for _, sum := range s.Assets {
			if sum == s.Primary {
				continue
			}
			if id, ok := ids[sum]; ok {
				assetIDs = append(assetIDs, id)
			}
		}
		if len(assetIDs) < 2 {
			continue
		}
		if _, err := e.client.CreateStack(ctx, assetIDs); err != nil {
			fail(fmt.Errorf("stack of %s: %w", s.Primary, err))
		}
	}
}

// pushTags creates the tag paths and tags the assets that carry them.
func (e *Exporter) pushTags(ctx context.Context, st *staged, fail func(error)) {
	assetsByTag := make(map[string][]string)
	for _, a := range st.assets {
		if a.ID == "" {
			continue
		}
		for _, t := range a.Meta.Tags {
			assetsByTag[t] = append(assetsByTag[t], a.ID)
		}
	}
	if len(assetsByTag) == 0 {
		return
	}

	values := make([]string, 0, len(assetsByTag))
	for v := range assetsByTag {
		values = append(values, v)
	}
	sort.Strings(values)

	tags, err := e.client.UpsertTags(ctx, values)
	if err != nil {
		fail(fmt.Errorf("tags: %w", err))
		return
	}
	idByValue := make(map[string]string, len(tags))
	for _, t := range tags {
		idByValue[t.Value] = t.ID
	}

	for _, v := range values {
		id, ok := idByValue[v]
		if !ok {
			fail(fmt.Errorf("tag %q: server did not return it", v))
			continue
		}
		if err := e.client.TagAssets(ctx, []string{id}, assetsByTag[v]); err != nil {
			fail(fmt.Errorf("tag %q: %w", v, err))
		}
	}
}

// pushPeople creates each person, or reuses one of the same name, then
// posts the stored face boxes.  Assets that already existed on the target
// keep the faces they have.
func (e *Exporter) pushPeople(ctx context.Context, st *staged, ids map[string]string, existing map[string]bool, fail func(error)) {
	if len(st.people) == 0 {
		return
	}
	people, err := e.client.People(ctx)
	if err != nil {
		fail(fmt.Errorf("list people: %w", err))
		return
	}
	byName := make(map[string]string, len(people))
	for _, p := range people {
		if p.Name == "" {
			continue
		}
		if _, dup := byName[p.Name]; !dup {
			byName[p.Name] = p.ID
		}
	}

	facesOf := make(map[string][]api.Face) // asset id -> faces, for existing assets

	for _, p := range st.people {
		if p.Name == "" {
			continue
		}
		personID, ok := byName[p.Name]
		if !ok {
			created, err := e.client.CreatePerson(ctx, api.CreatePerson{
				Name:       p.Name,
				BirthDate:  p.BirthDate,
				IsFavorite: p.IsFavorite,
				IsHidden:   p.IsHidden,
			})
			if err != nil {
				fail(fmt.Errorf("person %q: %w", p.Name, err))
				continue
			}
			personID = created.ID
			byName[p.Name] = personID
		}

		for _, f := range p.Faces {
			assetID, ok := ids[f.Asset]
			if !ok {
				continue
			}
			if existing[assetID] {
				faces, ok := facesOf[assetID]
				if !ok {
					faces, err = e.client.Faces(ctx, assetID)
					if err != nil {
						fail(fmt.Errorf("person %q: faces of %s: %w", p.Name, assetID, err))
						continue
					}
					facesOf[assetID] = faces
				}
				if hasPerson(faces, personID) {
					continue
				}
			}
			err := e.client.CreateFace(ctx, api.CreateFace{
				AssetID:     assetID,
				PersonID:    personID,
				ImageWidth:  f.ImageWidth,
				ImageHeight: f.ImageHeight,
				X:           f.X1,
				Y:           f.Y1,
				Width:       f.X2 - f.X1,
				Height:      f.Y2 - f.Y1,
			})
			if err != nil {
				fail(fmt.Errorf("person %q: face on %s: %w", p.Name, assetID, err))
			}
		}
	}
}

func hasPerson(faces []api.Face, personID string) bool {
	for _, f := range faces {
		if f.Person != nil && f.Person.ID == personID {
			return true
		}
	}
	return false
}

// fixup re-reads every asset this restore uploaded once the server has
// extracted its metadata, and sends only the fields that still differ,
// with the time zone: a blind date update after upload resets the zone.
func (e *Exporter) fixup(ctx context.Context, st *staged, fail func(error)) {
	// One deadline for the pass, not one per asset: a paused extraction
	// queue must not turn the restore into hours of polling.
	deadline := time.Now().Add(metadataWait)
	for _, a := range st.assets {
		if a.ID == "" || a.Existing {
			continue
		}
		cur, err := e.waitMetadata(ctx, a.ID, deadline)
		if errors.Is(err, errMetadataTimeout) {
			e.warnf("%s: metadata not extracted after %s, leaving it as uploaded", a.Meta.OriginalFileName, metadataWait)
			continue
		}
		if err != nil {
			fail(fmt.Errorf("%s: %w", a.Meta.OriginalFileName, err))
			continue
		}
		upd := diffAsset(&a.Meta, cur)
		if upd.IsEmpty() {
			continue
		}
		upd.IDs = []string{a.ID}
		if err := e.client.BulkUpdateAssets(ctx, upd); err != nil {
			fail(fmt.Errorf("%s: update: %w", a.Meta.OriginalFileName, err))
		}
	}
}

// errMetadataTimeout is waitMetadata giving up.
var errMetadataTimeout = errors.New("metadata extraction did not finish in time")

// waitMetadata polls the asset until the metadata extraction job has run.
// The upload itself already creates an EXIF row holding the file size, so
// the file size says nothing; the date is what the job fills in, from the
// EXIF, the sidecar, or fileCreatedAt as a last resort.
func (e *Exporter) waitMetadata(ctx context.Context, id string, deadline time.Time) (*api.Asset, error) {
	for {
		a, err := e.client.Asset(ctx, id)
		if err != nil {
			return nil, err
		}
		if a.ExifInfo != nil && a.ExifInfo.DateTimeOriginal != nil {
			return a, nil
		}
		if time.Now().After(deadline) {
			return nil, errMetadataTimeout
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(metadataPoll):
		}
	}
}

// diffAsset is what has to change on cur to match src.
func diffAsset(src *layout.Asset, cur *api.Asset) api.BulkUpdate {
	var upd api.BulkUpdate

	if src.IsFavorite != cur.IsFavorite {
		v := src.IsFavorite
		upd.IsFavorite = &v
	}
	switch src.Visibility {
	case "archive", "timeline":
		if src.Visibility != cur.Visibility {
			v := src.Visibility
			upd.Visibility = &v
		}
	}

	if src.Exif == nil {
		return upd
	}
	se, ce := src.Exif, cur.ExifInfo
	if ce == nil {
		ce = &api.Exif{}
	}

	hasGPS := se.Latitude != nil && se.Longitude != nil
	if hasGPS && (ce.Latitude == nil || ce.Longitude == nil ||
		math.Abs(*ce.Latitude-*se.Latitude) > 1e-6 || math.Abs(*ce.Longitude-*se.Longitude) > 1e-6) {
		lat, lon := *se.Latitude, *se.Longitude
		upd.Latitude, upd.Longitude = &lat, &lon
	}

	if se.DateTimeOriginal != nil {
		dateDiffers := ce.DateTimeOriginal == nil || !sameInstant(*se.DateTimeOriginal, *ce.DateTimeOriginal)
		zoneDiffers := se.TimeZone != nil && (ce.TimeZone == nil || *ce.TimeZone != *se.TimeZone)
		// With GPS the zone follows the coordinates, so only the instant
		// can be fixed there.
		if dateDiffers || (zoneDiffers && !hasGPS) {
			d := *se.DateTimeOriginal
			upd.DateTimeOriginal = &d
			if se.TimeZone != nil && !hasGPS {
				z := *se.TimeZone
				upd.TimeZone = &z
			}
		}
	}

	if deref(se.Description) != deref(ce.Description) {
		d := deref(se.Description)
		upd.Description = &d
	}
	if derefInt(se.Rating) != derefInt(ce.Rating) {
		r := derefInt(se.Rating)
		upd.Rating = &r
	}
	return upd
}

func sameInstant(a, b string) bool {
	ta, errA := time.Parse(time.RFC3339Nano, a)
	tb, errB := time.Parse(time.RFC3339Nano, b)
	if errA != nil || errB != nil {
		return a == b
	}
	return ta.Equal(tb)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(i *int) int {
	if i == nil {
		return 0
	}
	return *i
}

func (e *Exporter) warnf(format string, args ...any) {
	var w io.Writer = os.Stderr
	if e.opts != nil && e.opts.Stderr != nil {
		w = e.opts.Stderr
	}
	_, _ = fmt.Fprintf(w, "immich: "+format+"\n", args...)
}
