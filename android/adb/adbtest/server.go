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

// Package adbtest is a fake adb server with fake devices, for tests that
// must run without Android hardware.  It implements the host services and
// the device-side sync and shell services the android integration uses,
// with the same wire quirks as the real adbd (zero stat replies, "." and
// ".." in listings, DONE without a reason for unreadable directories).
package adbtest

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/poolpOrg/plakar-integrations/android/adb"
)

// DefaultFeatures is what a recent device advertises.
var DefaultFeatures = []string{"shell_v2", "cmd", "stat_v2", "ls_v2", "fixed_push_mkdir", "apex", "abb", "fixed_push_symlink_timestamp", "abb_exec", "remount_shell", "track_app", "sendrecv_v2", "sendrecv_v2_brotli", "sendrecv_v2_lz4", "sendrecv_v2_zstd", "sendrecv_v2_dry_run_send", "openscreen_mdns"}

// LegacyFeatures is what an Android 6 device advertises: v1 everything.
var LegacyFeatures = []string{}

// Device is a fake device attached to the fake server.
type Device struct {
	Serial      string
	State       string
	Product     string
	Model       string
	Name        string
	TransportID int
	Features    []string
	FS          *FS

	// DropRecvAfter closes the connection once this many bytes of the
	// given path were sent, simulating an unplugged cable.
	DropRecvAfter map[string]int64
	// FailSend rejects pushes to the given path with this FAIL message.
	FailSend map[string]string
	// ShellError makes every shell command fail with this message.
	ShellError string
	// DropOnList closes the connection when the given directory is
	// listed, simulating an unplugged cable during enumeration.
	DropOnList map[string]bool
}

// Counters count the device services used, per path.
type Counters struct {
	mu    sync.Mutex
	Recv  map[string]int
	Send  map[string]int
	List  map[string]int
	Shell []string
}

func (c *Counters) inc(m *map[string]int, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if *m == nil {
		*m = make(map[string]int)
	}
	(*m)[key]++
}

// RecvCount returns how many times path was pulled.
func (c *Counters) RecvCount(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Recv[path]
}

// RecvTotal returns how many pulls happened in all.
func (c *Counters) RecvTotal() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.Recv {
		n += v
	}
	return n
}

// SendCount returns how many times path was pushed.
func (c *Counters) SendCount(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Send[path]
}

// ShellCommands returns the shell commands run so far.
func (c *Counters) ShellCommands() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.Shell...)
}

// Server is a fake adb server listening on a loopback TCP port.
type Server struct {
	Addr     string
	Version  string
	Counters Counters

	mu      sync.Mutex
	devices []*Device
	ln      net.Listener
	wg      sync.WaitGroup
	closed  bool
	// RefuseHost makes every host request fail, like a server that is
	// too old or broken.
	RefuseHost bool
}

// New starts a fake server; it stops when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("adbtest: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// Listen starts a fake server on addr, outside of a test.
func Listen(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("adbtest: listen: %w", err)
	}
	s := &Server{Addr: ln.Addr().String(), Version: "0029", ln: ln}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Close stops the server and waits for its connections.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.ln.Close()
	s.wg.Wait()
}

// AddDevice attaches a device.  Missing fields get defaults: state
// "device", recent features, an empty filesystem.
func (s *Server) AddDevice(d *Device) *Device {
	if d.State == "" {
		d.State = adb.StateDevice
	}
	if d.Features == nil {
		d.Features = DefaultFeatures
	}
	if d.FS == nil {
		d.FS = NewFS()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.TransportID == 0 {
		d.TransportID = len(s.devices) + 1
	}
	s.devices = append(s.devices, d)
	return d
}

// RemoveDevice detaches the device with the given serial, as if it had
// been unplugged.  Streams already open keep running until they end.
func (s *Server) RemoveDevice(serial string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, d := range s.devices {
		if d.Serial == serial {
			s.devices = append(s.devices[:i], s.devices[i+1:]...)
			return
		}
	}
}

// SetState changes a device's state.
func (s *Server) SetState(serial, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.devices {
		if d.Serial == serial {
			d.State = state
		}
	}
}

func (s *Server) device(serial string) *Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.devices {
		if d.Serial == serial {
			return d
		}
	}
	return nil
}

func (s *Server) accept() {
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

type conn struct {
	net.Conn
	rd *bufio.Reader
}

func (c *conn) readRequest() (string, bool) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.rd, hdr[:]); err != nil {
		return "", false
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
	if err != nil {
		return "", false
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.rd, buf); err != nil {
		return "", false
	}
	return string(buf), true
}

func (c *conn) okay() {
	io.WriteString(c, "OKAY")
}

func (c *conn) fail(msg string) {
	fmt.Fprintf(c, "FAIL%04x%s", len(msg), msg)
}

func (c *conn) block(payload string) {
	fmt.Fprintf(c, "%04x%s", len(payload), payload)
}

func (s *Server) handle(nc net.Conn) {
	c := &conn{Conn: nc, rd: bufio.NewReader(nc)}
	req, ok := c.readRequest()
	if !ok {
		return
	}
	if s.RefuseHost {
		c.fail("server refuses")
		return
	}
	switch {
	case req == "host:version":
		c.okay()
		c.block(s.Version)
	case req == "host:devices-l" || req == "host:devices":
		c.okay()
		c.block(s.deviceList(req == "host:devices-l"))
	case strings.HasPrefix(req, "host-serial:"):
		rest := strings.TrimPrefix(req, "host-serial:")
		serial, sub, _ := strings.Cut(rest, ":")
		d := s.device(serial)
		if d == nil {
			c.fail(fmt.Sprintf("device '%s' not found", serial))
			return
		}
		if msg := stateFailure(d); msg != "" {
			c.fail(msg)
			return
		}
		switch sub {
		case "features":
			c.okay()
			c.block(strings.Join(d.Features, ","))
		case "get-state":
			c.okay()
			c.block(d.State)
		default:
			c.fail("unknown host service")
		}
	case strings.HasPrefix(req, "host:transport:"):
		serial := strings.TrimPrefix(req, "host:transport:")
		d := s.device(serial)
		if d == nil {
			c.fail(fmt.Sprintf("device '%s' not found", serial))
			return
		}
		if msg := stateFailure(d); msg != "" {
			c.fail(msg)
			return
		}
		c.okay()
		s.deviceService(c, d)
	default:
		c.fail(fmt.Sprintf("unknown host service '%s'", req))
	}
}

func stateFailure(d *Device) string {
	switch d.State {
	case adb.StateDevice:
		return ""
	case adb.StateUnauthorized:
		return "device unauthorized.\nThis adb server's $ADB_VENDOR_KEYS is not set\nTry 'adb kill-server' if that seems wrong.\nOtherwise check for a confirmation dialog on your device."
	case adb.StateOffline:
		return "device offline"
	default:
		return "device " + d.State
	}
}

func (s *Server) deviceList(long bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, d := range s.devices {
		if !long {
			fmt.Fprintf(&b, "%s\t%s\n", d.Serial, d.State)
			continue
		}
		fmt.Fprintf(&b, "%-22s %s", d.Serial, d.State)
		if d.State == adb.StateDevice {
			if d.Product != "" {
				fmt.Fprintf(&b, " product:%s", d.Product)
			}
			if d.Model != "" {
				fmt.Fprintf(&b, " model:%s", d.Model)
			}
			if d.Name != "" {
				fmt.Fprintf(&b, " device:%s", d.Name)
			}
		}
		fmt.Fprintf(&b, " transport_id:%d\n", d.TransportID)
	}
	return b.String()
}

func (s *Server) deviceService(c *conn, d *Device) {
	req, ok := c.readRequest()
	if !ok {
		return
	}
	features := adb.ParseFeatures(strings.Join(d.Features, ","))
	switch {
	case req == "sync:":
		c.okay()
		s.syncLoop(c, d)
	case strings.HasPrefix(req, "shell,v2,raw:"):
		if !features.Has(adb.FeatureShellV2) {
			c.fail("unknown service")
			return
		}
		c.okay()
		s.shellV2(c, d, strings.TrimPrefix(req, "shell,v2,raw:"))
	case strings.HasPrefix(req, "shell:"):
		c.okay()
		s.shellV1(c, d, strings.TrimPrefix(req, "shell:"))
	default:
		c.fail("unknown service")
	}
}

// --- sync -------------------------------------------------------------

func (c *conn) readSyncRequest() (id string, path string, ok bool) {
	var hdr [8]byte
	if _, err := io.ReadFull(c.rd, hdr[:]); err != nil {
		return "", "", false
	}
	n := binary.LittleEndian.Uint32(hdr[4:8])
	if n > adb.MaxPathLength {
		c.syncFail("path too long")
		return "", "", false
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.rd, buf); err != nil {
		return "", "", false
	}
	return string(hdr[:4]), string(buf), true
}

func (c *conn) syncMsg(id string, n uint32) {
	var hdr [8]byte
	copy(hdr[:], id)
	binary.LittleEndian.PutUint32(hdr[4:], n)
	c.Write(hdr[:])
}

func (c *conn) syncFail(msg string) {
	c.syncMsg("FAIL", uint32(len(msg)))
	io.WriteString(c, msg)
}

func statV2(id string, n *Node, e adb.Errno) []byte {
	b := make([]byte, 72)
	copy(b, id)
	binary.LittleEndian.PutUint32(b[4:], uint32(e))
	if e != 0 {
		return b
	}
	binary.LittleEndian.PutUint64(b[8:], 1)
	binary.LittleEndian.PutUint64(b[16:], n.Ino)
	binary.LittleEndian.PutUint32(b[24:], n.Mode)
	nlink := uint32(1)
	if n.children != nil {
		nlink = 2
	}
	binary.LittleEndian.PutUint32(b[28:], nlink)
	binary.LittleEndian.PutUint32(b[32:], n.UID)
	binary.LittleEndian.PutUint32(b[36:], n.GID)
	binary.LittleEndian.PutUint64(b[40:], uint64(nodeSize(n)))
	mt := uint64(n.ModTime.Unix())
	binary.LittleEndian.PutUint64(b[48:], mt)
	binary.LittleEndian.PutUint64(b[56:], mt)
	binary.LittleEndian.PutUint64(b[64:], mt)
	return b
}

func statV1(id string, n *Node) []byte {
	b := make([]byte, 16)
	copy(b, id)
	if n == nil {
		return b
	}
	binary.LittleEndian.PutUint32(b[4:], n.Mode)
	binary.LittleEndian.PutUint32(b[8:], uint32(nodeSize(n)))
	binary.LittleEndian.PutUint32(b[12:], uint32(n.ModTime.Unix()))
	return b
}

func nodeSize(n *Node) int64 {
	switch n.Mode & adb.S_IFMT {
	case adb.S_IFLNK:
		return int64(len(n.Target))
	case adb.S_IFDIR:
		return 4096
	}
	return int64(len(n.Data))
}

func (s *Server) syncLoop(c *conn, d *Device) {
	for {
		id, p, ok := c.readSyncRequest()
		if !ok {
			return
		}
		switch id {
		case "STAT":
			n, e := d.FS.Lstat(p)
			if e != 0 {
				n = nil
			}
			c.Write(statV1("STAT", n))
		case "LST2":
			n, e := d.FS.Lstat(p)
			c.Write(statV2("LST2", n, e))
		case "STA2":
			n, e := d.FS.Stat(p)
			c.Write(statV2("STA2", n, e))
		case "LIST", "LIS2":
			s.Counters.inc(&s.Counters.List, p)
			if d.DropOnList[p] {
				return
			}
			entries, _ := d.FS.list(p)
			for _, e := range entries {
				if id == "LIS2" {
					b := statV2("DNT2", e.node, e.node.StatErrno)
					var namelen [4]byte
					binary.LittleEndian.PutUint32(namelen[:], uint32(len(e.name)))
					c.Write(append(b, namelen[:]...))
				} else {
					b := statV1("DENT", e.node)
					var namelen [4]byte
					binary.LittleEndian.PutUint32(namelen[:], uint32(len(e.name)))
					c.Write(append(b, namelen[:]...))
				}
				io.WriteString(c, e.name)
			}
			c.syncMsg("DONE", 0)
		case "RECV":
			s.Counters.inc(&s.Counters.Recv, p)
			if !s.recv(c, d, p) {
				return
			}
		case "SEND":
			if !s.send(c, d, p) {
				return
			}
		case "QUIT":
			return
		default:
			c.syncFail(fmt.Sprintf("unknown command %s", id))
			return
		}
	}
}

func (s *Server) recv(c *conn, d *Device, p string) bool {
	// Like adbd, a FAIL ends the sync session: every failure path
	// returns false and the connection is closed.
	n, e := d.FS.Stat(p)
	if e != 0 {
		c.syncFail("open failed: " + e.Error())
		return false
	}
	if n.Mode&adb.S_IFMT != adb.S_IFREG {
		c.syncFail("open failed: " + adb.Errno(21).Error())
		return false
	}
	if n.ReadError != "" {
		c.syncFail("read failed: " + n.ReadError)
		return false
	}
	limit := int64(-1)
	if v, ok := d.DropRecvAfter[p]; ok {
		limit = v
	}
	d.FS.mu.Lock()
	data := append([]byte(nil), n.Data...)
	d.FS.mu.Unlock()
	var sent int64
	for len(data) > 0 {
		chunk := data
		if len(chunk) > adb.SyncDataMax {
			chunk = chunk[:adb.SyncDataMax]
		}
		if limit >= 0 && sent+int64(len(chunk)) > limit {
			chunk = chunk[:limit-sent]
			if len(chunk) > 0 {
				c.syncMsg("DATA", uint32(len(chunk)))
				c.Write(chunk)
			}
			return false // drop the connection
		}
		c.syncMsg("DATA", uint32(len(chunk)))
		c.Write(chunk)
		sent += int64(len(chunk))
		data = data[len(chunk):]
	}
	c.syncMsg("DONE", 0)
	return true
}

func (s *Server) send(c *conn, d *Device, spec string) bool {
	i := strings.LastIndexByte(spec, ',')
	if i < 0 {
		c.syncFail("missing , in ID_SEND_V1")
		return false
	}
	p := spec[:i]
	mode, err := strconv.ParseUint(spec[i+1:], 0, 32)
	if err != nil {
		c.syncFail("bad mode")
		return false
	}
	s.Counters.inc(&s.Counters.Send, p)

	var data bytes.Buffer
	var mtime uint32
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(c.rd, hdr[:]); err != nil {
			return false
		}
		n := binary.LittleEndian.Uint32(hdr[4:8])
		switch string(hdr[:4]) {
		case "DATA":
			if n > adb.SyncDataMax {
				c.syncFail("oversized data message")
				return false
			}
			if _, err := io.CopyN(&data, c.rd, int64(n)); err != nil {
				return false
			}
			continue
		case "DONE":
			mtime = n
		default:
			c.syncFail("invalid data message: expected ID_DONE")
			return false
		}
		break
	}
	if msg, ok := d.FailSend[p]; ok {
		c.syncFail(msg)
		return false
	}
	ts := time.Unix(int64(mtime), 0)
	var e adb.Errno
	if uint32(mode)&adb.S_IFMT == adb.S_IFLNK {
		e = d.FS.sendLink(p, data.String(), ts)
		if e != 0 {
			c.syncFail("symlink failed: " + e.Error())
			return false
		}
	} else {
		e = d.FS.send(p, uint32(mode), data.Bytes(), ts)
		if e != 0 {
			c.syncFail("couldn't create file: " + e.Error())
			return false
		}
	}
	c.syncMsg("OKAY", 0)
	return true
}

// --- shell ------------------------------------------------------------

var v1Template = regexp.MustCompile(`^(.*) 2>&1 </dev/null; echo "([A-Za-z0-9_]+)\$\?"$`)

func (s *Server) shellV1(c *conn, d *Device, cmd string) {
	m := v1Template.FindStringSubmatch(cmd)
	if m == nil {
		fmt.Fprintf(c, "/system/bin/sh: unsupported fake command\r\n")
		return
	}
	res := s.runShell(d, m[1])
	out := append(res.Stdout, res.Stderr...)
	out = bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
	c.Write(out)
	fmt.Fprintf(c, "%s%d\r\n", m[2], res.ExitCode)
}

func (s *Server) shellV2(c *conn, d *Device, cmd string) {
	res := s.runShell(d, cmd)
	packet := func(id byte, payload []byte) {
		var hdr [5]byte
		hdr[0] = id
		binary.LittleEndian.PutUint32(hdr[1:], uint32(len(payload)))
		c.Write(hdr[:])
		c.Write(payload)
	}
	if len(res.Stdout) > 0 {
		packet(1, res.Stdout)
	}
	if len(res.Stderr) > 0 {
		packet(2, res.Stderr)
	}
	packet(3, []byte{byte(res.ExitCode)})
}

func (s *Server) runShell(d *Device, cmd string) adb.ShellResult {
	s.Counters.mu.Lock()
	s.Counters.Shell = append(s.Counters.Shell, cmd)
	s.Counters.mu.Unlock()

	if d.ShellError != "" {
		return adb.ShellResult{Stderr: []byte(d.ShellError + "\n"), ExitCode: 1}
	}
	args, ok := tokenize(cmd)
	if !ok || len(args) == 0 {
		return adb.ShellResult{Stderr: []byte("sh: syntax error\n"), ExitCode: 2}
	}
	name, args := args[0], args[1:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	switch name {
	case "readlink":
		if len(args) != 1 {
			return adb.ShellResult{Stderr: []byte("usage: readlink FILE\n"), ExitCode: 1}
		}
		n, e := d.FS.Lstat(args[0])
		if e != 0 || n.Mode&adb.S_IFMT != adb.S_IFLNK {
			return adb.ShellResult{ExitCode: 1}
		}
		return adb.ShellResult{Stdout: []byte(n.Target + "\n")}
	case "mkdir":
		if len(args) > 0 && args[0] == "-p" {
			args = args[1:]
			if len(args) > 0 && args[0] == "--" {
				args = args[1:]
			}
		}
		for _, p := range args {
			if d.FS.MkdirAll(p, time.Now()) == nil {
				return adb.ShellResult{Stderr: []byte("mkdir: '" + p + "': Not a directory\n"), ExitCode: 1}
			}
		}
		return adb.ShellResult{}
	case "true":
		return adb.ShellResult{}
	case "echo":
		return adb.ShellResult{Stdout: []byte(strings.Join(args, " ") + "\n")}
	default:
		return adb.ShellResult{Stderr: []byte("sh: " + name + ": inaccessible or not found\n"), ExitCode: 127}
	}
}

// tokenize splits a command the way a POSIX shell would for words made of
// single-quoted strings and bare words.  Anything else is a syntax error,
// which is the point: the integration must quote everything.
func tokenize(cmd string) ([]string, bool) {
	var (
		args []string
		cur  strings.Builder
		in   bool // inside a word
	)
	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]
		switch {
		case ch == '\'':
			in = true
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				return nil, false
			}
			cur.WriteString(cmd[i+1 : i+1+j])
			i += j + 1
		case ch == '\\' && i+1 < len(cmd):
			in = true
			cur.WriteByte(cmd[i+1])
			i++
		case ch == ' ' || ch == '\t':
			if in {
				args = append(args, cur.String())
				cur.Reset()
				in = false
			}
		case strings.ContainsRune("|&;<>()$`\"*?[]#~=%{}", rune(ch)):
			return nil, false
		default:
			in = true
			cur.WriteByte(ch)
		}
	}
	if in {
		args = append(args, cur.String())
	}
	return args, true
}
