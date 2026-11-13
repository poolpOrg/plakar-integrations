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

// Package dump holds what the immich+dump:// importer and exporter share:
// the layout of a snapshot, the shape of a dump filename and the config
// keys of the protocol.
package dump

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// Scheme is the protocol name.
	Scheme = "immich+dump"

	// BackupsDir holds the database dump in a snapshot.
	BackupsDir = "/backups"

	// UploadDir holds the walk of UPLOAD_LOCATION in a snapshot.
	UploadDir = "/upload_location"

	// EnvMaintenanceToken carries the maintenance token of a fresh server,
	// ahead of the maintenance_token config entry.
	EnvMaintenanceToken = "PLAKAR_IMMICH_MAINTENANCE_TOKEN"

	// UploadedPrefix is what the server prepends to an uploaded dump.
	UploadedPrefix = "uploaded-"
)

// Regenerable are the directories of UPLOAD_LOCATION that are not backed
// up: the server rebuilds them from the originals.
var Regenerable = []string{"thumbs", "encoded-video"}

// Info is what a dump filename says about the dump.
type Info struct {
	// Timestamp is when the dump was taken, in the server's zone.
	Timestamp time.Time

	// Immich is the server version that made it, without the v.
	Immich string

	// Postgres is the database version that made it.
	Postgres string
}

// immich-db-backup-20261111T015106-v3.4.0-pg14.19.sql.gz, with or without
// a restore-point- or uploaded- prefix.
var dumpName = regexp.MustCompile(`immich-db-backup-(\d{8}T\d{6})-v(.+)-pg([^-]+)\.sql\.gz$`)

// IsDump reports whether a file name is a database dump at all.
func IsDump(name string) bool {
	return strings.HasSuffix(name, ".sql.gz") || strings.HasSuffix(name, ".sql")
}

// Parse reads the versions and the timestamp out of a dump filename.  tz
// is the zone the server named with the file, "" for UTC.
func Parse(name, tz string) (Info, error) {
	m := dumpName.FindStringSubmatch(name)
	if m == nil {
		return Info{}, fmt.Errorf("%q does not carry the Immich and Postgres versions", name)
	}

	loc := time.UTC
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	ts, err := time.ParseInLocation("20060102T150405", m[1], loc)
	if err != nil {
		return Info{}, fmt.Errorf("%q: %w", name, err)
	}
	return Info{Timestamp: ts, Immich: strings.TrimPrefix(m[2], "v"), Postgres: m[3]}, nil
}

// CheckVersions refuses a dump that the server would not take as its own:
// the Immich version must be the same, and the Postgres major must be the
// one of the dumps the server made itself, when it has any.
func CheckVersions(info Info, immich string, postgres []string) error {
	if got, want := strings.TrimPrefix(immich, "v"), info.Immich; got != want {
		return fmt.Errorf("the dump was made by Immich v%s, the server runs v%s", want, got)
	}
	if len(postgres) == 0 {
		return nil
	}
	want := major(info.Postgres)
	for _, p := range postgres {
		if major(p) == want {
			return nil
		}
	}
	return fmt.Errorf("the dump was made by Postgres %s, the server's own dumps were made by Postgres %s", info.Postgres, postgres[0])
}

func major(v string) string {
	v, _, _ = strings.Cut(v, ".")
	return v
}

// ParseBool reads an optional boolean config entry.
func ParseBool(config map[string]string, key string) (bool, error) {
	v, ok := config[key]
	if !ok {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid value for %s: %w", key, err)
	}
	return b, nil
}

// ParseDuration reads an optional duration config entry.
func ParseDuration(config map[string]string, key string, def time.Duration) (time.Duration, error) {
	v, ok := config[key]
	if !ok {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid value for %s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid value for %s: must be positive", key)
	}
	return d, nil
}
