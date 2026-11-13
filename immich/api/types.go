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
	"encoding/json"
	"fmt"
	"strings"
)

// The DTOs below carry the fields we read or write, named as the Immich
// OpenAPI spec (3.4.0) names them.  Nullable fields are pointers.

// Duration is a video duration as the server spells it: milliseconds as a
// number on 3.x, "HH:MM:SS.mmm" as a string before.  It is kept verbatim
// and sent back the same way; "" is null.
type Duration string

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*d = ""
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*d = Duration(str)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		*d = Duration(n.String())
		return nil
	}
	return fmt.Errorf("duration: neither a string nor a number: %s", s)
}

func (d Duration) MarshalJSON() ([]byte, error) {
	if d == "" {
		return []byte("null"), nil
	}
	if _, err := json.Number(d).Float64(); err == nil {
		return []byte(d), nil
	}
	return json.Marshal(string(d))
}

// ServerVersion is GET /server/version.
type ServerVersion struct {
	Major int `json:"major"`
	Minor int `json:"minor"`
	Patch int `json:"patch"`
}

// User is GET /users/me.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// APIKey is GET /api-keys/me.
type APIKey struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
}

// Exif is the exifInfo of an asset.
type Exif struct {
	Make             *string  `json:"make"`
	Model            *string  `json:"model"`
	ExifImageWidth   *int     `json:"exifImageWidth"`
	ExifImageHeight  *int     `json:"exifImageHeight"`
	FileSizeInByte   *int64   `json:"fileSizeInByte"`
	Orientation      *string  `json:"orientation"`
	DateTimeOriginal *string  `json:"dateTimeOriginal"`
	ModifyDate       *string  `json:"modifyDate"`
	TimeZone         *string  `json:"timeZone"`
	LensModel        *string  `json:"lensModel"`
	FNumber          *float64 `json:"fNumber"`
	FocalLength      *float64 `json:"focalLength"`
	ISO              *int     `json:"iso"`
	ExposureTime     *string  `json:"exposureTime"`
	Latitude         *float64 `json:"latitude"`
	Longitude        *float64 `json:"longitude"`
	City             *string  `json:"city"`
	State            *string  `json:"state"`
	Country          *string  `json:"country"`
	Description      *string  `json:"description"`
	ProjectionType   *string  `json:"projectionType"`
	Rating           *int     `json:"rating"`
}

// Tag is one entry of GET /tags; Value is the full path (parent/child).
type Tag struct {
	ID       string  `json:"id"`
	ParentID *string `json:"parentId,omitempty"`
	Name     string  `json:"name"`
	Value    string  `json:"value"`
}

// Person is one entry of GET /people.
type Person struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	BirthDate  *string `json:"birthDate"`
	IsHidden   bool    `json:"isHidden"`
	IsFavorite bool    `json:"isFavorite"`
}

// Asset is the asset DTO as search, albums, stacks and GET /assets/{id}
// return it.
type Asset struct {
	ID               string   `json:"id"`
	OwnerID          string   `json:"ownerId"`
	Type             string   `json:"type"`
	OriginalFileName string   `json:"originalFileName"`
	OriginalMimeType string   `json:"originalMimeType"`
	FileCreatedAt    string   `json:"fileCreatedAt"`
	FileModifiedAt   string   `json:"fileModifiedAt"`
	LocalDateTime    string   `json:"localDateTime"`
	IsFavorite       bool     `json:"isFavorite"`
	IsTrashed        bool     `json:"isTrashed"`
	Visibility       string   `json:"visibility"`
	Duration         Duration `json:"duration"`
	LivePhotoVideoID *string  `json:"livePhotoVideoId"`
	Checksum         string   `json:"checksum"`
	Width            *int     `json:"width"`
	Height           *int     `json:"height"`
	HasMetadata      bool     `json:"hasMetadata"`
	ExifInfo         *Exif    `json:"exifInfo"`
	People           []Person `json:"people"`
	Tags             []Tag    `json:"tags"`
}

// Album is one entry of GET /albums.
type Album struct {
	ID                    string  `json:"id"`
	AlbumName             string  `json:"albumName"`
	Description           string  `json:"description"`
	AlbumThumbnailAssetID *string `json:"albumThumbnailAssetId"`
	Order                 string  `json:"order"`
	AssetCount            int     `json:"assetCount"`
}

// Stack is one entry of GET /stacks.
type Stack struct {
	ID             string  `json:"id"`
	PrimaryAssetID string  `json:"primaryAssetId"`
	Assets         []Asset `json:"assets"`
}

// Face is one entry of GET /faces?id=<asset>.
type Face struct {
	ID            string  `json:"id"`
	BoundingBoxX1 int     `json:"boundingBoxX1"`
	BoundingBoxY1 int     `json:"boundingBoxY1"`
	BoundingBoxX2 int     `json:"boundingBoxX2"`
	BoundingBoxY2 int     `json:"boundingBoxY2"`
	ImageWidth    int     `json:"imageWidth"`
	ImageHeight   int     `json:"imageHeight"`
	SourceType    string  `json:"sourceType"`
	Person        *Person `json:"person"`
}

// AssetFile is one entry of GET /asset-files.
type AssetFile struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Path string `json:"path"`
}

// SearchRequest is the body of POST /search/metadata; zero fields are left
// out so the server applies its defaults.
type SearchRequest struct {
	Page       int      `json:"page,omitempty"`
	Size       int      `json:"size,omitempty"`
	WithExif   bool     `json:"withExif,omitempty"`
	WithPeople bool     `json:"withPeople,omitempty"`
	AlbumIDs   []string `json:"albumIds,omitempty"`
	TagIDs     []string `json:"tagIds,omitempty"`
}

type searchResponse struct {
	Assets struct {
		Total    int     `json:"total"`
		Count    int     `json:"count"`
		Items    []Asset `json:"items"`
		NextPage *string `json:"nextPage"`
	} `json:"assets"`
}

type peopleResponse struct {
	People      []Person `json:"people"`
	HasNextPage bool     `json:"hasNextPage"`
	Total       int      `json:"total"`
}

// BulkCheckAsset is one entry of POST /assets/bulk-upload-check.
type BulkCheckAsset struct {
	ID       string `json:"id"`
	Checksum string `json:"checksum"`
}

// BulkCheckResult is the answer for one BulkCheckAsset.
type BulkCheckResult struct {
	ID      string  `json:"id"`
	Action  string  `json:"action"` // accept or reject
	Reason  *string `json:"reason"` // duplicate, unsupported-format
	AssetID *string `json:"assetId"`
}

// UploadResult is the answer of POST /assets.
type UploadResult struct {
	ID     string `json:"id"`
	Status string `json:"status"` // created, replaced, duplicate
}

// CreateAlbum is the body of POST /albums.
type CreateAlbum struct {
	AlbumName   string   `json:"albumName"`
	Description string   `json:"description,omitempty"`
	AssetIDs    []string `json:"assetIds,omitempty"`
}

// UpdateAlbum is the body of PATCH /albums/{id}.
type UpdateAlbum struct {
	AlbumThumbnailAssetID *string `json:"albumThumbnailAssetId,omitempty"`
	Order                 *string `json:"order,omitempty"`
}

// BulkIDResult is one entry of the answer to PUT /albums/{id}/assets.
type BulkIDResult struct {
	ID      string  `json:"id"`
	Success bool    `json:"success"`
	Error   *string `json:"error"`
}

// CreatePerson is the body of POST /people.
type CreatePerson struct {
	Name       string  `json:"name"`
	BirthDate  *string `json:"birthDate,omitempty"`
	IsFavorite bool    `json:"isFavorite"`
	IsHidden   bool    `json:"isHidden"`
}

// CreateFace is the body of POST /faces.
type CreateFace struct {
	AssetID     string `json:"assetId"`
	PersonID    string `json:"personId"`
	ImageWidth  int    `json:"imageWidth"`
	ImageHeight int    `json:"imageHeight"`
	X           int    `json:"x"`
	Y           int    `json:"y"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

// BulkUpdate is the body of PUT /assets; only the set fields are sent.
type BulkUpdate struct {
	IDs              []string `json:"ids"`
	DateTimeOriginal *string  `json:"dateTimeOriginal,omitempty"`
	TimeZone         *string  `json:"timeZone,omitempty"`
	Latitude         *float64 `json:"latitude,omitempty"`
	Longitude        *float64 `json:"longitude,omitempty"`
	IsFavorite       *bool    `json:"isFavorite,omitempty"`
	Visibility       *string  `json:"visibility,omitempty"`
	Rating           *int     `json:"rating,omitempty"`
	Description      *string  `json:"description,omitempty"`
}

// IsEmpty reports whether the update carries no field.
func (u *BulkUpdate) IsEmpty() bool {
	return u.DateTimeOriginal == nil && u.TimeZone == nil && u.Latitude == nil && u.Longitude == nil &&
		u.IsFavorite == nil && u.Visibility == nil && u.Rating == nil && u.Description == nil
}
