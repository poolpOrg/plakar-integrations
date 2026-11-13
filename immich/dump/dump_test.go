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

package dump

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	require.NoError(t, err)

	tests := []struct {
		name string
		file string
		tz   string
		want Info
		err  string
	}{
		{
			name: "routine",
			file: "immich-db-backup-20261111T015106-v3.4.0-pg14.19.sql.gz",
			tz:   "Europe/Paris",
			want: Info{Timestamp: time.Date(2026, 11, 11, 1, 51, 6, 0, paris), Immich: "3.4.0", Postgres: "14.19"},
		},
		{
			name: "restore point",
			file: "restore-point-immich-db-backup-20261111T020000-v3.4.0-pg14.19.sql.gz",
			want: Info{Timestamp: time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC), Immich: "3.4.0", Postgres: "14.19"},
		},
		{
			name: "uploaded",
			file: "uploaded-immich-db-backup-20261111T020000-v3.4.0-pg14.19.sql.gz",
			want: Info{Timestamp: time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC), Immich: "3.4.0", Postgres: "14.19"},
		},
		{
			name: "prerelease version",
			file: "immich-db-backup-20261111T020000-v3.5.0-rc.1-pg16.4.sql.gz",
			want: Info{Timestamp: time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC), Immich: "3.5.0-rc.1", Postgres: "16.4"},
		},
		{
			name: "unknown zone falls back to UTC",
			file: "immich-db-backup-20261111T020000-v3.4.0-pg14.19.sql.gz",
			tz:   "Mars/Olympus",
			want: Info{Timestamp: time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC), Immich: "3.4.0", Postgres: "14.19"},
		},
		{
			name: "old style",
			file: "immich-db-backup-1700000000000.sql.gz",
			err:  "does not carry the Immich and Postgres versions",
		},
		{
			name: "not a dump",
			file: "notes.txt",
			err:  "does not carry",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.file, tt.tz)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want.Immich, got.Immich)
			require.Equal(t, tt.want.Postgres, got.Postgres)
			require.True(t, tt.want.Timestamp.Equal(got.Timestamp), "want %s got %s", tt.want.Timestamp, got.Timestamp)
		})
	}
}

func TestIsDump(t *testing.T) {
	require.True(t, IsDump("immich-db-backup-20261111T015106-v3.4.0-pg14.19.sql.gz"))
	require.True(t, IsDump("custom.sql"))
	require.False(t, IsDump("photo.jpg"))
	require.False(t, IsDump("dump.sql.gz.tmp"))
}

func TestCheckVersions(t *testing.T) {
	info := Info{Immich: "3.4.0", Postgres: "14.19"}

	tests := []struct {
		name     string
		immich   string
		postgres []string
		err      string
	}{
		{name: "same", immich: "v3.4.0", postgres: []string{"14.19"}},
		{name: "same without v", immich: "3.4.0", postgres: []string{"14.20"}},
		{name: "no dumps on the target", immich: "v3.4.0"},
		{name: "mixed majors after an upgrade", immich: "v3.4.0", postgres: []string{"16.4", "14.19"}},
		{name: "immich differs", immich: "v3.5.0", postgres: []string{"14.19"}, err: "made by Immich v3.4.0, the server runs v3.5.0"},
		{name: "postgres major differs", immich: "v3.4.0", postgres: []string{"16.4"}, err: "made by Postgres 14.19, the server's own dumps were made by Postgres 16.4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckVersions(info, tt.immich, tt.postgres)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestParseConfig(t *testing.T) {
	b, err := ParseBool(map[string]string{"force": "true"}, "force")
	require.NoError(t, err)
	require.True(t, b)
	b, err = ParseBool(map[string]string{}, "force")
	require.NoError(t, err)
	require.False(t, b)
	_, err = ParseBool(map[string]string{"force": "maybe"}, "force")
	require.ErrorContains(t, err, "invalid value for force")

	d, err := ParseDuration(map[string]string{}, "dump_timeout", time.Minute)
	require.NoError(t, err)
	require.Equal(t, time.Minute, d)
	d, err = ParseDuration(map[string]string{"dump_timeout": "90s"}, "dump_timeout", time.Minute)
	require.NoError(t, err)
	require.Equal(t, 90*time.Second, d)
	_, err = ParseDuration(map[string]string{"dump_timeout": "0s"}, "dump_timeout", time.Minute)
	require.ErrorContains(t, err, "must be positive")
	_, err = ParseDuration(map[string]string{"dump_timeout": "soon"}, "dump_timeout", time.Minute)
	require.ErrorContains(t, err, "invalid value for dump_timeout")
}
