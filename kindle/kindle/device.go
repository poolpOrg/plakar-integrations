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

// Package kindle locates an attached Kindle and decides what is worth
// backing up from it.
package kindle

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// documents/ is the marker: every generation has it.
const (
	DocumentsDir = "documents"
	SystemDir    = "system"
)

const ClippingsFile = "My Clippings.txt"

// Volume is a mounted Kindle.
type Volume struct {
	Root string // mount point
}

// candidateMountRoots returns the directories removable volumes are mounted
// under.  A Kindle appears as a child of one of them, never as one itself.
func candidateMountRoots() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"/Volumes"}
	case "windows":
		return nil // see detectWindows
	default:
		roots := []string{"/run/media", "/media", "/mnt"}
		if u := os.Getenv("USER"); u != "" {
			roots = append([]string{
				filepath.Join("/run/media", u),
				filepath.Join("/media", u),
			}, roots...)
		}
		return roots
	}
}

// IsKindleVolume reports whether dir is the root of a Kindle volume.
func IsKindleVolume(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, DocumentsDir))
	return err == nil && fi.IsDir()
}

// storage names seen between an MTP mount point and the Kindle layout.
var mtpStorageNames = []string{
	"Internal Storage",
	"Internal storage",
	"internal storage",
	"Kindle",
}

// resolveMTPRoot returns the directory holding documents/, given a mount
// point.  MTP mounts nest the layout under a storage directory:
//
//	/run/user/1000/gvfs/mtp:host=.../Internal Storage/documents
func resolveMTPRoot(dir string) (string, bool) {
	if IsKindleVolume(dir) {
		return dir, true
	}

	for _, name := range mtpStorageNames {
		if candidate := filepath.Join(dir, name); IsKindleVolume(candidate) {
			return candidate, true
		}
	}

	// storage names are localised, so scan rather than trust the list.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if candidate := filepath.Join(dir, entry.Name()); IsKindleVolume(candidate) {
			return candidate, true
		}
	}

	return "", false
}

// mtpMountRoots returns the directories gvfs mounts MTP devices under.  FUSE
// helpers mount wherever they were told to, so those need an explicit location.
func mtpMountRoots() []string {
	var roots []string

	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		roots = append(roots, filepath.Join(dir, "gvfs"))
	}
	if uid := os.Getuid(); uid >= 0 {
		roots = append(roots, filepath.Join("/run/user", strconv.Itoa(uid), "gvfs"))
	}

	return dedup(roots)
}

// Detect returns the mounted Kindle, and fails if there is not exactly one:
// picking one of two at random would back up or restore the wrong device.
func Detect() (*Volume, error) {
	found, err := detectAll()
	if err != nil {
		return nil, err
	}

	switch len(found) {
	case 0:
		return nil, fmt.Errorf("no Kindle found: plug the device in over USB, " +
			"make sure it is in USB drive mode, or set location to its mount point")
	case 1:
		return &Volume{Root: found[0]}, nil
	default:
		return nil, fmt.Errorf("several Kindles found (%s): "+
			"set location to the mount point of the one you want",
			strings.Join(found, ", "))
	}
}

func detectAll() ([]string, error) {
	if runtime.GOOS == "windows" {
		return detectWindows(), nil
	}

	// a device shows up in one or the other, depending on its generation.
	roots := append(candidateMountRoots(), mtpMountRoots()...)

	var found []string
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue // root does not exist on this system, that is fine
		}
		for _, entry := range entries {
			dir := filepath.Join(root, entry.Name())
			if resolved, ok := resolveMTPRoot(dir); ok {
				found = append(found, resolved)
			}
		}
	}

	sort.Strings(found)
	return dedup(found), nil
}

// detectWindows probes drive letters: Windows has no mount point directory.
func detectWindows() []string {
	var found []string
	for c := 'D'; c <= 'Z'; c++ { // A-C are floppies and the system disk
		dir := string(c) + `:\`
		if IsKindleVolume(dir) {
			found = append(found, dir)
		}
	}
	return found
}

func dedup(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// Open resolves a location into a Volume.  A bare "kindle://" means detect the
// device; anything else is a mount point, which is what you want for a second
// Kindle or for a copy of a Kindle tree sitting on disk.
func Open(location string) (*Volume, error) {
	path := strings.TrimPrefix(location, "kindle://")
	path = strings.TrimSuffix(path, "/")

	if path == "" {
		return Detect()
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("invalid location %q: %w", location, err)
	}

	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("cannot access %s: %w", abs, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", abs)
	}

	// accept an MTP mount point, not just the Kindle root.
	if resolved, ok := resolveMTPRoot(abs); ok {
		return &Volume{Root: resolved}, nil
	}

	return &Volume{Root: abs}, nil
}

// Validate checks the volume is a Kindle.  Separate from Open so a configured
// path can be opened before the layout exists.
func (v *Volume) Validate() error {
	if !IsKindleVolume(v.Root) {
		return fmt.Errorf("%s does not look like a Kindle: no %s/ directory",
			v.Root, DocumentsDir)
	}
	return nil
}

// Documents is the path of the books directory.
func (v *Volume) Documents() string {
	return filepath.Join(v.Root, DocumentsDir)
}

// serialFromPath recovers the serial from an MTP mount point.  gvfs names its
// mounts after the udev device, so the serial is the last underscore-separated
// field:
//
//	/run/user/1000/gvfs/mtp:host=Amazon_Kindle_Scribe_Colorsoft_G094PH08620701GQ
//
// MTP does not expose system/ at all, so this is the only place it appears.
func serialFromPath(root string) string {
	for dir := filepath.Clean(root); ; dir = filepath.Dir(dir) {
		base := filepath.Base(dir)

		if host, ok := strings.CutPrefix(base, "mtp:host="); ok {
			if i := strings.LastIndexByte(host, '_'); i >= 0 {
				if serial := host[i+1:]; looksLikeSerial(serial) {
					return serial
				}
			}
			return ""
		}

		if parent := filepath.Dir(dir); parent == dir {
			return "" // reached the root without finding a mount
		}
	}
}

// looksLikeSerial rejects model names from devices that report no serial.
func looksLikeSerial(s string) bool {
	if len(s) < 12 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

// Serial returns the device serial, if it exposes one.  Amazon puts it in no
// documented place, so best-effort: the empty string is a normal answer.
func (v *Volume) Serial() string {
	if serial := serialFromPath(v.Root); serial != "" {
		return serial
	}

	// newer devices leave a JSON blob here, older ones nothing.  the schema
	// is undocumented and varies, so look for the key rather than parse it.
	for _, rel := range []string{
		filepath.Join(SystemDir, "device.secure"),
		filepath.Join(SystemDir, ".device_info"),
	} {
		data, err := os.ReadFile(filepath.Join(v.Root, rel))
		if err != nil || len(data) > 64*1024 {
			continue
		}
		if s := extractSerial(string(data)); s != "" {
			return s
		}
	}
	return ""
}

func extractSerial(s string) string {
	for _, key := range []string{`"serial":"`, `"SerialNumber":"`} {
		if i := strings.Index(s, key); i >= 0 {
			rest := s[i+len(key):]
			if j := strings.IndexByte(rest, '"'); j > 0 {
				return rest[:j]
			}
		}
	}
	return ""
}
