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

package adb

import (
	"fmt"
	"io/fs"
)

// Linux st_mode bits.  The device always runs Linux, whatever the host.
const (
	S_IFMT   = 0o170000
	S_IFSOCK = 0o140000
	S_IFLNK  = 0o120000
	S_IFREG  = 0o100000
	S_IFBLK  = 0o060000
	S_IFDIR  = 0o040000
	S_IFCHR  = 0o020000
	S_IFIFO  = 0o010000
	S_ISUID  = 0o4000
	S_ISGID  = 0o2000
	S_ISVTX  = 0o1000
)

// FileModeFromUnix converts a Linux st_mode into a Go fs.FileMode.
func FileModeFromUnix(mode uint32) fs.FileMode {
	m := fs.FileMode(mode & 0o777)
	switch mode & S_IFMT {
	case S_IFREG:
	case S_IFDIR:
		m |= fs.ModeDir
	case S_IFLNK:
		m |= fs.ModeSymlink
	case S_IFBLK:
		m |= fs.ModeDevice
	case S_IFCHR:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case S_IFIFO:
		m |= fs.ModeNamedPipe
	case S_IFSOCK:
		m |= fs.ModeSocket
	default:
		m |= fs.ModeIrregular
	}
	if mode&S_ISUID != 0 {
		m |= fs.ModeSetuid
	}
	if mode&S_ISGID != 0 {
		m |= fs.ModeSetgid
	}
	if mode&S_ISVTX != 0 {
		m |= fs.ModeSticky
	}
	return m
}

// UnixFromFileMode converts a Go fs.FileMode into a Linux st_mode.
func UnixFromFileMode(m fs.FileMode) uint32 {
	mode := uint32(m.Perm())
	switch {
	case m&fs.ModeDir != 0:
		mode |= S_IFDIR
	case m&fs.ModeSymlink != 0:
		mode |= S_IFLNK
	case m&fs.ModeCharDevice != 0:
		mode |= S_IFCHR
	case m&fs.ModeDevice != 0:
		mode |= S_IFBLK
	case m&fs.ModeNamedPipe != 0:
		mode |= S_IFIFO
	case m&fs.ModeSocket != 0:
		mode |= S_IFSOCK
	default:
		mode |= S_IFREG
	}
	if m&fs.ModeSetuid != 0 {
		mode |= S_ISUID
	}
	if m&fs.ModeSetgid != 0 {
		mode |= S_ISGID
	}
	if m&fs.ModeSticky != 0 {
		mode |= S_ISVTX
	}
	return mode
}

// Errno is an error number sent by the device in a v2 stat or list reply.
// The wire values are the Linux ones.
type Errno uint32

var errnoText = map[Errno]string{
	1:  "operation not permitted",
	2:  "no such file or directory",
	5:  "input/output error",
	12: "cannot allocate memory",
	13: "permission denied",
	17: "file exists",
	20: "not a directory",
	21: "is a directory",
	22: "invalid argument",
	24: "too many open files",
	28: "no space left on device",
	30: "read-only file system",
	36: "file name too long",
	40: "too many levels of symbolic links",
	75: "value too large",
}

func (e Errno) Error() string {
	if s, ok := errnoText[e]; ok {
		return s
	}
	return fmt.Sprintf("errno %d", uint32(e))
}

// Is lets errors.Is match the portable fs sentinel errors.
func (e Errno) Is(target error) bool {
	switch target {
	case fs.ErrNotExist:
		return e == 2
	case fs.ErrPermission:
		return e == 1 || e == 13
	case fs.ErrExist:
		return e == 17
	}
	return false
}
