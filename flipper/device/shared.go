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
	"encoding/gob"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A serial port opens once.  plakar routinely runs two connector
// processes against the same store at the same time (the command and
// the cached daemon rebuilding its state, or a backup from the device
// into a kloset on the device), so the first process to open the port
// becomes its owner and serves the others over a unix socket.  The
// socket lives in a private directory and dies with the owner.

type request struct {
	Op        string
	Path      string
	Path2     string
	Data      []byte
	Recursive bool
}

type response struct {
	Err   string
	Code  int
	Name  string
	File  *File
	Files []*File
	Data  []byte
	Sum   string
}

const (
	codeOK = iota
	codeError
	codeNotExist
	codeExist
)

func encodeError(err error) (int, string) {
	switch {
	case err == nil:
		return codeOK, ""
	case errors.Is(err, ErrNotExist):
		return codeNotExist, ""
	case errors.Is(err, ErrExist):
		return codeExist, ""
	}
	return codeError, err.Error()
}

func decodeError(code int, msg string) error {
	switch code {
	case codeOK:
		return nil
	case codeNotExist:
		return ErrNotExist
	case codeExist:
		return ErrExist
	}
	return errors.New(msg)
}

func socketPath(portName string) (string, error) {
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("plakar-flipper-%d", os.Getuid()))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	name := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '_'
		}
		return r
	}, strings.TrimPrefix(portName, "/dev/"))
	return filepath.Join(dir, name+".sock"), nil
}

// serve answers requests on ln with conn until ln is closed, then
// drops the clients so that they fail over rather than hang.
func serve(ln net.Listener, conn Conn) {
	var mu sync.Mutex
	clients := map[net.Conn]struct{}{}

	for {
		c, err := ln.Accept()
		if err != nil {
			break
		}
		mu.Lock()
		clients[c] = struct{}{}
		mu.Unlock()
		go func() {
			handle(c, conn)
			mu.Lock()
			delete(clients, c)
			mu.Unlock()
		}()
	}

	mu.Lock()
	defer mu.Unlock()
	for c := range clients {
		c.Close()
	}
}

func handle(c net.Conn, conn Conn) {
	defer c.Close()
	dec := gob.NewDecoder(c)
	enc := gob.NewEncoder(c)
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			return
		}
		var res response
		var err error
		switch req.Op {
		case "name":
			res.Name = conn.Name()
		case "stat":
			res.File, err = conn.Stat(req.Path)
		case "list":
			res.Files, err = conn.List(req.Path)
		case "read":
			res.Data, err = conn.Read(req.Path)
		case "write":
			err = conn.Write(req.Path, req.Data)
		case "mkdir":
			err = conn.Mkdir(req.Path)
		case "delete":
			err = conn.Delete(req.Path, req.Recursive)
		case "rename":
			err = conn.Rename(req.Path, req.Path2)
		case "md5":
			res.Sum, err = conn.Md5(req.Path)
		case "alert":
			if a, ok := conn.(interface{ Alert() error }); ok {
				err = a.Alert()
			}
		default:
			err = fmt.Errorf("unknown op %q", req.Op)
		}
		res.Code, res.Err = encodeError(err)
		if err := enc.Encode(&res); err != nil {
			return
		}
	}
}

// shared is what Open hands out: a Conn that is either the owner of
// the port or a client of it, and that reconnects when the owner it
// was talking to exits between two calls.
type shared struct {
	name     string
	portName string
	sock     string

	mu   sync.Mutex
	conn Conn
}

var errOwnerGone = errors.New("port owner went away")

func (s *shared) do(f func(Conn) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := f(s.conn)
	if _, ok := s.conn.(*remote); !ok || !errors.Is(err, errOwnerGone) {
		return err
	}

	s.conn.Close()
	if err := s.connect(); err != nil {
		return err
	}
	return f(s.conn)
}

func (s *shared) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.Name()
}

func (s *shared) Stat(path string) (f *File, err error) {
	err = s.do(func(c Conn) error { f, err = c.Stat(path); return err })
	return
}

func (s *shared) List(path string) (files []*File, err error) {
	err = s.do(func(c Conn) error { files, err = c.List(path); return err })
	return
}

func (s *shared) Read(path string) (data []byte, err error) {
	err = s.do(func(c Conn) error { data, err = c.Read(path); return err })
	return
}

func (s *shared) Write(path string, data []byte) error {
	return s.do(func(c Conn) error { return c.Write(path, data) })
}

func (s *shared) Mkdir(path string) error {
	return s.do(func(c Conn) error { return c.Mkdir(path) })
}

func (s *shared) Delete(path string, recursive bool) error {
	return s.do(func(c Conn) error { return c.Delete(path, recursive) })
}

func (s *shared) Rename(oldpath, newpath string) error {
	return s.do(func(c Conn) error { return c.Rename(oldpath, newpath) })
}

func (s *shared) Md5(path string) (sum string, err error) {
	err = s.do(func(c Conn) error { sum, err = c.Md5(path); return err })
	return
}

func (s *shared) Alert() error {
	return s.do(func(c Conn) error {
		if a, ok := c.(interface{ Alert() error }); ok {
			return a.Alert()
		}
		return nil
	})
}

func (s *shared) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.Close()
}

// remote is a Conn served by the owner of the port.
type remote struct {
	c    net.Conn
	enc  *gob.Encoder
	dec  *gob.Decoder
	mu   sync.Mutex
	name string
}

func dial(sock string) (*remote, error) {
	c, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	r := &remote{c: c, enc: gob.NewEncoder(c), dec: gob.NewDecoder(c)}
	res, err := r.call(request{Op: "name"})
	if err != nil {
		c.Close()
		return nil, err
	}
	r.name = res.Name
	return r, nil
}

// dialRetry keeps trying for a while: the owner may still be in its
// handshake with the device when a second process shows up.
func dialRetry(sock string, timeout time.Duration) (*remote, error) {
	deadline := time.Now().Add(timeout)
	for {
		r, err := dial(sock)
		if err == nil || time.Now().After(deadline) {
			return r, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (r *remote) call(req request) (*response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.enc.Encode(&req); err != nil {
		return nil, fmt.Errorf("%w: %v", errOwnerGone, err)
	}
	var res response
	if err := r.dec.Decode(&res); err != nil {
		return nil, fmt.Errorf("%w: %v", errOwnerGone, err)
	}
	return &res, decodeError(res.Code, res.Err)
}

func (r *remote) Name() string { return r.name }

func (r *remote) Stat(path string) (*File, error) {
	res, err := r.call(request{Op: "stat", Path: path})
	if err != nil {
		return nil, err
	}
	return res.File, nil
}

func (r *remote) List(path string) ([]*File, error) {
	res, err := r.call(request{Op: "list", Path: path})
	if err != nil {
		return nil, err
	}
	return res.Files, nil
}

func (r *remote) Read(path string) ([]byte, error) {
	res, err := r.call(request{Op: "read", Path: path})
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}

func (r *remote) Write(path string, data []byte) error {
	_, err := r.call(request{Op: "write", Path: path, Data: data})
	return err
}

func (r *remote) Mkdir(path string) error {
	_, err := r.call(request{Op: "mkdir", Path: path})
	return err
}

func (r *remote) Delete(path string, recursive bool) error {
	_, err := r.call(request{Op: "delete", Path: path, Recursive: recursive})
	return err
}

func (r *remote) Rename(oldpath, newpath string) error {
	_, err := r.call(request{Op: "rename", Path: oldpath, Path2: newpath})
	return err
}

func (r *remote) Md5(path string) (string, error) {
	res, err := r.call(request{Op: "md5", Path: path})
	if err != nil {
		return "", err
	}
	return res.Sum, nil
}

func (r *remote) Alert() error {
	_, err := r.call(request{Op: "alert"})
	return err
}

func (r *remote) Close() error {
	return r.c.Close()
}
