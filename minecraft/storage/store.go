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

// Package storage keeps a kloset in the world of a Minecraft server:
// packfiles and states are written as terrain into the region files of
// the world named by server.properties, locks and the configuration
// are plain files under <world>/plakar.  The server must be stopped
// while the store writes, a running one serves the world to players.
package storage

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"strings"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"github.com/poolpOrg/plakar-integrations/minecraft/server"
)

var (
	ErrRunning  = errors.New("the server is running, stop it before writing to its world")
	ErrNotFound = errors.New("resource not found")
	ErrExists   = errors.New("the world already holds a kloset")
)

type Store struct {
	location string // the server directory
	world    string // the world directory under it
}

func NewStore(ctx context.Context, proto string, config map[string]string) (integration.Store, error) {
	dir := strings.TrimPrefix(config["location"], proto+"://")
	if dir == "" {
		return nil, fmt.Errorf("location is required")
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("not an absolute path: %s", dir)
	}
	return &Store{location: dir}, nil
}

func (s *Store) Origin() string        { return "localhost" }
func (s *Store) Root() string          { return s.location }
func (s *Store) Type() string          { return "minecraft" }
func (s *Store) Flags() location.Flags { return location.FLAG_LOCALFS }

func (s *Store) Mode(context.Context) (integration.Mode, error) {
	return integration.ModeRead | integration.ModeWrite, nil
}

// regionDir is where the overworld's region files are: since 26.3 the
// server keeps them under dimensions/minecraft/overworld, and moves
// them there the first time it opens an older world.
func (s *Store) regionDir() string {
	modern := filepath.Join(s.world, "dimensions", "minecraft", "overworld", "region")
	if st, err := os.Stat(modern); err == nil && st.IsDir() {
		return modern
	}
	return filepath.Join(s.world, "region")
}
func (s *Store) plakarDir() string { return filepath.Join(s.world, "plakar") }
func (s *Store) lockDir() string   { return filepath.Join(s.plakarDir(), "locks") }
func (s *Store) configPath() string {
	return filepath.Join(s.plakarDir(), "CONFIG")
}

// open resolves the world directory from server.properties, "world"
// when there is none, and tells whether a server is running on it.
func (s *Store) open() (running bool, err error) {
	srv, err := server.Open(s.location)
	if err != nil {
		return false, err
	}
	level := srv.Properties["level-name"]
	if level == "" {
		level = "world"
	}
	s.world = filepath.Join(s.location, level)
	return srv.Running()
}

func (s *Store) refuseRunning() error {
	running, err := s.open()
	if err != nil {
		return err
	}
	if running {
		return ErrRunning
	}
	return nil
}

func (s *Store) Create(ctx context.Context, config []byte) error {
	if err := os.MkdirAll(s.location, 0700); err != nil {
		return err
	}
	if err := s.refuseRunning(); err != nil {
		return err
	}
	if _, err := os.Stat(s.configPath()); err == nil {
		return ErrExists
	}
	if entries, err := os.ReadDir(s.regionDir()); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s already has regions, the kloset wants an empty world", s.world)
	}

	for _, dir := range []string{s.regionDir(), s.lockDir()} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	level := filepath.Join(s.world, "level.dat")
	if _, err := os.Stat(level); errors.Is(err, fs.ErrNotExist) {
		if err := writeLevel(level, filepath.Base(s.world)); err != nil {
			return err
		}
	}
	return writeFileAtomic(s.configPath(), bytes.NewReader(config))
}

func (s *Store) Open(ctx context.Context) ([]byte, error) {
	if _, err := s.open(); err != nil {
		return nil, err
	}
	return os.ReadFile(s.configPath())
}

func (s *Store) Ping(ctx context.Context) error { return nil }

func (s *Store) Close(ctx context.Context) error { return nil }

func (s *Store) Size(ctx context.Context) (int64, error) {
	var size int64
	err := filepath.WalkDir(s.world, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func (s *Store) List(ctx context.Context, res integration.StorageResource, _ integration.StorageFlag) iter.Seq2[*integration.StorageResourceEntry, error] {
	return func(yield func(*integration.StorageResourceEntry, error) bool) {
		entries, err := s.list(res)
		if err != nil {
			yield(nil, err)
			return
		}
		for _, e := range entries {
			if !yield(e, nil) {
				return
			}
		}
	}
}

func (s *Store) list(res integration.StorageResource) ([]*integration.StorageResourceEntry, error) {
	if res == integration.StorageResourceLock {
		return s.listLocks()
	}
	if _, err := s.open(); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(s.regionDir())
	if err != nil {
		return nil, err
	}
	found := make([]*integration.StorageResourceEntry, 0)
	for _, e := range entries {
		x, z, ok := parseRegionFile(e.Name())
		if !ok {
			continue
		}
		h, err := s.readHeader(x, z)
		if err != nil {
			continue // not one of ours, or torn, either way not a resource
		}
		if h.index != 0 || h.resource != uint8(res) {
			continue
		}
		entry := &integration.StorageResourceEntry{MAC: h.mac, Size: int64(h.length)}
		if info, err := e.Info(); err == nil {
			entry.MTime = info.ModTime()
		}
		found = append(found, entry)
	}
	return found, nil
}

func (s *Store) Put(ctx context.Context, res integration.StorageResource, _ integration.StorageFlag, mac integration.MAC, rd io.Reader) (int64, error) {
	if res == integration.StorageResourceLock {
		return s.putLock(mac, rd)
	}
	if err := s.refuseRunning(); err != nil {
		return 0, err
	}

	data, err := io.ReadAll(rd)
	if err != nil {
		return 0, err
	}

	// the same MAC is the same content, nothing to rewrite
	if _, _, _, err := s.locate(res, mac); err == nil {
		return int64(len(data)), nil
	}

	h := header{
		codec:    codecTerrain,
		resource: uint8(res),
		mac:      mac,
		length:   uint64(len(data)),
		count:    regionCount(uint64(len(data))),
	}

	x, z, err := s.place(h)
	if err != nil {
		return 0, err
	}
	for i := uint32(0); i < h.count; i++ {
		rx, rz := regionAt(x, z, i)
		chunks := encodeRegion(h, i, x, z, data)
		if err := writeRegion(regionFile(s.regionDir(), rx, rz), chunks); err != nil {
			return 0, err
		}
	}
	return int64(len(data)), nil
}

// place finds a spot where every region of the resource is free.
func (s *Store) place(h header) (int32, int32, error) {
	for probe := 0; probe < 64; probe++ {
		x, z := footprint(h.mac, probe)
		free := true
		for i := uint32(0); i < h.count && free; i++ {
			rx, rz := regionAt(x, z, i)
			if _, err := os.Stat(regionFile(s.regionDir(), rx, rz)); err == nil {
				free = false
			}
		}
		if free {
			return x, z, nil
		}
	}
	return 0, 0, fmt.Errorf("no free spot for %x", h.mac[:8])
}

// locate finds the origin of a resource, probing the way place did.
func (s *Store) locate(res integration.StorageResource, mac integration.MAC) (x, z int32, h header, err error) {
	for probe := 0; probe < 64; probe++ {
		x, z = footprint(mac, probe)
		if _, err := os.Stat(regionFile(s.regionDir(), x, z)); err != nil {
			return 0, 0, header{}, ErrNotFound
		}
		h, err := s.readHeader(x, z)
		if err != nil {
			continue
		}
		if h.index == 0 && h.resource == uint8(res) && h.mac == [32]byte(mac) {
			return x, z, h, nil
		}
	}
	return 0, 0, header{}, ErrNotFound
}

func (s *Store) readHeader(x, z int32) (header, error) {
	rr, err := openRegion(regionFile(s.regionDir(), x, z))
	if err != nil {
		return header{}, err
	}
	defer rr.Close()
	c, err := rr.chunk(0, 0)
	if err != nil {
		return header{}, err
	}
	data, err := decodeChunk(c)
	if err != nil {
		return header{}, err
	}
	return unmarshalHeader(data)
}

func (s *Store) Get(ctx context.Context, res integration.StorageResource, _ integration.StorageFlag, mac integration.MAC, rg *integration.Range) (io.ReadCloser, error) {
	if res == integration.StorageResourceLock {
		return s.getLock(mac)
	}
	if _, err := s.open(); err != nil {
		return nil, err
	}

	x, z, h, err := s.locate(res, mac)
	if err != nil {
		return nil, err
	}

	var offset, length uint64
	if rg == nil {
		offset, length = 0, h.length
	} else {
		offset, length = rg.Offset, uint64(rg.Length)
		if offset > h.length {
			return nil, fmt.Errorf("offset %d beyond %d bytes", offset, h.length)
		}
		length = min(length, h.length-offset)
	}
	if length == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}

	first := offset / chunkBytes
	last := (offset + length - 1) / chunkBytes
	buf := make([]byte, 0, (last-first+1)*chunkBytes)

	var rr *regionReader
	var opened uint32
	defer func() {
		if rr != nil {
			rr.Close()
		}
	}()
	for c := first; c <= last; c++ {
		i := uint32(c / chunksPerRegion)
		if rr == nil || opened != i {
			if rr != nil {
				rr.Close()
			}
			rx, rz := regionAt(x, z, i)
			rr, err = openRegion(regionFile(s.regionDir(), rx, rz))
			if err != nil {
				return nil, err
			}
			opened = i
		}
		cx, cz := chunkAt(int(c % chunksPerRegion))
		chunk, err := rr.chunk(cx, cz)
		if err != nil {
			return nil, fmt.Errorf("chunk %d,%d of region %d: %w", cx, cz, i, err)
		}
		data, err := decodeChunk(chunk)
		if err != nil {
			return nil, fmt.Errorf("chunk %d,%d of region %d: %w", cx, cz, i, err)
		}
		buf = append(buf, data...)
	}

	skip := offset - first*chunkBytes
	return io.NopCloser(bytes.NewReader(buf[skip : skip+length])), nil
}

func (s *Store) Delete(ctx context.Context, res integration.StorageResource, _ integration.StorageFlag, mac integration.MAC) error {
	if res == integration.StorageResourceLock {
		return s.deleteLock(mac)
	}
	if err := s.refuseRunning(); err != nil {
		return err
	}

	x, z, h, err := s.locate(res, mac)
	if err != nil {
		return err
	}
	// the header goes last so that a failed delete stays findable
	for i := h.count; i > 0; i-- {
		rx, rz := regionAt(x, z, i-1)
		if err := os.Remove(regionFile(s.regionDir(), rx, rz)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) lockPath(mac integration.MAC) string {
	return filepath.Join(s.lockDir(), hex.EncodeToString(mac[:]))
}

func (s *Store) listLocks() ([]*integration.StorageResourceEntry, error) {
	if _, err := s.open(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.lockDir())
	if err != nil {
		return nil, err
	}
	found := make([]*integration.StorageResourceEntry, 0)
	for _, e := range entries {
		id, err := hex.DecodeString(e.Name())
		if err != nil || len(id) != 32 || !e.Type().IsRegular() {
			continue
		}
		entry := &integration.StorageResourceEntry{MAC: integration.MAC(id)}
		if info, err := e.Info(); err == nil {
			entry.Size = info.Size()
			entry.MTime = info.ModTime()
		}
		found = append(found, entry)
	}
	return found, nil
}

func (s *Store) putLock(mac integration.MAC, rd io.Reader) (int64, error) {
	if _, err := s.open(); err != nil {
		return 0, err
	}
	data, err := io.ReadAll(rd)
	if err != nil {
		return 0, err
	}
	return int64(len(data)), writeFileAtomic(s.lockPath(mac), bytes.NewReader(data))
}

func (s *Store) getLock(mac integration.MAC) (io.ReadCloser, error) {
	if _, err := s.open(); err != nil {
		return nil, err
	}
	return os.Open(s.lockPath(mac))
}

func (s *Store) deleteLock(mac integration.MAC) error {
	if _, err := s.open(); err != nil {
		return err
	}
	return os.Remove(s.lockPath(mac))
}

func writeFileAtomic(path string, rd io.Reader) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp.")
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, rd); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		os.Remove(f.Name())
		return err
	}
	return nil
}
