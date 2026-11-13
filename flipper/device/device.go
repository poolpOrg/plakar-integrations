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

package device

import (
	"errors"
)

// File is a directory entry as reported by the Flipper storage RPC.
// The device does not report modification times or permissions.  Md5
// is filled by List on firmwares that support it, and empty otherwise.
type File struct {
	Name  string
	Size  int64
	IsDir bool
	Md5   string
}

// Conn is the subset of the Flipper RPC the connectors rely on.  The
// serial implementation talks to a real device, the fake one backs the
// tests.
type Conn interface {
	Name() string
	Stat(path string) (*File, error)
	List(path string) ([]*File, error)
	Read(path string) ([]byte, error)
	Write(path string, data []byte) error
	Mkdir(path string) error
	Delete(path string, recursive bool) error
	Rename(oldpath, newpath string) error
	Md5(path string) (string, error)
	Close() error
}

var (
	ErrNotExist = errors.New("no such file or directory")
	ErrExist    = errors.New("file exists")
)
