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

// Package adb is a client for the Android Debug Bridge server.
//
// It speaks the host protocol over TCP to a running adb server (the one
// started by the adb command line tool, listening on 127.0.0.1:5037 by
// default), selects a device by serial number and drives the device-side
// SYNC and shell services.  Only the subset needed to enumerate, read and
// write files on a device is implemented; USB is never touched directly,
// the adb server owns the transport.
//
// Everything received from the server or the device is treated as
// untrusted input: lengths are bounded, identifiers are checked and a
// malformed reply ends the connection with ErrProtocol.
package adb

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// DefaultServerAddr is where the adb server listens unless told otherwise.
const DefaultServerAddr = "127.0.0.1:5037"

// maxHostReply bounds host service replies ("host:devices-l", features...).
// The wire format encodes the length on four hex digits, so 64 KiB is also
// the protocol maximum.
const maxHostReply = 0xFFFF

// ErrProtocol is wrapped by every error caused by a reply that does not
// follow the adb wire protocol.
var ErrProtocol = errors.New("adb: protocol error")

// Error is a FAIL reply from the server or the device for a given service.
type Error struct {
	Service string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("adb: %s: %s", e.Service, e.Message)
}

// Dialer opens a new connection to the adb server.
type Dialer func(ctx context.Context) (net.Conn, error)

// Client talks to an adb server.  It is safe for concurrent use; every
// operation opens its own connection, as the adb protocol requires.
type Client struct {
	dial Dialer
}

// NewClient returns a client for the adb server at addr (host:port).  An
// empty addr means DefaultServerAddr.
func NewClient(addr string) *Client {
	if addr == "" {
		addr = DefaultServerAddr
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &Client{
		dial: func(ctx context.Context) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", addr)
		},
	}
}

// NewClientWithDialer returns a client that reaches the server through dial.
func NewClientWithDialer(dial Dialer) *Client {
	return &Client{dial: dial}
}

// Conn is one connection to the adb server.  After Transport it is bound
// to a device service and carries that service's stream.
type Conn struct {
	nc   net.Conn
	rd   *bufio.Reader
	stop func() bool
}

func (c *Client) open(ctx context.Context) (*Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nc, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("adb: cannot reach the adb server: %w", err)
	}
	conn := &Conn{nc: nc, rd: bufio.NewReaderSize(nc, 64*1024)}
	// A cancelled context closes the socket, which unblocks any pending
	// read or write with an error.  stop() detaches the watcher on Close.
	conn.stop = context.AfterFunc(ctx, func() { nc.Close() })
	return conn, nil
}

// Close releases the connection.
func (c *Conn) Close() error {
	if c.stop != nil {
		c.stop()
	}
	return c.nc.Close()
}

func (c *Conn) write(p []byte) error {
	_, err := c.nc.Write(p)
	if err != nil {
		return fmt.Errorf("adb: write: %w", err)
	}
	return nil
}

func (c *Conn) readFull(p []byte) error {
	if _, err := io.ReadFull(c.rd, p); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return fmt.Errorf("adb: connection closed: %w", err)
		}
		return fmt.Errorf("adb: read: %w", err)
	}
	return nil
}

// send writes a host-protocol request: four hex digits of length then the
// service string.
func (c *Conn) send(service string) error {
	if len(service) > maxHostReply {
		return fmt.Errorf("adb: service request too long")
	}
	return c.write([]byte(fmt.Sprintf("%04x%s", len(service), service)))
}

// readHexBlock reads a length-prefixed block (four hex digits then the
// payload), as used by host service replies and FAIL messages.
func (c *Conn) readHexBlock() ([]byte, error) {
	var hdr [4]byte
	if err := c.readFull(hdr[:]); err != nil {
		return nil, err
	}
	var raw [2]byte
	if _, err := hex.Decode(raw[:], hdr[:]); err != nil {
		return nil, fmt.Errorf("%w: bad length %q", ErrProtocol, hdr[:])
	}
	n := int(raw[0])<<8 | int(raw[1])
	buf := make([]byte, n)
	if err := c.readFull(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// readStatus reads the OKAY/FAIL reply to a request.
func (c *Conn) readStatus(service string) error {
	var id [4]byte
	if err := c.readFull(id[:]); err != nil {
		return err
	}
	switch string(id[:]) {
	case "OKAY":
		return nil
	case "FAIL":
		msg, err := c.readHexBlock()
		if err != nil {
			return err
		}
		return &Error{Service: service, Message: string(msg)}
	default:
		return fmt.Errorf("%w: unexpected reply %q to %s", ErrProtocol, id[:], service)
	}
}

// Request sends a service request and waits for its OKAY.
func (c *Conn) Request(service string) error {
	if err := c.send(service); err != nil {
		return err
	}
	return c.readStatus(service)
}

// Query sends a host service request and returns its length-prefixed reply.
func (c *Conn) Query(service string) ([]byte, error) {
	if err := c.Request(service); err != nil {
		return nil, err
	}
	return c.readHexBlock()
}

func (c *Client) hostQuery(ctx context.Context, service string) ([]byte, error) {
	conn, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.Query(service)
}

// Version returns the adb server protocol version (an hexadecimal string
// such as "0029").
func (c *Client) Version(ctx context.Context) (string, error) {
	v, err := c.hostQuery(ctx, "host:version")
	if err != nil {
		return "", err
	}
	return string(v), nil
}

// Devices lists the devices known to the server, whatever their state.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	out, err := c.hostQuery(ctx, "host:devices-l")
	if err != nil {
		return nil, err
	}
	return ParseDevices(string(out))
}

// Features returns the feature set advertised by the device.  The device
// must be in the "device" state.
func (c *Client) Features(ctx context.Context, serial string) (Features, error) {
	if err := ValidateSerial(serial); err != nil {
		return nil, err
	}
	out, err := c.hostQuery(ctx, "host-serial:"+serial+":features")
	if err != nil {
		return nil, err
	}
	return ParseFeatures(string(out)), nil
}

// Transport opens a connection bound to the device with the given serial
// and starts the given device service ("sync:", "shell,v2,raw:...") on it.
// The device is addressed by serial only: the server never substitutes
// another device.
func (c *Client) Transport(ctx context.Context, serial, service string) (*Conn, error) {
	if err := ValidateSerial(serial); err != nil {
		return nil, err
	}
	conn, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	if err := conn.Request("host:transport:" + serial); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Request(service); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// ValidateSerial rejects serials that could not come from the adb server
// or would break a host request.  USB serials are alphanumeric, emulators
// look like "emulator-5554" and TCP devices like "192.168.1.2:5555".
func ValidateSerial(serial string) error {
	if serial == "" {
		return errors.New("adb: empty device serial")
	}
	if len(serial) > 255 {
		return errors.New("adb: device serial too long")
	}
	for _, r := range serial {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':', r == '[', r == ']':
		default:
			return fmt.Errorf("adb: invalid character %q in device serial", r)
		}
	}
	if strings.Contains(serial, "..") {
		return errors.New("adb: invalid device serial")
	}
	return nil
}
