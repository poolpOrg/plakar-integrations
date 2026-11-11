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

// Package rcontest is a fake RCON console for tests.
package rcontest

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
)

// Server listens on the loopback, checks the password and records the
// commands it is sent.  Replies come from Handler, which defaults to
// the vanilla answers of the save commands.
type Server struct {
	Password string
	Handler  func(cmd string) string

	ln   net.Listener
	mu   sync.Mutex
	cmds []string
	wg   sync.WaitGroup
}

// New serves on a free loopback port until the test ends.
func New(t testing.TB, password string, handler func(cmd string) string) *Server {
	t.Helper()

	s, err := Listen("127.0.0.1:0", password, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// Listen serves on addr until Close.
func Listen(addr, password string, handler func(cmd string) string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	if handler == nil {
		handler = Vanilla
	}

	s := &Server{Password: password, Handler: handler, ln: ln}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// Vanilla answers the save commands like a stock server does.
func Vanilla(cmd string) string {
	switch cmd {
	case "save-off":
		return "Automatic saving is now disabled"
	case "save-on":
		return "Automatic saving is now enabled"
	case "save-all flush":
		return "Saved the game"
	}
	return "Unknown or incomplete command, see below for error"
}

// Addr is the host:port the server listens on.
func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

// Commands lists the commands received so far, in order.
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

// Close stops serving and waits for the connections to end.
func (s *Server) Close() {
	s.ln.Close()
	s.wg.Wait()
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer c.Close()
			s.handle(c)
		}()
	}
}

func (s *Server) handle(c net.Conn) {
	authed := false
	for {
		id, typ, body, err := read(c)
		if err != nil {
			return
		}

		switch {
		case typ == 3:
			if body != s.Password {
				write(c, -1, 2, "")
				return
			}
			authed = true
			write(c, id, 2, "")

		case typ == 2 && authed:
			s.mu.Lock()
			s.cmds = append(s.cmds, body)
			s.mu.Unlock()
			write(c, id, 0, s.Handler(strings.TrimSpace(body)))

		default:
			return
		}
	}
}

func read(c net.Conn) (id, typ int32, body string, err error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return 0, 0, "", err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n < 10 || n > 4106 {
		return 0, 0, "", io.ErrUnexpectedEOF
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		return 0, 0, "", err
	}
	return int32(binary.LittleEndian.Uint32(buf[0:4])), int32(binary.LittleEndian.Uint32(buf[4:8])), string(buf[8 : n-2]), nil
}

func write(c net.Conn, id, typ int32, body string) {
	buf := make([]byte, 0, 14+len(body))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(10+len(body)))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(id))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(typ))
	buf = append(buf, body...)
	buf = append(buf, 0, 0)
	c.Write(buf)
}
