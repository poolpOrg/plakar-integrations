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
	"crypto/md5"
	"encoding/hex"
	"path"
	"sort"
	"strings"
	"sync"
)

// Fake is an in-memory Conn with the same semantics as the device:
// directories must exist before files are written into them, mkdir on
// an existing path fails with ErrExist, rename replaces an existing
// file.
type Fake struct {
	mu    sync.Mutex
	name  string
	dirs  map[string]bool
	files map[string][]byte
}

func NewFake(name string) *Fake {
	return &Fake{
		name:  name,
		dirs:  map[string]bool{"/": true, "/ext": true, "/int": true},
		files: map[string][]byte{},
	}
}

func (f *Fake) Name() string { return f.name }

func (f *Fake) Stat(p string) (*File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = path.Clean(p)
	if f.dirs[p] {
		return &File{Name: path.Base(p), IsDir: true}, nil
	}
	if data, ok := f.files[p]; ok {
		return &File{Name: path.Base(p), Size: int64(len(data))}, nil
	}
	return nil, ErrNotExist
}

func (f *Fake) List(p string) ([]*File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = path.Clean(p)
	if !f.dirs[p] {
		return nil, ErrNotExist
	}
	var out []*File
	for d := range f.dirs {
		if path.Dir(d) == p && d != p {
			out = append(out, &File{Name: path.Base(d), IsDir: true})
		}
	}
	for name, data := range f.files {
		if path.Dir(name) == p {
			sum := md5.Sum(data)
			out = append(out, &File{Name: path.Base(name), Size: int64(len(data)), Md5: hex.EncodeToString(sum[:])})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *Fake) Read(p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[path.Clean(p)]
	if !ok {
		return nil, ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (f *Fake) Write(p string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = path.Clean(p)
	if !f.dirs[path.Dir(p)] || f.dirs[p] {
		return ErrNotExist
	}
	f.files[p] = append([]byte(nil), data...)
	return nil
}

func (f *Fake) Mkdir(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = path.Clean(p)
	if f.dirs[p] {
		return ErrExist
	}
	if _, ok := f.files[p]; ok {
		return ErrExist
	}
	if !f.dirs[path.Dir(p)] {
		return ErrNotExist
	}
	f.dirs[p] = true
	return nil
}

func (f *Fake) Delete(p string, recursive bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = path.Clean(p)
	if _, ok := f.files[p]; ok {
		delete(f.files, p)
		return nil
	}
	if !f.dirs[p] {
		return ErrNotExist
	}
	for name := range f.files {
		if strings.HasPrefix(name, p+"/") {
			if !recursive {
				return ErrExist
			}
			delete(f.files, name)
		}
	}
	for d := range f.dirs {
		if strings.HasPrefix(d, p+"/") {
			if !recursive {
				return ErrExist
			}
			delete(f.dirs, d)
		}
	}
	delete(f.dirs, p)
	return nil
}

func (f *Fake) Rename(oldpath, newpath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	oldpath, newpath = path.Clean(oldpath), path.Clean(newpath)
	data, ok := f.files[oldpath]
	if !ok {
		return ErrNotExist
	}
	if f.dirs[newpath] {
		return ErrExist
	}
	if !f.dirs[path.Dir(newpath)] {
		return ErrNotExist
	}
	delete(f.files, oldpath)
	f.files[newpath] = data
	return nil
}

func (f *Fake) Md5(p string) (string, error) {
	data, err := f.Read(p)
	if err != nil {
		return "", err
	}
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:]), nil
}

func (f *Fake) Close() error { return nil }
