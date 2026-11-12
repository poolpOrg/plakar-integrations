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

package common

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/poolpOrg/plakar-integrations/android/adb"
)

// SharedStorageCandidates are the paths where the primary shared storage
// (photos, downloads, documents) is found, most common first.
var SharedStorageCandidates = []string{
	"/storage/emulated/0",
	"/sdcard",
	"/storage/self/primary",
	"/mnt/sdcard",
}

// sharedStoragePrefixes are the places the exporter accepts to write to
// without being told otherwise: shared storage and the shell's scratch
// directory, never system or private application data.
var sharedStoragePrefixes = []string{
	"/storage/",
	"/sdcard",
	"/mnt/sdcard",
	"/mnt/user/",
	"/mnt/media_rw/",
	"/data/local/tmp",
}

// WithinSharedStorage reports whether the (clean, absolute) path lies in
// user-accessible storage.
func WithinSharedStorage(p string) bool {
	for _, prefix := range sharedStoragePrefixes {
		if strings.HasSuffix(prefix, "/") {
			if strings.HasPrefix(p, prefix) {
				return true
			}
			continue
		}
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// StatFollow stats p, following a final symlink when the device can
// (stat_v2); otherwise the lstat result is returned as is.
func StatFollow(s *adb.Sync, p string) (adb.Stat, error) {
	st, err := s.Lstat(p)
	if err != nil {
		return st, err
	}
	if !st.IsSymlink() {
		return st, nil
	}
	if followed, err := s.Stat(p); err == nil {
		return followed, nil
	} else if !errors.Is(err, errors.ErrUnsupported) {
		return st, err
	}
	// v1 device: no way to follow.  A symlinked root is still usable
	// when its listing succeeds; report it as a directory then.
	if readable, err := s.List(p, func(adb.Dirent) error { return nil }); err == nil && readable {
		st.Mode = adb.S_IFDIR | st.Mode&0o777
	}
	return st, nil
}

// ResolveRoot validates the requested device path, or finds the shared
// storage when it is empty.  The returned path is the one given (or the
// candidate), not a resolved real path, so snapshots keep a stable layout.
func ResolveRoot(s *adb.Sync, requested string) (string, adb.Stat, error) {
	if requested != "" {
		st, err := StatFollow(s, requested)
		if err != nil {
			return "", st, fmt.Errorf("%s: %w", requested, err)
		}
		if !st.IsDir() && !st.IsRegular() {
			return "", st, fmt.Errorf("%s: not a directory or a regular file", requested)
		}
		if st.IsDir() {
			readable, err := s.List(requested, func(adb.Dirent) error { return nil })
			if err != nil {
				return "", st, fmt.Errorf("%s: %w", requested, err)
			}
			if !readable {
				return "", st, fmt.Errorf("%s: cannot list directory: %w", requested, fs.ErrPermission)
			}
		}
		return requested, st, nil
	}

	var tried []string
	for _, candidate := range SharedStorageCandidates {
		st, err := StatFollow(s, candidate)
		if err != nil || !st.IsDir() {
			tried = append(tried, candidate)
			continue
		}
		readable, err := s.List(candidate, func(adb.Dirent) error { return nil })
		if err != nil {
			return "", st, err
		}
		if !readable {
			tried = append(tried, candidate+" (not readable)")
			continue
		}
		return candidate, st, nil
	}
	return "", adb.Stat{}, fmt.Errorf("no accessible shared storage found (tried %s): unlock the device, make sure USB debugging is allowed, or pass the path explicitly", strings.Join(tried, ", "))
}
