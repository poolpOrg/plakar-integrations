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

package api

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strconv"
)

// searchPageSize is how many assets one POST /search/metadata page carries.
const searchPageSize = 500

// ServerVersion is GET /server/version.
func (c *Client) ServerVersion(ctx context.Context) (*ServerVersion, error) {
	var v ServerVersion
	if err := c.get(ctx, "/server/version", nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Me is GET /users/me, the owner of the API key.
func (c *Client) Me(ctx context.Context) (*User, error) {
	var u User
	if err := c.get(ctx, "/users/me", nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// APIKeyMe is GET /api-keys/me, the key in use and its permissions.
func (c *Client) APIKeyMe(ctx context.Context) (*APIKey, error) {
	var k APIKey
	if err := c.get(ctx, "/api-keys/me", nil, &k); err != nil {
		return nil, err
	}
	return &k, nil
}

// Search walks every page of POST /search/metadata for req and hands each
// asset to fn.  Page and Size in req are overridden.
func (c *Client) Search(ctx context.Context, req SearchRequest, fn func(*Asset) error) error {
	req.Size = searchPageSize
	for page := 1; ; page++ {
		req.Page = page
		var resp searchResponse
		if err := c.post(ctx, "/search/metadata", &req, &resp); err != nil {
			return err
		}
		for i := range resp.Assets.Items {
			if err := fn(&resp.Assets.Items[i]); err != nil {
				return err
			}
		}
		if resp.Assets.NextPage == nil || len(resp.Assets.Items) == 0 {
			return nil
		}
		// The server answers the page number it expects next; keep
		// counting ourselves but refuse to loop forever on a server
		// that always announces one.
		next, err := strconv.Atoi(*resp.Assets.NextPage)
		if err != nil || next != page+1 {
			return fmt.Errorf("POST /search/metadata: unexpected next page %q after page %d", *resp.Assets.NextPage, page)
		}
	}
}

// Asset is GET /assets/{id}, the full DTO with tags and people.
func (c *Client) Asset(ctx context.Context, id string) (*Asset, error) {
	var a Asset
	if err := c.get(ctx, "/assets/"+url.PathEscape(id), nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// Albums is GET /albums, the albums the user owns or shares.
func (c *Client) Albums(ctx context.Context) ([]Album, error) {
	var albums []Album
	if err := c.get(ctx, "/albums", nil, &albums); err != nil {
		return nil, err
	}
	return albums, nil
}

// Stacks is GET /stacks.
func (c *Client) Stacks(ctx context.Context) ([]Stack, error) {
	var stacks []Stack
	if err := c.get(ctx, "/stacks", nil, &stacks); err != nil {
		return nil, err
	}
	return stacks, nil
}

// Tags is GET /tags, every tag of the user with its full path in Value.
func (c *Client) Tags(ctx context.Context) ([]Tag, error) {
	var tags []Tag
	if err := c.get(ctx, "/tags", nil, &tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// People walks every page of GET /people, hidden ones included.
func (c *Client) People(ctx context.Context) ([]Person, error) {
	var people []Person
	for page := 1; ; page++ {
		q := url.Values{
			"withHidden": {"true"},
			"page":       {strconv.Itoa(page)},
			"size":       {"500"},
		}
		var resp peopleResponse
		if err := c.get(ctx, "/people", q, &resp); err != nil {
			return nil, err
		}
		people = append(people, resp.People...)
		if !resp.HasNextPage || len(resp.People) == 0 {
			return people, nil
		}
	}
}

// Faces is GET /faces?id=<asset>, the faces on one asset.
func (c *Client) Faces(ctx context.Context, assetID string) ([]Face, error) {
	var faces []Face
	if err := c.get(ctx, "/faces", url.Values{"id": {assetID}}, &faces); err != nil {
		return nil, err
	}
	return faces, nil
}

// AssetFiles is GET /asset-files?assetId=&type=, the server-side files of
// one asset of that type (sidecar, preview, thumbnail).
func (c *Client) AssetFiles(ctx context.Context, assetID, typ string) ([]AssetFile, error) {
	var files []AssetFile
	q := url.Values{"assetId": {assetID}}
	if typ != "" {
		q.Set("type", typ)
	}
	if err := c.get(ctx, "/asset-files", q, &files); err != nil {
		return nil, err
	}
	return files, nil
}

// Original streams GET /assets/{id}/original.  size bounds the body when
// known; pass 0 to accept whatever Content-Length says.
func (c *Client) Original(ctx context.Context, id string, size int64) (io.ReadCloser, error) {
	return c.Download(ctx, "/assets/"+url.PathEscape(id)+"/original", size)
}

// AssetFileDownload streams GET /asset-files/{id}/download.
func (c *Client) AssetFileDownload(ctx context.Context, fileID string) (io.ReadCloser, error) {
	return c.Download(ctx, "/asset-files/"+url.PathEscape(fileID)+"/download", MaxSidecarSize)
}

// BulkUploadCheck is POST /assets/bulk-upload-check: which of these
// checksums the server already holds.
func (c *Client) BulkUploadCheck(ctx context.Context, assets []BulkCheckAsset) ([]BulkCheckResult, error) {
	var resp struct {
		Results []BulkCheckResult `json:"results"`
	}
	in := struct {
		Assets []BulkCheckAsset `json:"assets"`
	}{Assets: assets}
	if err := c.post(ctx, "/assets/bulk-upload-check", &in, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// CreateAlbum is POST /albums.
func (c *Client) CreateAlbum(ctx context.Context, in CreateAlbum) (*Album, error) {
	var a Album
	if err := c.post(ctx, "/albums", &in, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// AddAlbumAssets is PUT /albums/{id}/assets.
func (c *Client) AddAlbumAssets(ctx context.Context, albumID string, assetIDs []string) ([]BulkIDResult, error) {
	in := struct {
		IDs []string `json:"ids"`
	}{IDs: assetIDs}
	var out []BulkIDResult
	if err := c.put(ctx, "/albums/"+url.PathEscape(albumID)+"/assets", &in, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateAlbum is PATCH /albums/{id}.
func (c *Client) UpdateAlbum(ctx context.Context, albumID string, in UpdateAlbum) error {
	return c.patch(ctx, "/albums/"+url.PathEscape(albumID), &in, nil)
}

// CreateStack is POST /stacks; the first id becomes the primary asset.
func (c *Client) CreateStack(ctx context.Context, assetIDs []string) (*Stack, error) {
	in := struct {
		AssetIDs []string `json:"assetIds"`
	}{AssetIDs: assetIDs}
	var s Stack
	if err := c.post(ctx, "/stacks", &in, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// UpsertTags is PUT /tags: creates the paths that do not exist yet and
// returns every tag named, with ids.
func (c *Client) UpsertTags(ctx context.Context, values []string) ([]Tag, error) {
	in := struct {
		Tags []string `json:"tags"`
	}{Tags: values}
	var out []Tag
	if err := c.put(ctx, "/tags", &in, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// TagAssets is PUT /tags/assets: puts these tags on these assets.
func (c *Client) TagAssets(ctx context.Context, tagIDs, assetIDs []string) error {
	in := struct {
		TagIDs   []string `json:"tagIds"`
		AssetIDs []string `json:"assetIds"`
	}{TagIDs: tagIDs, AssetIDs: assetIDs}
	return c.put(ctx, "/tags/assets", &in, nil)
}

// CreatePerson is POST /people.
func (c *Client) CreatePerson(ctx context.Context, in CreatePerson) (*Person, error) {
	var p Person
	if err := c.post(ctx, "/people", &in, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// CreateFace is POST /faces: a manual face box for a person on an asset.
func (c *Client) CreateFace(ctx context.Context, in CreateFace) error {
	return c.post(ctx, "/faces", &in, nil)
}

// BulkUpdateAssets is PUT /assets.
func (c *Client) BulkUpdateAssets(ctx context.Context, in BulkUpdate) error {
	return c.put(ctx, "/assets", &in, nil)
}
