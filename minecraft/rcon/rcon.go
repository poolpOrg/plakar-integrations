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

// Package rcon is a client for the RCON console of a Minecraft server,
// the Source RCON protocol over TCP.
package rcon

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Packet types.  The server answers an auth request with a typeCommand
// packet, not a typeResponse one.
const (
	typeResponse = 0
	typeCommand  = 2
	typeAuth     = 3
)

// A packet is a little endian length followed by that many bytes: a 4
// byte id, a 4 byte type, the body and two nul bytes.  The server caps
// the body of its replies at 4096 bytes and the body of requests at
// 1446; anything beyond that is not RCON.
const (
	minPacket  = 10
	maxPacket  = 4096 + minPacket
	maxCommand = 1446
)

var ErrAuth = errors.New("authentication failed")

// Conn is an authenticated connection to the console of a server.  It
// is not safe for concurrent use.
type Conn struct {
	conn    net.Conn
	timeout time.Duration
	id      int32
}

// Dial connects to addr and authenticates.  Every exchange with the
// server is bounded by timeout, no deadline when it is zero.
func Dial(ctx context.Context, addr, password string, timeout time.Duration) (*Conn, error) {
	d := net.Dialer{Timeout: timeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	c := &Conn{conn: nc, timeout: timeout}
	if err := c.auth(password); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

// Close drops the connection.
func (c *Conn) Close() error {
	return c.conn.Close()
}

func (c *Conn) auth(password string) error {
	id, err := c.write(typeAuth, password)
	if err != nil {
		return err
	}

	// a refused login is answered with id -1.  Some servers send an
	// empty response packet before the auth reply, skip it.
	for {
		rid, typ, _, err := c.read()
		if err != nil {
			return err
		}
		if typ != typeCommand {
			continue
		}
		if rid == -1 {
			return ErrAuth
		}
		if rid != id {
			return fmt.Errorf("unexpected packet id %d, want %d", rid, id)
		}
		return nil
	}
}

// Command runs a console command and returns the reply of the server.
// Only the first packet of the reply is read: the server splits
// replies longer than 4096 bytes, which the save commands never are.
func (c *Conn) Command(cmd string) (string, error) {
	id, err := c.write(typeCommand, cmd)
	if err != nil {
		return "", err
	}

	rid, typ, body, err := c.read()
	if err != nil {
		return "", err
	}
	if typ != typeResponse || rid != id {
		return "", fmt.Errorf("unexpected reply to %q: id %d, type %d", cmd, rid, typ)
	}
	return body, nil
}

func (c *Conn) deadline() error {
	if c.timeout == 0 {
		return c.conn.SetDeadline(time.Time{})
	}
	return c.conn.SetDeadline(time.Now().Add(c.timeout))
}

func (c *Conn) write(typ int32, body string) (int32, error) {
	if len(body) > maxCommand {
		return 0, fmt.Errorf("command too long: %d bytes", len(body))
	}
	if err := c.deadline(); err != nil {
		return 0, err
	}

	c.id++
	id := c.id

	buf := make([]byte, 0, 4+minPacket+len(body))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(minPacket+len(body)))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(id))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(typ))
	buf = append(buf, body...)
	buf = append(buf, 0, 0)

	if _, err := c.conn.Write(buf); err != nil {
		return 0, err
	}
	return id, nil
}

func (c *Conn) read() (id, typ int32, body string, err error) {
	if err := c.deadline(); err != nil {
		return 0, 0, "", err
	}

	var hdr [4]byte
	if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
		return 0, 0, "", err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n < minPacket || n > maxPacket {
		return 0, 0, "", fmt.Errorf("invalid packet length %d", n)
	}

	buf := make([]byte, n)
	if _, err := io.ReadFull(c.conn, buf); err != nil {
		return 0, 0, "", err
	}

	id = int32(binary.LittleEndian.Uint32(buf[0:4]))
	typ = int32(binary.LittleEndian.Uint32(buf[4:8]))
	body = string(buf[8 : n-2])
	return id, typ, body, nil
}
