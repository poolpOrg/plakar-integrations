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

package adbtest

import (
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/poolpOrg/plakar-integrations/android/adb"
)

// Node is a file, directory or symlink of a fake device filesystem.
type Node struct {
	Mode    uint32 // Linux st_mode
	Data    []byte
	ModTime time.Time
	UID     uint32
	GID     uint32
	Ino     uint64
	Target  string // symlink target
	// Unreadable makes LIST behave like opendir failed: no entries at all.
	Unreadable bool
	// ReadError makes RECV fail with this message instead of sending data.
	ReadError string
	// StatErrno is reported in the error field of v2 directory entries,
	// like adbd does when lstat fails on an entry.
	StatErrno adb.Errno

	children map[string]*Node
}

// FS is an in-memory Linux-like filesystem served by the fake adbd.
type FS struct {
	mu      sync.Mutex
	root    *Node
	nextIno uint64
	// NoSymlinks mimics Android's emulated storage, where symlink(2)
	// fails with EPERM.
	NoSymlinks bool
}

// NewFS returns an empty filesystem with a root directory.
func NewFS() *FS {
	fs := &FS{nextIno: 1}
	fs.root = fs.newNode(adb.S_IFDIR|0o755, time.Unix(1700000000, 0))
	return fs
}

func (fs *FS) newNode(mode uint32, mtime time.Time) *Node {
	n := &Node{Mode: mode, ModTime: mtime, Ino: fs.nextIno}
	fs.nextIno++
	if mode&adb.S_IFMT == adb.S_IFDIR {
		n.children = make(map[string]*Node)
	}
	return n
}

func split(p string) []string {
	var parts []string
	for _, c := range strings.Split(p, "/") {
		if c != "" && c != "." {
			parts = append(parts, c)
		}
	}
	return parts
}

// lookup resolves p.  Intermediate symlinks are always followed; the last
// component is followed only when follow is set.  It returns the node, its
// parent directory and the final name, or an errno.
func (fs *FS) lookup(p string, follow bool) (node, parent *Node, name string, err adb.Errno) {
	if !strings.HasPrefix(p, "/") {
		return nil, nil, "", 2
	}
	return fs.walk(fs.root, split(p), follow, 0)
}

func (fs *FS) walk(dir *Node, parts []string, follow bool, depth int) (*Node, *Node, string, adb.Errno) {
	if depth > 40 {
		return nil, nil, "", 40
	}
	cur := dir
	var parent *Node
	name := ""
	for i, part := range parts {
		if cur.Mode&adb.S_IFMT != adb.S_IFDIR {
			return nil, nil, "", 20
		}
		if part == ".." {
			// Keep it simple: the fake never serves "..".
			return nil, nil, "", 2
		}
		next, ok := cur.children[part]
		parent, name = cur, part
		if !ok {
			if i == len(parts)-1 {
				return nil, parent, name, 2
			}
			return nil, nil, "", 2
		}
		last := i == len(parts)-1
		if next.Mode&adb.S_IFMT == adb.S_IFLNK && (!last || follow) {
			target := next.Target
			base := fs.root
			if !strings.HasPrefix(target, "/") {
				base = cur
			}
			resolved, _, _, e := fs.walk(base, split(target), true, depth+1)
			if e != 0 {
				return nil, nil, "", e
			}
			next = resolved
		}
		cur = next
	}
	return cur, parent, name, 0
}

// Lstat returns the node at p without following a final symlink.
func (fs *FS) Lstat(p string) (*Node, adb.Errno) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n, _, _, e := fs.lookup(p, false)
	return n, e
}

// Stat returns the node at p, following symlinks.
func (fs *FS) Stat(p string) (*Node, adb.Errno) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n, _, _, e := fs.lookup(p, true)
	return n, e
}

// MkdirAll creates the directory p and its parents.
func (fs *FS) MkdirAll(p string, mtime time.Time) *Node {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.mkdirAll(p, mtime)
}

func (fs *FS) mkdirAll(p string, mtime time.Time) *Node {
	cur := fs.root
	for _, part := range split(p) {
		next, ok := cur.children[part]
		if !ok {
			next = fs.newNode(adb.S_IFDIR|0o771, mtime)
			cur.children[part] = next
		}
		if next.Mode&adb.S_IFMT == adb.S_IFLNK {
			resolved, _, _, e := fs.walk(fs.root, split(next.Target), true, 0)
			if e != 0 {
				return nil
			}
			next = resolved
		}
		if next.Mode&adb.S_IFMT != adb.S_IFDIR {
			return nil
		}
		cur = next
	}
	return cur
}

// WriteFile creates or replaces the regular file p.
func (fs *FS) WriteFile(p string, data []byte, mode uint32, mtime time.Time) *Node {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	dir := fs.mkdirAll(path.Dir(p), mtime)
	if dir == nil {
		return nil
	}
	n := fs.newNode(adb.S_IFREG|(mode&0o7777), mtime)
	n.Data = append([]byte(nil), data...)
	dir.children[path.Base(p)] = n
	return n
}

// Symlink creates the symlink p pointing at target.
func (fs *FS) Symlink(p, target string, mtime time.Time) *Node {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	dir := fs.mkdirAll(path.Dir(p), mtime)
	n := fs.newNode(adb.S_IFLNK|0o777, mtime)
	n.Target = target
	dir.children[path.Base(p)] = n
	return n
}

// Remove deletes p (recursively for a directory).
func (fs *FS) Remove(p string) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	_, parent, name, e := fs.lookup(p, false)
	if e != 0 {
		return false
	}
	delete(parent.children, name)
	return true
}

// ReadFile returns the content of the regular file at p.
func (fs *FS) ReadFile(p string) ([]byte, bool) {
	n, e := fs.Stat(p)
	if e != 0 || n.Mode&adb.S_IFMT != adb.S_IFREG {
		return nil, false
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]byte(nil), n.Data...), true
}

// Entries returns the sorted entry names of the directory p.
func (fs *FS) Entries(p string) []string {
	n, e := fs.Stat(p)
	if e != 0 || n.children == nil {
		return nil
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Paths returns every path of the filesystem, sorted, for comparisons.
func (fs *FS) Paths() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []string
	var rec func(dir *Node, prefix string)
	rec = func(dir *Node, prefix string) {
		for name, child := range dir.children {
			p := prefix + "/" + name
			out = append(out, p)
			if child.children != nil {
				rec(child, p)
			}
		}
	}
	rec(fs.root, "")
	sort.Strings(out)
	return out
}

// list returns the sorted children of a directory for the LIST service, or
// ok=false when opendir would have failed.
func (fs *FS) list(p string) (entries []listEntry, ok bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n, _, _, e := fs.lookup(p, true)
	if e != 0 || n.Mode&adb.S_IFMT != adb.S_IFDIR || n.Unreadable {
		return nil, false
	}
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)
	entries = append(entries, listEntry{name: ".", node: n}, listEntry{name: "..", node: n})
	for _, name := range names {
		entries = append(entries, listEntry{name: name, node: n.children[name]})
	}
	return entries, true
}

type listEntry struct {
	name string
	node *Node
}

// send stores a file pushed by the SEND service, like adbd does: parents
// are created, an existing regular file is replaced.
func (fs *FS) send(p string, mode uint32, data []byte, mtime time.Time) adb.Errno {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !strings.HasPrefix(p, "/") {
		return 2
	}
	dir := fs.mkdirAll(path.Dir(p), time.Now())
	if dir == nil {
		return 20
	}
	name := path.Base(p)
	if existing, ok := dir.children[name]; ok && existing.Mode&adb.S_IFMT == adb.S_IFDIR {
		return 21
	}
	n := fs.newNode(adb.S_IFREG|(mode&0o777), mtime)
	n.Data = data
	dir.children[name] = n
	return 0
}

func (fs *FS) sendLink(p, target string, mtime time.Time) adb.Errno {
	if fs.NoSymlinks {
		return 1
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	dir := fs.mkdirAll(path.Dir(p), time.Now())
	if dir == nil {
		return 20
	}
	n := fs.newNode(adb.S_IFLNK|0o777, mtime)
	n.Target = target
	dir.children[path.Base(p)] = n
	return 0
}
