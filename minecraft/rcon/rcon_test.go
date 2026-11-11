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

package rcon

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/poolpOrg/plakar-integrations/minecraft/rcon/rcontest"
	"github.com/stretchr/testify/require"
)

func TestCommand(t *testing.T) {
	srv := rcontest.New(t, "secret", nil)

	c, err := Dial(t.Context(), srv.Addr(), "secret", time.Second)
	require.NoError(t, err)
	defer c.Close()

	reply, err := c.Command("save-all flush")
	require.NoError(t, err)
	require.Equal(t, "Saved the game", reply)

	reply, err = c.Command("list")
	require.NoError(t, err)
	require.Contains(t, reply, "Unknown")

	require.Equal(t, []string{"save-all flush", "list"}, srv.Commands())
}

func TestBadPassword(t *testing.T) {
	srv := rcontest.New(t, "secret", nil)

	_, err := Dial(t.Context(), srv.Addr(), "wrong", time.Second)
	require.ErrorIs(t, err, ErrAuth)
	require.Empty(t, srv.Commands())
}

func TestNobodyListens(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	ln.Close()

	_, err = Dial(t.Context(), addr, "secret", time.Second)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAuth)
}

func TestCommandTooLong(t *testing.T) {
	srv := rcontest.New(t, "secret", nil)

	c, err := Dial(t.Context(), srv.Addr(), "secret", time.Second)
	require.NoError(t, err)
	defer c.Close()

	_, err = c.Command(string(make([]byte, maxCommand+1)))
	require.ErrorContains(t, err, "too long")
}

// A peer announcing a packet bigger than the protocol allows is cut off
// rather than read into memory.
func TestOversizedReply(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], 1<<30)
		c.Write(hdr[:])
		<-done
	}()

	_, err = Dial(context.Background(), ln.Addr().String(), "secret", 5*time.Second)
	require.ErrorContains(t, err, "invalid packet length")
}

func TestTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		<-done
	}()

	start := time.Now()
	_, err = Dial(context.Background(), ln.Addr().String(), "secret", 100*time.Millisecond)
	require.Error(t, err)
	require.Less(t, time.Since(start), time.Second)
}
