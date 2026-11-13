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
	"io"
	"testing"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/kloset/connectors/storage"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/poolpOrg/plakar-integrations/flipper/device"
	"github.com/stretchr/testify/require"
)

func mac(b byte) integration.MAC {
	var m integration.MAC
	for i := range m {
		m[i] = b
	}
	return m
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	f := device.NewFake("Lc4str1g1")
	s := New(device.Location{Path: "/ext/plakar/store"}, f)

	require.Equal(t, "Lc4str1g1", s.Origin())
	require.Equal(t, "/ext/plakar/store", s.Root())
	require.Error(t, s.Ping(ctx))

	require.NoError(t, s.Create(ctx, []byte("config")))
	require.NoError(t, s.Ping(ctx))

	config, err := s.Open(ctx)
	require.NoError(t, err)
	require.Equal(t, "config", string(config))

	// create refuses a non-empty directory
	require.Error(t, New(device.Location{Path: "/ext/plakar/store"}, f).Create(ctx, nil))

	for _, res := range []storage.StorageResource{storage.StorageResourcePackfile, storage.StorageResourceState, storage.StorageResourceLock} {
		macs, err := s.List(ctx, res)
		require.NoError(t, err)
		require.Empty(t, macs)
	}

	n, err := s.Put(ctx, storage.StorageResourcePackfile, mac(1), bytes.NewReader([]byte("0123456789")))
	require.NoError(t, err)
	require.Equal(t, int64(10), n)

	// same MAC twice is fine
	_, err = s.Put(ctx, storage.StorageResourcePackfile, mac(1), bytes.NewReader([]byte("0123456789")))
	require.NoError(t, err)

	macs, err := s.List(ctx, storage.StorageResourcePackfile)
	require.NoError(t, err)
	require.Equal(t, []integration.MAC{mac(1)}, macs)

	// no stray tmp files
	entries, err := f.List("/ext/plakar/store/packfiles")
	require.NoError(t, err)
	require.Len(t, entries, 1)

	rd, err := s.Get(ctx, storage.StorageResourcePackfile, mac(1), nil)
	require.NoError(t, err)
	data, err := io.ReadAll(rd)
	require.NoError(t, err)
	require.Equal(t, "0123456789", string(data))

	rd, err = s.Get(ctx, storage.StorageResourcePackfile, mac(1), &storage.Range{Offset: 3, Length: 4})
	require.NoError(t, err)
	data, err = io.ReadAll(rd)
	require.NoError(t, err)
	require.Equal(t, "3456", string(data))

	_, err = s.Get(ctx, storage.StorageResourcePackfile, mac(1), &storage.Range{Offset: 8, Length: 4})
	require.Error(t, err)

	_, err = s.Get(ctx, storage.StorageResourcePackfile, mac(2), nil)
	require.ErrorIs(t, err, repository.ErrPackfileNotFound)

	_, err = s.Put(ctx, storage.StorageResourceState, mac(3), bytes.NewReader([]byte("state")))
	require.NoError(t, err)
	_, err = s.Put(ctx, storage.StorageResourceLock, mac(4), bytes.NewReader([]byte("lock")))
	require.NoError(t, err)

	size, err := s.Size(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(10+5+4), size)

	require.NoError(t, s.Delete(ctx, storage.StorageResourceLock, mac(4)))
	macs, err = s.List(ctx, storage.StorageResourceLock)
	require.NoError(t, err)
	require.Empty(t, macs)

	// the cache does not outlive the packfile
	require.NoError(t, s.Delete(ctx, storage.StorageResourcePackfile, mac(1)))
	_, err = s.Get(ctx, storage.StorageResourcePackfile, mac(1), nil)
	require.ErrorIs(t, err, repository.ErrPackfileNotFound)

	_, err = s.List(ctx, storage.StorageResourceECCPackfile)
	require.Error(t, err)
}

func TestPackCache(t *testing.T) {
	c := newPackCache(2, 100)
	loads := 0
	load := func(data []byte) func() ([]byte, error) {
		return func() ([]byte, error) { loads++; return data, nil }
	}

	_, err := c.get(mac(1), load(make([]byte, 40)))
	require.NoError(t, err)
	_, err = c.get(mac(1), load(nil))
	require.NoError(t, err)
	require.Equal(t, 1, loads)

	_, err = c.get(mac(2), load(make([]byte, 40)))
	require.NoError(t, err)
	_, err = c.get(mac(3), load(make([]byte, 40)))
	require.NoError(t, err)
	require.Equal(t, 3, loads)
	require.Len(t, c.entries, 2)
	require.NotContains(t, c.entries, mac(1))

	// too big to keep, still served
	big, err := c.get(mac(5), load(make([]byte, 200)))
	require.NoError(t, err)
	require.Len(t, big, 200)
	require.NotContains(t, c.entries, mac(5))

	c.evict(mac(2))
	require.NotContains(t, c.entries, mac(2))
	require.Equal(t, 40, c.bytes)
}
