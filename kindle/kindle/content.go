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

package kindle

import (
	"path/filepath"
	"strings"
)

// Selection decides what gets backed up.  The default is what a user would be
// sad to lose; the device's caches are large, regenerable, and meaningless
// elsewhere, so they stay out unless asked for.
type Selection struct {
	Documents   bool // the books
	Annotations bool // .sdr sidecars, My Clippings.txt, handwritten notebooks
	System      bool // collections, settings and other device bookkeeping
	Everything  bool // the whole volume, caches included
}

// DefaultSelection is what you get when nothing is configured.
func DefaultSelection() Selection {
	return Selection{
		Documents:   true,
		Annotations: true,
		System:      false,
	}
}

// extensions a Kindle can open, used to tell books from the debris that
// accumulates in documents/.
var bookExtensions = map[string]bool{
	".azw":     true,
	".azw1":    true,
	".azw2":    true,
	".azw3":    true,
	".azw4":    true,
	".azw6":    true,
	".azw8":    true,
	".kfx":     true,
	".mobi":    true,
	".prc":     true,
	".pobi":    true,
	".epub":    true,
	".pdf":     true,
	".txt":     true,
	".doc":     true,
	".docx":    true,
	".rtf":     true,
	".htm":     true,
	".html":    true,
	".cbz":     true,
	".cbr":     true,
	".kfx-zip": true,
}

// IsBook reports whether name looks like readable content.
func IsBook(name string) bool {
	return bookExtensions[strings.ToLower(filepath.Ext(name))]
}

// per-book state lives in "<book title>.sdr" next to the book.
func isSidecarDir(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".sdr")
}

// NotebooksDir holds the Scribe's handwritten notebooks.  They exist nowhere
// else, so they count as annotations despite the leading dot.
const NotebooksDir = ".notebooks"

// content the user put there: side-loaded, not reconstructible from the
// account, so it follows the documents flag.
var userContentDirs = map[string]bool{
	"screenshots": true, // screenshots taken on the device
	"fonts":       true, // side-loaded fonts
	"audible":     true, // audiobooks
	"voice":       true, // voice recordings
}

// device-generated and worthless off-device.  matched lowercased: MTP and
// mass storage disagree about capitalisation.
var excludedPrefixes = []string{
	"system/thumbnails",      // cover thumbnails, rebuilt on demand
	"system/grok_thumbnails", // ditto, newer devices
	"system/dictionaries",    // shipped with the firmware
	"documents/dictionaries", // ditto, as exposed over MTP
	"system/glossary",
	"system/search indexes", // full-text index, rebuilt on demand
	"system/cloudindices",   // cloud content index, refetched
	"system/fmcache",
	"system/readingstreams",   // telemetry
	"system/btlogs",           // bluetooth logs
	"system/userannotlogsdir", // annotation write-ahead logs
	"system/nfa",
	"system/preloader",
	"system/.temp",
	"documents/.active_content_sandbox",
}

// OS droppings and device scratch state.
var excludedNames = map[string]bool{
	".ds_store":             true,
	"thumbs.db":             true,
	".fseventsd":            true,
	".spotlight-v100":       true,
	".trashes":              true,
	".metadata_never_index": true,
	"desktop.ini":           true,
}

// Skip reports whether rel, relative to the volume root, stays out of the
// backup.  Directories are reported too so the walker can prune subtrees.
func (s Selection) Skip(rel string, isDir bool) bool {
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == "" {
		return false
	}

	base := strings.ToLower(filepath.Base(rel))
	if excludedNames[base] {
		return true
	}

	if s.Everything {
		return false
	}

	lower := strings.ToLower(rel)
	for _, p := range excludedPrefixes {
		if lower == p || strings.HasPrefix(lower, p+"/") {
			return true
		}
	}

	top, rest, _ := strings.Cut(rel, "/")
	switch {
	case strings.EqualFold(top, DocumentsDir):
		return s.skipDocuments(rel, rest, isDir)

	case strings.EqualFold(top, NotebooksDir):
		return !s.Annotations

	case strings.EqualFold(top, SystemDir):
		return !s.System

	case userContentDirs[strings.ToLower(top)]:
		return !s.Documents

	default:
		// not something the device put there, so treat it as user content.
		return !s.Documents
	}
}

func (s Selection) skipDocuments(rel, rest string, isDir bool) bool {
	if rest == "" {
		// documents/ itself: keep it if we want anything inside.
		return !s.Documents && !s.Annotations
	}

	// an annotation despite sitting among the books with a book-ish extension.
	if !isDir && strings.EqualFold(filepath.Base(rel), ClippingsFile) {
		return !s.Annotations
	}

	for _, seg := range strings.Split(rest, "/") {
		if isSidecarDir(seg) {
			return !s.Annotations
		}
	}

	if isDir {
		// sidecars nest in here too, so keep it for annotations as well.
		return !s.Documents && !s.Annotations
	}

	if !s.Documents {
		return true
	}

	// drops .mbp cursors from old firmware and half-finished downloads.
	return !IsBook(filepath.Base(rel))
}
