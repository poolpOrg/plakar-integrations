/*
 * Copyright (c) 2026 Gilles Chehade <gilles@plakar.io>
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
	"os"
	"path/filepath"
)

// ErrLocked is returned when another process holds the working directory
// lock, that is another backup or restore of the same device is running.
var ErrLocked = errors.New("another backup or restore of this device is in progress")

// Lock is an exclusive lock on a device's working directory. idevicebackup2
// mutates that directory in place, so two runs against it at once would
// corrupt the staging state; the lock serializes them.
type Lock struct {
	f    *os.File
	path string
}

// LockPath is the lock file guarding root/<udid>.
func LockPath(root, udid string) string {
	return filepath.Join(root, "."+udid+".lock")
}

// Path returns the lock file.
func (l *Lock) Path() string {
	return l.path
}
