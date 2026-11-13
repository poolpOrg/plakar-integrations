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
	"net"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShared(t *testing.T) {
	owner := NewFake("Lc4str1g")
	require.NoError(t, owner.Mkdir("/ext/nfc"))
	require.NoError(t, owner.Write("/ext/nfc/a.nfc", []byte("aaa")))

	sock := filepath.Join(t.TempDir(), "flipper.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	defer ln.Close()
	go serve(ln, owner)

	r, err := dialRetry(sock, 0)
	require.NoError(t, err)
	defer r.Close()
	require.Equal(t, "Lc4str1g", r.Name())

	st, err := r.Stat("/ext/nfc/a.nfc")
	require.NoError(t, err)
	require.Equal(t, int64(3), st.Size)

	_, err = r.Stat("/ext/nope")
	require.ErrorIs(t, err, ErrNotExist)
	require.ErrorIs(t, r.Mkdir("/ext/nfc"), ErrExist)

	files, err := r.List("/ext/nfc")
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.NotEmpty(t, files[0].Md5)

	require.NoError(t, r.Write("/ext/nfc/b.nfc", []byte("bbb")))
	data, err := owner.Read("/ext/nfc/b.nfc")
	require.NoError(t, err)
	require.Equal(t, "bbb", string(data))

	sum, err := r.Md5("/ext/nfc/b.nfc")
	require.NoError(t, err)
	require.Len(t, sum, 32)

	require.NoError(t, r.Rename("/ext/nfc/b.nfc", "/ext/nfc/c.nfc"))
	require.NoError(t, r.Delete("/ext/nfc/c.nfc", false))
	_, err = owner.Stat("/ext/nfc/c.nfc")
	require.ErrorIs(t, err, ErrNotExist)
	require.NoError(t, r.Alert())

	// a second client at the same time
	r2, err := dial(sock)
	require.NoError(t, err)
	defer r2.Close()
	data, err = r2.Read("/ext/nfc/a.nfc")
	require.NoError(t, err)
	require.Equal(t, "aaa", string(data))
}

func TestSharedFailover(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "flipper.sock")

	first := NewFake("first")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	go serve(ln, first)

	s := &shared{sock: sock}
	require.NoError(t, s.connect())
	require.Equal(t, "first", s.Name())

	// the owner goes away, another one shows up on the same socket
	ln.Close()
	second := NewFake("second")
	ln, err = net.Listen("unix", sock)
	require.NoError(t, err)
	defer ln.Close()
	go serve(ln, second)

	_, err = s.Stat("/ext")
	require.NoError(t, err)
	require.Equal(t, "second", s.Name())
}

func TestSocketPath(t *testing.T) {
	p, err := socketPath("/dev/cu.usbmodemflip_Lc4str1g1")
	require.NoError(t, err)
	require.Equal(t, "cu.usbmodemflip_Lc4str1g1.sock", filepath.Base(p))

	p, err = socketPath("COM3")
	require.NoError(t, err)
	require.Equal(t, "COM3.sock", filepath.Base(p))
}
