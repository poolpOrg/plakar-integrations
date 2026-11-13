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

// Package layout is the shape of an immich:// snapshot: where each file
// goes and what the JSON side files carry.  The importer writes it, the
// exporter reads it back, and nothing else depends on the paths: the
// exporter joins on the checksums inside the JSON, never on a path.
//
//	/manifest.json
//	/assets/2024/07/IMG_0123.HEIC          original, bytes as-is
//	/assets/2024/07/IMG_0123.HEIC.xmp      sidecar when present
//	/assets/2024/07/IMG_0123.HEIC.json     Asset
//	/albums/<name>.json                    Album
//	/stacks/<primary sha1 hex>.json        Stack
//	/people/<name>.json                    Person
package layout

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/poolpOrg/plakar-integrations/immich/api"
)

const (
	ManifestPath = "/manifest.json"
	AssetsDir    = "/assets"
	AlbumsDir    = "/albums"
	StacksDir    = "/stacks"
	PeopleDir    = "/people"

	// Format is bumped when the side files change shape.
	Format = 1
)

// Manifest is /manifest.json, written first.
type Manifest struct {
	Format     int       `json:"format"`
	ExportedAt time.Time `json:"exportedAt"`
	Server     Server    `json:"server"`
	User       api.User  `json:"user"`
	APIKey     APIKey    `json:"apiKey"`
	Counts     Counts    `json:"counts"`
}

// Server identifies the Immich the snapshot came from.
type Server struct {
	URL     string `json:"url"`
	Version string `json:"version"`
}

// APIKey records which key made the backup; the secret is not in it.
type APIKey struct {
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
}

// Counts is what the backup found, for a quick sanity check.
type Counts struct {
	Assets   int `json:"assets"`
	Sidecars int `json:"sidecars"`
	Albums   int `json:"albums"`
	Stacks   int `json:"stacks"`
	Tags     int `json:"tags"`
	People   int `json:"people"`
}

// Asset is the .json beside an original.  Checksum is the base64 SHA1 as
// Immich carries it, and the only identity the exporter relies on.
type Asset struct {
	Checksum         string       `json:"checksum"`
	ImmichID         string       `json:"immichId"`
	Type             string       `json:"type"`
	OriginalFileName string       `json:"originalFileName"`
	OriginalMimeType string       `json:"originalMimeType,omitempty"`
	FileCreatedAt    string       `json:"fileCreatedAt"`
	FileModifiedAt   string       `json:"fileModifiedAt"`
	LocalDateTime    string       `json:"localDateTime,omitempty"`
	IsFavorite       bool         `json:"isFavorite"`
	Visibility       string       `json:"visibility"`
	Duration         api.Duration `json:"duration,omitempty"`
	Width            *int         `json:"width,omitempty"`
	Height           *int         `json:"height,omitempty"`
	LivePhotoVideo   string       `json:"livePhotoVideo,omitempty"` // checksum of the video
	Exif             *api.Exif    `json:"exif,omitempty"`
	Tags             []string     `json:"tags,omitempty"`   // full tag paths
	People           []string     `json:"people,omitempty"` // names
	Sidecar          bool         `json:"sidecar"`
}

// Album is /albums/<name>.json.
type Album struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Order       string   `json:"order,omitempty"`
	Thumbnail   string   `json:"thumbnail,omitempty"` // checksum
	Assets      []string `json:"assets"`              // checksums
}

// Stack is /stacks/<primary>.json; Assets starts with the primary.
type Stack struct {
	Primary string   `json:"primary"`
	Assets  []string `json:"assets"`
}

// Face is one box on one asset, as Immich reports it.
type Face struct {
	Asset       string `json:"asset"` // checksum
	ImageWidth  int    `json:"imageWidth"`
	ImageHeight int    `json:"imageHeight"`
	X1          int    `json:"x1"`
	Y1          int    `json:"y1"`
	X2          int    `json:"x2"`
	Y2          int    `json:"y2"`
}

// Person is /people/<name>.json.
type Person struct {
	Name       string  `json:"name"`
	BirthDate  *string `json:"birthDate,omitempty"`
	IsFavorite bool    `json:"isFavorite"`
	IsHidden   bool    `json:"isHidden"`
	Faces      []Face  `json:"faces"`
}

// HexChecksum turns Immich's base64 SHA1 into hex, for file names and for
// the x-immich-checksum header.
func HexChecksum(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("invalid checksum %q: %w", b64, err)
	}
	if len(raw) != 20 {
		return "", fmt.Errorf("invalid checksum %q: %d bytes, want a SHA1", b64, len(raw))
	}
	return hex.EncodeToString(raw), nil
}

// AssetDir is the /assets/YYYY/MM directory of an asset, from the local
// date Immich shows it under.
func AssetDir(a *api.Asset) string {
	for _, s := range []string{a.LocalDateTime, a.FileCreatedAt} {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return fmt.Sprintf("%s/%04d/%02d", AssetsDir, t.Year(), int(t.Month()))
		}
	}
	return AssetsDir + "/undated"
}

// SafeName makes a name usable as one path element: no separators, no
// control characters, no dot-only names.
func SafeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\':
			b.WriteRune('_')
		case r < 0x20 || r == 0x7f || !unicode.IsPrint(r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	s := strings.TrimSpace(b.String())
	if s == "" || strings.Trim(s, ".") == "" {
		return "untitled"
	}
	return s
}

// Namer hands out unique paths.  Side files live beside the asset, so a
// name is taken when it, its .json or its .xmp is.
type Namer struct {
	used map[string]struct{}
}

func NewNamer() *Namer {
	return &Namer{used: make(map[string]struct{})}
}

// Reserve marks a path as taken.
func (n *Namer) Reserve(p string) {
	n.used[p] = struct{}{}
}

func (n *Namer) free(p string) bool {
	for _, s := range []string{p, p + ".json", p + ".xmp"} {
		if _, ok := n.used[s]; ok {
			return false
		}
	}
	return true
}

// Unique returns dir/name, or dir/name-<8 hex of the checksum><ext> on a
// collision, or the full hex when even that is taken, and reserves it.
func (n *Namer) Unique(dir, name, hexsum string) string {
	name = SafeName(name)
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)

	candidates := []string{path.Join(dir, name)}
	if len(hexsum) >= 8 {
		candidates = append(candidates, path.Join(dir, stem+"-"+hexsum[:8]+ext))
	}
	if hexsum != "" {
		candidates = append(candidates, path.Join(dir, stem+"-"+hexsum+ext))
	}
	for _, c := range candidates {
		if n.free(c) {
			n.Reserve(c)
			return c
		}
	}
	// Two assets with the same name and the same checksum are the same
	// bytes; number the rest so the walk never fails.
	for i := 2; ; i++ {
		c := path.Join(dir, fmt.Sprintf("%s-%s-%d%s", stem, hexsum, i, ext))
		if n.free(c) {
			n.Reserve(c)
			return c
		}
	}
}
