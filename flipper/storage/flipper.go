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

package storage

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sync"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/PlakarKorp/kloset/connectors/storage"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/poolpOrg/plakar-integrations/flipper/device"
)

// The kloset lives in a directory on the device:
//
//	CONFIG
//	packfiles/<mac>
//	states/<mac>
//	locks/<mac>
//
// The layout is flat on purpose: the fs store fans out into 256 bucket
// directories, which is 512 mkdir round-trips over the serial link.
//
// The RPC has no ranged read, a blob fetch reads the whole packfile, so
// the last few packfiles are kept in memory.

type Store struct {
	loc   device.Location
	conn  device.Conn
	cache *packCache
}

func init() {
	storage.Register("flipper", 0, NewStore)
}

func NewStore(ctx context.Context, proto string, config map[string]string) (storage.Store, error) {
	loc, err := device.ParseLocation(proto, config["location"])
	if err != nil {
		return nil, err
	}

	conn, err := device.Open(ctx, loc.Name, config["port"])
	if err != nil {
		return nil, err
	}

	return New(loc, conn), nil
}

func New(loc device.Location, conn device.Conn) *Store {
	return &Store{
		loc:   loc,
		conn:  conn,
		cache: newPackCache(4, 64<<20),
	}
}

func (s *Store) Origin() string {
	if name := s.conn.Name(); name != "" {
		return name
	}
	return "flipper"
}

func (s *Store) Type() string          { return "flipper" }
func (s *Store) Root() string          { return s.loc.Path }
func (s *Store) Flags() location.Flags { return 0 }

func (s *Store) Mode(context.Context) (storage.Mode, error) {
	return storage.ModeRead | storage.ModeWrite, nil
}

func (s *Store) path(elems ...string) string {
	return path.Join(append([]string{s.loc.Path}, elems...)...)
}

func (s *Store) dir(res storage.StorageResource) (string, error) {
	switch res {
	case storage.StorageResourcePackfile:
		return s.path("packfiles"), nil
	case storage.StorageResourceState:
		return s.path("states"), nil
	case storage.StorageResourceLock:
		return s.path("locks"), nil
	}
	return "", errors.ErrUnsupported
}

func (s *Store) file(res storage.StorageResource, mac integration.MAC) (string, error) {
	dir, err := s.dir(res)
	if err != nil {
		return "", err
	}
	return path.Join(dir, hex.EncodeToString(mac[:])), nil
}

func (s *Store) mkdirAll(dir string) error {
	if parent := path.Dir(dir); parent != dir {
		if _, err := s.conn.Stat(parent); errors.Is(err, device.ErrNotExist) {
			if err := s.mkdirAll(parent); err != nil {
				return err
			}
		}
	}
	if err := s.conn.Mkdir(dir); err != nil && !errors.Is(err, device.ErrExist) {
		return err
	}
	return nil
}

func (s *Store) Create(ctx context.Context, config []byte) error {
	st, err := s.conn.Stat(s.loc.Path)
	switch {
	case err == nil:
		if !st.IsDir {
			return fmt.Errorf("%s is not a directory", s.loc.Path)
		}
		entries, err := s.conn.List(s.loc.Path)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return fmt.Errorf("directory %s is not empty", s.loc.Path)
		}
	case errors.Is(err, device.ErrNotExist):
		if err := s.mkdirAll(s.loc.Path); err != nil {
			return err
		}
	default:
		return err
	}

	for _, sub := range []string{"packfiles", "states", "locks"} {
		if err := s.conn.Mkdir(s.path(sub)); err != nil {
			return err
		}
	}

	return s.conn.Write(s.path("CONFIG"), config)
}

func (s *Store) Open(ctx context.Context) ([]byte, error) {
	return s.conn.Read(s.path("CONFIG"))
}

func (s *Store) Ping(ctx context.Context) error {
	_, err := s.conn.Stat(s.path("CONFIG"))
	return err
}

func (s *Store) Size(ctx context.Context) (int64, error) {
	var size int64
	for _, res := range []storage.StorageResource{storage.StorageResourcePackfile, storage.StorageResourceState, storage.StorageResourceLock} {
		dir, _ := s.dir(res)
		entries, err := s.conn.List(dir)
		if err != nil {
			return -1, err
		}
		for _, e := range entries {
			if !e.IsDir {
				size += e.Size
			}
		}
	}
	return size, nil
}

func (s *Store) List(ctx context.Context, res storage.StorageResource) ([]integration.MAC, error) {
	dir, err := s.dir(res)
	if err != nil {
		return nil, err
	}

	entries, err := s.conn.List(dir)
	if err != nil {
		return nil, err
	}

	macs := make([]integration.MAC, 0, len(entries))
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		raw, err := hex.DecodeString(e.Name)
		if err != nil || len(raw) != len(integration.MAC{}) {
			continue
		}
		macs = append(macs, integration.MAC(raw))
	}
	return macs, nil
}

// Put writes next to the final name then renames, so that a transfer
// cut short leaves a stray .tmp rather than a truncated packfile that
// List would report as valid.
func (s *Store) Put(ctx context.Context, res storage.StorageResource, mac integration.MAC, rd io.Reader) (int64, error) {
	final, err := s.file(res, mac)
	if err != nil {
		return -1, err
	}

	data, err := io.ReadAll(rd)
	if err != nil {
		return -1, err
	}

	tmp := final + ".tmp"
	if err := s.conn.Write(tmp, data); err != nil {
		return -1, err
	}

	if err := s.conn.Rename(tmp, final); err != nil {
		_ = s.conn.Delete(tmp, false)
		if errors.Is(err, device.ErrExist) {
			// same MAC, same content: already there
			return int64(len(data)), nil
		}
		return -1, err
	}

	return int64(len(data)), nil
}

func (s *Store) Get(ctx context.Context, res storage.StorageResource, mac integration.MAC, rg *storage.Range) (io.ReadCloser, error) {
	file, err := s.file(res, mac)
	if err != nil {
		return nil, err
	}

	var data []byte
	if res == storage.StorageResourcePackfile {
		data, err = s.cache.get(mac, func() ([]byte, error) { return s.conn.Read(file) })
		if errors.Is(err, device.ErrNotExist) {
			err = repository.ErrPackfileNotFound
		}
	} else {
		data, err = s.conn.Read(file)
	}
	if err != nil {
		return nil, err
	}

	if rg != nil {
		end := rg.Offset + uint64(rg.Length)
		if end > uint64(len(data)) {
			return nil, fmt.Errorf("range %d+%d past the end of %s (%d bytes)", rg.Offset, rg.Length, path.Base(file), len(data))
		}
		data = data[rg.Offset:end]
	}

	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *Store) Delete(ctx context.Context, res storage.StorageResource, mac integration.MAC) error {
	file, err := s.file(res, mac)
	if err != nil {
		return err
	}
	if res == storage.StorageResourcePackfile {
		s.cache.evict(mac)
	}
	return s.conn.Delete(file, false)
}

func (s *Store) Close(ctx context.Context) error {
	return s.conn.Close()
}

// packCache keeps the most recently read packfiles, bounded in count
// and in bytes.  A packfile bigger than the byte budget is served but
// not kept.
type packCache struct {
	mu         sync.Mutex
	entries    map[integration.MAC][]byte
	order      []integration.MAC
	maxEntries int
	maxBytes   int
	bytes      int
}

func newPackCache(maxEntries, maxBytes int) *packCache {
	return &packCache{
		entries:    make(map[integration.MAC][]byte),
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
	}
}

func (c *packCache) get(mac integration.MAC, load func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if data, ok := c.entries[mac]; ok {
		return data, nil
	}

	data, err := load()
	if err != nil {
		return nil, err
	}

	if len(data) > c.maxBytes {
		return data, nil
	}

	for len(c.order) >= c.maxEntries || c.bytes+len(data) > c.maxBytes {
		c.dropOldest()
	}
	c.entries[mac] = data
	c.order = append(c.order, mac)
	c.bytes += len(data)
	return data, nil
}

func (c *packCache) dropOldest() {
	if len(c.order) == 0 {
		return
	}
	oldest := c.order[0]
	c.order = c.order[1:]
	c.bytes -= len(c.entries[oldest])
	delete(c.entries, oldest)
}

func (c *packCache) evict(mac integration.MAC) {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, ok := c.entries[mac]
	if !ok {
		return
	}
	c.bytes -= len(data)
	delete(c.entries, mac)
	for i, m := range c.order {
		if m == mac {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}
