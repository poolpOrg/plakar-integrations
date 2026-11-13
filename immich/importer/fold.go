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
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/poolpOrg/plakar-integrations/immich/api"
	"github.com/poolpOrg/plakar-integrations/immich/layout"
	"golang.org/x/sync/errgroup"
)

// Entry is one asset with everything the emitter needs: the side-file
// content, where it goes, and how to fetch its bytes.
type Entry struct {
	Asset   layout.Asset
	Path    string // /assets/YYYY/MM/<name>
	Hex     string // hex SHA1, for the collision suffix
	Size    int64  // of the original, -1 when unknown
	ModTime time.Time

	// SidecarFileID is the asset-files id of the XMP sidecar, "" if none.
	SidecarFileID string
}

// State is one user's Immich, folded in memory.
type State struct {
	Manifest layout.Manifest
	Assets   []*Entry
	Albums   []layout.Album
	Stacks   []layout.Stack
	People   []layout.Person

	// Paths of the albums, stacks and people files, parallel to the slices.
	AlbumPaths  []string
	StackPaths  []string
	PersonPaths []string
}

// Fold reads the user's whole state through the API-key endpoints.
// concurrency bounds the per-asset requests (sidecars, faces, tags).
func Fold(ctx context.Context, c *api.Client, concurrency int) (*State, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	st := &State{}

	if err := foldManifest(ctx, c, st); err != nil {
		return nil, err
	}

	// Every asset the key can see, with EXIF and the people on it.
	var assets []api.Asset
	err := c.Search(ctx, api.SearchRequest{WithExif: true, WithPeople: true}, func(a *api.Asset) error {
		if a.IsTrashed {
			return nil
		}
		assets = append(assets, *a)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list assets: %w", err)
	}

	// Stable order for the naming pass: by local date, then name, then
	// checksum, so two backups of the same library pick the same paths.
	sort.SliceStable(assets, func(i, j int) bool {
		if assets[i].LocalDateTime != assets[j].LocalDateTime {
			return assets[i].LocalDateTime < assets[j].LocalDateTime
		}
		if assets[i].OriginalFileName != assets[j].OriginalFileName {
			return assets[i].OriginalFileName < assets[j].OriginalFileName
		}
		return assets[i].Checksum < assets[j].Checksum
	})

	byID := make(map[string]*api.Asset, len(assets))
	for i := range assets {
		byID[assets[i].ID] = &assets[i]
	}
	checksumOf := func(id string) string {
		if a, ok := byID[id]; ok {
			return a.Checksum
		}
		return ""
	}

	namer := layout.NewNamer()
	namer.Reserve(layout.ManifestPath)
	entries := make([]*Entry, 0, len(assets))
	entryByID := make(map[string]*Entry, len(assets))
	for i := range assets {
		a := &assets[i]
		hexsum, err := layout.HexChecksum(a.Checksum)
		if err != nil {
			return nil, fmt.Errorf("asset %s (%s): %w", a.ID, a.OriginalFileName, err)
		}
		e := &Entry{
			Asset: layout.Asset{
				Checksum:         a.Checksum,
				ImmichID:         a.ID,
				Type:             a.Type,
				OriginalFileName: a.OriginalFileName,
				OriginalMimeType: a.OriginalMimeType,
				FileCreatedAt:    a.FileCreatedAt,
				FileModifiedAt:   a.FileModifiedAt,
				LocalDateTime:    a.LocalDateTime,
				IsFavorite:       a.IsFavorite,
				Visibility:       a.Visibility,
				Duration:         a.Duration,
				Width:            a.Width,
				Height:           a.Height,
				Exif:             a.ExifInfo,
			},
			Path:    namer.Unique(layout.AssetDir(a), a.OriginalFileName, hexsum),
			Hex:     hexsum,
			Size:    -1,
			ModTime: parseTime(a.FileModifiedAt),
		}
		if a.LivePhotoVideoID != nil {
			e.Asset.LivePhotoVideo = checksumOf(*a.LivePhotoVideoID)
		}
		if a.ExifInfo != nil && a.ExifInfo.FileSizeInByte != nil && *a.ExifInfo.FileSizeInByte > 0 {
			e.Size = *a.ExifInfo.FileSizeInByte
		}
		for _, p := range a.People {
			if p.Name != "" {
				e.Asset.People = append(e.Asset.People, p.Name)
			}
		}
		sort.Strings(e.Asset.People)
		entries = append(entries, e)
		entryByID[a.ID] = e
	}
	st.Assets = entries

	if err := foldAlbums(ctx, c, st, checksumOf); err != nil {
		return nil, err
	}
	if err := foldStacks(ctx, c, st); err != nil {
		return nil, err
	}
	if err := foldTags(ctx, c, st, entryByID, concurrency); err != nil {
		return nil, err
	}
	if err := foldPeople(ctx, c, st, assets, concurrency); err != nil {
		return nil, err
	}
	if err := foldSidecars(ctx, c, st, concurrency); err != nil {
		return nil, err
	}

	st.Manifest.Counts.Assets = len(st.Assets)
	st.Manifest.Counts.Albums = len(st.Albums)
	st.Manifest.Counts.Stacks = len(st.Stacks)
	st.Manifest.Counts.People = len(st.People)
	for _, e := range st.Assets {
		if e.SidecarFileID != "" {
			st.Manifest.Counts.Sidecars++
		}
	}
	return st, nil
}

func foldManifest(ctx context.Context, c *api.Client, st *State) error {
	v, err := c.ServerVersion(ctx)
	if err != nil {
		return fmt.Errorf("server version: %w", err)
	}
	me, err := c.Me(ctx)
	if err != nil {
		return fmt.Errorf("current user: %w", err)
	}
	st.Manifest = layout.Manifest{
		Format:     layout.Format,
		ExportedAt: time.Now().UTC(),
		Server: layout.Server{
			URL:     c.BaseURL(),
			Version: fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch),
		},
		User: *me,
	}
	// The key endpoint is informational; an older server without it, or a
	// key that may not read keys, does not stop the backup.
	if k, err := c.APIKeyMe(ctx); err == nil {
		st.Manifest.APIKey = layout.APIKey{Name: k.Name, Permissions: k.Permissions}
	} else if !api.IsStatus(err, 404) && !api.IsStatus(err, 403) {
		return fmt.Errorf("api key: %w", err)
	}
	return nil
}

func foldAlbums(ctx context.Context, c *api.Client, st *State, checksumOf func(string) string) error {
	albums, err := c.Albums(ctx)
	if err != nil {
		return fmt.Errorf("list albums: %w", err)
	}
	sort.SliceStable(albums, func(i, j int) bool { return albums[i].AlbumName < albums[j].AlbumName })

	namer := layout.NewNamer()
	for _, al := range albums {
		out := layout.Album{
			Name:        al.AlbumName,
			Description: al.Description,
			Order:       al.Order,
			Assets:      []string{},
		}
		if al.AlbumThumbnailAssetID != nil {
			out.Thumbnail = checksumOf(*al.AlbumThumbnailAssetID)
		}
		// Album membership is not in the album DTO any more (3.x); the
		// search endpoint filters by album.
		err := c.Search(ctx, api.SearchRequest{AlbumIDs: []string{al.ID}}, func(a *api.Asset) error {
			if a.IsTrashed {
				return nil
			}
			if checksumOf(a.ID) == "" {
				// An asset shared into the album by another user; it is
				// not ours to back up and could not be restored by this
				// key anyway.
				return nil
			}
			out.Assets = append(out.Assets, a.Checksum)
			return nil
		})
		if err != nil {
			return fmt.Errorf("album %q: %w", al.AlbumName, err)
		}
		st.Albums = append(st.Albums, out)
		st.AlbumPaths = append(st.AlbumPaths, namer.Unique(layout.AlbumsDir, al.AlbumName+".json", strings.ReplaceAll(al.ID, "-", "")))
	}
	return nil
}

func foldStacks(ctx context.Context, c *api.Client, st *State) error {
	stacks, err := c.Stacks(ctx)
	if err != nil {
		return fmt.Errorf("list stacks: %w", err)
	}
	for _, s := range stacks {
		out := layout.Stack{}
		for _, a := range s.Assets {
			if a.ID == s.PrimaryAssetID {
				out.Primary = a.Checksum
			}
		}
		if out.Primary == "" {
			continue
		}
		out.Assets = append(out.Assets, out.Primary)
		for _, a := range s.Assets {
			if a.ID != s.PrimaryAssetID {
				out.Assets = append(out.Assets, a.Checksum)
			}
		}
		hexsum, err := layout.HexChecksum(out.Primary)
		if err != nil {
			return fmt.Errorf("stack %s: %w", s.ID, err)
		}
		st.Stacks = append(st.Stacks, out)
		st.StackPaths = append(st.StackPaths, layout.StacksDir+"/"+hexsum+".json")
	}
	return nil
}

// foldTags finds the tagged assets through one search per tag, then reads
// each tagged asset once: a search by a parent tag also returns the assets
// tagged with its children, and only GET /assets/{id} says which tag an
// asset really carries.
func foldTags(ctx context.Context, c *api.Client, st *State, entryByID map[string]*Entry, concurrency int) error {
	tags, err := c.Tags(ctx)
	if err != nil {
		return fmt.Errorf("list tags: %w", err)
	}
	st.Manifest.Counts.Tags = len(tags)
	if len(tags) == 0 {
		return nil
	}

	tagged := make(map[string]struct{})
	for _, t := range tags {
		err := c.Search(ctx, api.SearchRequest{TagIDs: []string{t.ID}}, func(a *api.Asset) error {
			if _, ok := entryByID[a.ID]; ok {
				tagged[a.ID] = struct{}{}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("tag %q: %w", t.Value, err)
		}
	}

	ids := make([]string, 0, len(tagged))
	for id := range tagged {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for _, id := range ids {
		e := entryByID[id]
		g.Go(func() error {
			a, err := c.Asset(gctx, id)
			if err != nil {
				return fmt.Errorf("asset %s: %w", e.Asset.OriginalFileName, err)
			}
			values := make([]string, 0, len(a.Tags))
			for _, t := range a.Tags {
				values = append(values, t.Value)
			}
			sort.Strings(values)
			e.Asset.Tags = values
			return nil
		})
	}
	return g.Wait()
}

func foldPeople(ctx context.Context, c *api.Client, st *State, assets []api.Asset, concurrency int) error {
	people, err := c.People(ctx)
	if err != nil {
		return fmt.Errorf("list people: %w", err)
	}

	// Named people only: an unnamed cluster is ML's to find again.
	persons := make(map[string]*layout.Person)
	var order []string
	for _, p := range people {
		if p.Name == "" {
			continue
		}
		persons[p.ID] = &layout.Person{
			Name:       p.Name,
			BirthDate:  p.BirthDate,
			IsFavorite: p.IsFavorite,
			IsHidden:   p.IsHidden,
			Faces:      []layout.Face{},
		}
		order = append(order, p.ID)
	}
	if len(persons) == 0 {
		return nil
	}

	// Faces live per asset; ask only for the assets the search said have
	// people on them.
	var withPeople []*api.Asset
	for i := range assets {
		if len(assets[i].People) > 0 {
			withPeople = append(withPeople, &assets[i])
		}
	}
	faces := make([][]api.Face, len(withPeople))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for i, a := range withPeople {
		g.Go(func() error {
			f, err := c.Faces(gctx, a.ID)
			if err != nil {
				return fmt.Errorf("faces of %s: %w", a.OriginalFileName, err)
			}
			faces[i] = f
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	for i, a := range withPeople {
		for _, f := range faces[i] {
			if f.Person == nil {
				continue
			}
			p, ok := persons[f.Person.ID]
			if !ok {
				continue
			}
			p.Faces = append(p.Faces, layout.Face{
				Asset:       a.Checksum,
				ImageWidth:  f.ImageWidth,
				ImageHeight: f.ImageHeight,
				X1:          f.BoundingBoxX1,
				Y1:          f.BoundingBoxY1,
				X2:          f.BoundingBoxX2,
				Y2:          f.BoundingBoxY2,
			})
		}
	}

	sort.SliceStable(order, func(i, j int) bool { return persons[order[i]].Name < persons[order[j]].Name })
	namer := layout.NewNamer()
	for _, id := range order {
		p := persons[id]
		st.People = append(st.People, *p)
		st.PersonPaths = append(st.PersonPaths, namer.Unique(layout.PeopleDir, p.Name+".json", strings.ReplaceAll(id, "-", "")))
	}
	return nil
}

func foldSidecars(ctx context.Context, c *api.Client, st *State, concurrency int) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for _, e := range st.Assets {
		g.Go(func() error {
			files, err := c.AssetFiles(gctx, e.Asset.ImmichID, "sidecar")
			if err != nil {
				return fmt.Errorf("sidecar of %s: %w", e.Asset.OriginalFileName, err)
			}
			for _, f := range files {
				if f.Type == "sidecar" {
					e.SidecarFileID = f.ID
					e.Asset.Sidecar = true
					break
				}
			}
			return nil
		})
	}
	return g.Wait()
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Now().UTC()
	}
	return t
}
