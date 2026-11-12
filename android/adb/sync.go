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

package adb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"time"
)

// SYNC protocol constants, from adb's file_sync_protocol.h.
const (
	// SyncDataMax is the largest DATA payload either side may send.
	SyncDataMax = 64 * 1024
	// MaxPathLength is the longest path a sync request may carry.
	MaxPathLength = 1024
	// maxNameLength bounds a directory entry name (Linux NAME_MAX).
	maxNameLength = 255
)

const (
	idLstatV1 = "STAT"
	idStatV2  = "STA2"
	idLstatV2 = "LST2"
	idListV1  = "LIST"
	idListV2  = "LIS2"
	idDentV1  = "DENT"
	idDentV2  = "DNT2"
	idSendV1  = "SEND"
	idRecvV1  = "RECV"
	idDone    = "DONE"
	idData    = "DATA"
	idOkay    = "OKAY"
	idFail    = "FAIL"
	idQuit    = "QUIT"
)

// ErrNameTooLong is returned for device paths longer than MaxPathLength.
var ErrNameTooLong = errors.New("adb: path too long")

// ErrBroken is returned by a Sync whose stream can no longer be trusted
// (a transfer was abandoned half way or a protocol error occurred).
var ErrBroken = errors.New("adb: sync connection is broken")

// Stat is the metadata of a device path.
type Stat struct {
	Mode    uint32 // Linux st_mode
	Size    int64
	ModTime time.Time
	Dev     uint64
	Ino     uint64
	Nlink   uint32
	UID     uint32
	GID     uint32
}

// FileMode is the Go view of Mode.
func (st Stat) FileMode() fs.FileMode { return FileModeFromUnix(st.Mode) }

// IsDir reports whether the path is a directory.
func (st Stat) IsDir() bool { return st.Mode&S_IFMT == S_IFDIR }

// IsRegular reports whether the path is a regular file.
func (st Stat) IsRegular() bool { return st.Mode&S_IFMT == S_IFREG }

// IsSymlink reports whether the path is a symbolic link.
func (st Stat) IsSymlink() bool { return st.Mode&S_IFMT == S_IFLNK }

// Dirent is one entry of a directory listing.  Err is set when the device
// could not stat the entry (v2 listings only).
type Dirent struct {
	Name string
	Stat Stat
	Err  error
}

// Sync is a device "sync:" session.  It is not safe for concurrent use;
// callers that want parallel transfers open several sessions.
type Sync struct {
	conn   *Conn
	stat2  bool
	ls2    bool
	v1Size bool
	broken bool
	hdr    [76]byte
}

// Sync opens a sync session with the device.  The features decide whether
// the v2 (64-bit sizes, errno, ownership) stat and list requests are used.
func (c *Client) Sync(ctx context.Context, serial string, features Features) (*Sync, error) {
	conn, err := c.Transport(ctx, serial, "sync:")
	if err != nil {
		return nil, err
	}
	return &Sync{
		conn:   conn,
		stat2:  features.Has(FeatureStatV2),
		ls2:    features.Has(FeatureLsV2),
		v1Size: !features.Has(FeatureStatV2),
	}, nil
}

// V1Metadata reports whether the device only offers the v1 protocol, whose
// sizes are 32-bit and whose stat replies carry no error.
func (s *Sync) V1Metadata() bool { return s.v1Size }

// Broken reports whether the session must be discarded.
func (s *Sync) Broken() bool { return s.broken }

// Close ends the session.  A healthy session says goodbye first.
func (s *Sync) Close() error {
	if !s.broken {
		_ = s.request(idQuit, "")
	}
	return s.conn.Close()
}

func (s *Sync) fail(err error) error {
	s.broken = true
	return err
}

func checkPath(path string) error {
	if len(path) > MaxPathLength {
		return ErrNameTooLong
	}
	if strings.IndexByte(path, 0) >= 0 {
		return fmt.Errorf("adb: path contains a NUL byte")
	}
	return nil
}

// request writes a SyncRequest: id, little-endian length, path.
func (s *Sync) request(id, path string) error {
	if err := checkPath(path); err != nil {
		return err
	}
	buf := make([]byte, 8+len(path))
	copy(buf, id)
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(path)))
	copy(buf[8:], path)
	if err := s.conn.write(buf); err != nil {
		return s.fail(err)
	}
	return nil
}

func (s *Sync) readID() (string, error) {
	if err := s.conn.readFull(s.hdr[:4]); err != nil {
		return "", s.fail(err)
	}
	return string(s.hdr[:4]), nil
}

// readStatusMessage reads the body of an OKAY/FAIL/DONE message once its
// id has been consumed and returns the FAIL text.
func (s *Sync) readStatusMessage(id string) (string, error) {
	if err := s.conn.readFull(s.hdr[4:8]); err != nil {
		return "", s.fail(err)
	}
	n := binary.LittleEndian.Uint32(s.hdr[4:8])
	if id != idFail {
		return "", nil
	}
	if n > SyncDataMax {
		return "", s.fail(fmt.Errorf("%w: oversized FAIL message", ErrProtocol))
	}
	msg := make([]byte, n)
	if err := s.conn.readFull(msg); err != nil {
		return "", s.fail(err)
	}
	return string(msg), nil
}

func (s *Sync) decodeStatV2(b []byte) (Stat, Errno) {
	// id(4) error(4) dev(8) ino(8) mode(4) nlink(4) uid(4) gid(4)
	// size(8) atime(8) mtime(8) ctime(8)
	e := Errno(binary.LittleEndian.Uint32(b[4:8]))
	st := Stat{
		Dev:   binary.LittleEndian.Uint64(b[8:16]),
		Ino:   binary.LittleEndian.Uint64(b[16:24]),
		Mode:  binary.LittleEndian.Uint32(b[24:28]),
		Nlink: binary.LittleEndian.Uint32(b[28:32]),
		UID:   binary.LittleEndian.Uint32(b[32:36]),
		GID:   binary.LittleEndian.Uint32(b[36:40]),
		Size:  int64(binary.LittleEndian.Uint64(b[40:48])),
	}
	mtime := int64(binary.LittleEndian.Uint64(b[56:64]))
	st.ModTime = time.Unix(mtime, 0)
	if st.Size < 0 {
		st.Size = 0
	}
	return st, e
}

func (s *Sync) decodeStatV1(b []byte) Stat {
	// id(4) mode(4) size(4) mtime(4)
	return Stat{
		Mode:    binary.LittleEndian.Uint32(b[4:8]),
		Size:    int64(binary.LittleEndian.Uint32(b[8:12])),
		ModTime: time.Unix(int64(binary.LittleEndian.Uint32(b[12:16])), 0),
	}
}

// Lstat returns the metadata of path without following a final symlink.
func (s *Sync) Lstat(path string) (Stat, error) {
	return s.stat(idLstatV2, path)
}

// Stat returns the metadata of path, following symlinks.  It needs the
// stat_v2 feature; older devices get ErrUnsupported.
func (s *Sync) Stat(path string) (Stat, error) {
	if !s.stat2 {
		return Stat{}, fmt.Errorf("adb: stat: %w", errors.ErrUnsupported)
	}
	return s.stat(idStatV2, path)
}

func (s *Sync) stat(id, path string) (Stat, error) {
	if s.broken {
		return Stat{}, ErrBroken
	}
	if !s.stat2 {
		id = idLstatV1
	}
	if err := s.request(id, path); err != nil {
		return Stat{}, err
	}
	got, err := s.readID()
	if err != nil {
		return Stat{}, err
	}
	if got != id {
		if got == idFail {
			msg, err := s.readStatusMessage(got)
			if err != nil {
				return Stat{}, err
			}
			return Stat{}, s.fail(&Error{Service: "sync:" + id, Message: msg})
		}
		return Stat{}, s.fail(fmt.Errorf("%w: expected %s, got %q", ErrProtocol, id, got))
	}
	if id == idLstatV1 {
		if err := s.conn.readFull(s.hdr[4:16]); err != nil {
			return Stat{}, s.fail(err)
		}
		st := s.decodeStatV1(s.hdr[:16])
		if st.Mode == 0 && st.Size == 0 && st.ModTime.Unix() == 0 {
			// v1 has no error channel: an all-zero reply is a failure.
			return Stat{}, Errno(2)
		}
		return st, nil
	}
	if err := s.conn.readFull(s.hdr[4:72]); err != nil {
		return Stat{}, s.fail(err)
	}
	st, e := s.decodeStatV2(s.hdr[:72])
	if e != 0 {
		return Stat{}, e
	}
	return st, nil
}

// List calls fn for every entry of the directory, excluding "." and "..".
// Entries are reported with their lstat metadata, so symlinks are not
// followed.  An unreadable or non-existent directory yields no entry at
// all and no error: the protocol does not say why.  Readable lists
// reports whether the device sent the "." entry, which only a successfully
// opened directory produces.
//
// If fn returns an error the listing is drained and the error returned, so
// the session stays usable.
func (s *Sync) List(path string, fn func(Dirent) error) (readable bool, err error) {
	if s.broken {
		return false, ErrBroken
	}
	id, dent, hdrlen := idListV2, idDentV2, 76
	if !s.ls2 {
		id, dent, hdrlen = idListV1, idDentV1, 20
	}
	if err := s.request(id, path); err != nil {
		return false, err
	}
	var fnErr error
	for {
		got, err := s.readID()
		if err != nil {
			return readable, err
		}
		switch got {
		case idDone:
			if _, err := s.readStatusMessage(got); err != nil {
				return readable, err
			}
			return readable, fnErr
		case idFail:
			msg, err := s.readStatusMessage(got)
			if err != nil {
				return readable, err
			}
			return readable, s.fail(&Error{Service: "sync:" + id, Message: msg})
		case dent:
		default:
			return readable, s.fail(fmt.Errorf("%w: unexpected %q in listing", ErrProtocol, got))
		}
		if err := s.conn.readFull(s.hdr[4:hdrlen]); err != nil {
			return readable, s.fail(err)
		}
		var (
			entry   Dirent
			namelen uint32
		)
		if dent == idDentV2 {
			st, e := s.decodeStatV2(s.hdr[:72])
			namelen = binary.LittleEndian.Uint32(s.hdr[72:76])
			entry.Stat = st
			if e != 0 {
				entry.Err = e
			}
		} else {
			entry.Stat = s.decodeStatV1(s.hdr[:16])
			namelen = binary.LittleEndian.Uint32(s.hdr[16:20])
		}
		if namelen > MaxPathLength {
			return readable, s.fail(fmt.Errorf("%w: oversized directory entry name", ErrProtocol))
		}
		name := make([]byte, namelen)
		if err := s.conn.readFull(name); err != nil {
			return readable, s.fail(err)
		}
		entry.Name = string(name)
		switch entry.Name {
		case ".":
			readable = true
			continue
		case "..":
			continue
		}
		if fnErr != nil {
			continue
		}
		if err := validateName(entry.Name); err != nil {
			entry.Err = err
		}
		if err := fn(entry); err != nil {
			fnErr = err
		}
	}
}

// validateName rejects entry names the device should never produce.  A
// name with a slash would let the device escape the directory it was asked
// to list.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty directory entry name", ErrProtocol)
	}
	if len(name) > maxNameLength {
		return fmt.Errorf("%w: directory entry name too long", ErrProtocol)
	}
	if strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("%w: invalid directory entry name %q", ErrProtocol, name)
	}
	return nil
}

// Recv starts reading the file at path.  The returned reader streams the
// DATA chunks as the device sends them; it must be read to EOF before the
// session is used again, otherwise the session is marked broken when the
// reader is closed.
func (s *Sync) Recv(path string) (*RecvReader, error) {
	if s.broken {
		return nil, ErrBroken
	}
	if err := s.request(idRecvV1, path); err != nil {
		return nil, err
	}
	return &RecvReader{s: s}, nil
}

// RecvReader streams one file from the device.
type RecvReader struct {
	s         *Sync
	remaining int
	eof       bool
	err       error
	n         int64
}

// Read implements io.Reader.
func (r *RecvReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.eof {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		if err := r.nextChunk(); err != nil {
			r.err = err
			return 0, err
		}
		if r.eof {
			return 0, io.EOF
		}
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.s.conn.rd.Read(p)
	r.remaining -= n
	r.n += int64(n)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		r.err = r.s.fail(fmt.Errorf("adb: read: %w", err))
		return n, r.err
	}
	return n, nil
}

func (r *RecvReader) nextChunk() error {
	s := r.s
	id, err := s.readID()
	if err != nil {
		return err
	}
	switch id {
	case idData:
		if err := s.conn.readFull(s.hdr[4:8]); err != nil {
			return s.fail(err)
		}
		n := binary.LittleEndian.Uint32(s.hdr[4:8])
		if n > SyncDataMax {
			return s.fail(fmt.Errorf("%w: oversized DATA chunk", ErrProtocol))
		}
		r.remaining = int(n)
		if n == 0 {
			// Not produced by adbd, tolerated: just look for the next chunk.
			return r.nextChunk()
		}
		return nil
	case idDone:
		if _, err := s.readStatusMessage(id); err != nil {
			return err
		}
		r.eof = true
		return nil
	case idFail:
		msg, err := s.readStatusMessage(id)
		if err != nil {
			return err
		}
		// adbd closes the sync session after any FAIL: this reader
		// is done and the session cannot be reused.
		r.eof = true
		s.broken = true
		return &Error{Service: "sync:RECV", Message: msg}
	default:
		return s.fail(fmt.Errorf("%w: unexpected %q while receiving", ErrProtocol, id))
	}
}

// Bytes returns how many bytes of content were read so far.
func (r *RecvReader) Bytes() int64 { return r.n }

// Complete reports whether the device said DONE.
func (r *RecvReader) Complete() bool { return r.eof && r.err == nil }

// Close abandons the transfer.  A transfer that did not reach DONE leaves
// unread bytes on the wire, so the session is marked broken.
func (r *RecvReader) Close() error {
	if !r.eof {
		r.s.broken = true
	}
	return nil
}

// Send writes the content of r to path on the device, creating missing
// parent directories, and sets its mode (permission bits) and mtime.  An
// existing regular file at path is replaced.  The transfer either ends
// with the device's OKAY or with an error; nothing is retried.
func (s *Sync) Send(path string, mode uint32, mtime time.Time, r io.Reader) error {
	if s.broken {
		return ErrBroken
	}
	spec := path + "," + strconv.FormatUint(uint64(mode), 10)
	if err := s.request(idSendV1, spec); err != nil {
		return err
	}
	buf := make([]byte, 8+SyncDataMax)
	copy(buf, idData)
	for {
		n, rerr := io.ReadFull(r, buf[8:])
		if n > 0 {
			binary.LittleEndian.PutUint32(buf[4:8], uint32(n))
			if err := s.conn.write(buf[:8+n]); err != nil {
				return s.fail(s.failReason(err))
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				break
			}
			// The device is waiting for more data; there is no way to
			// abort cleanly, so drop the session.
			return s.fail(fmt.Errorf("adb: send %s: %w", path, rerr))
		}
	}
	var ts uint32
	if sec := mtime.Unix(); sec > 0 && sec <= int64(^uint32(0)) {
		ts = uint32(sec)
	}
	copy(buf, idDone)
	binary.LittleEndian.PutUint32(buf[4:8], ts)
	if err := s.conn.write(buf[:8]); err != nil {
		return s.fail(s.failReason(err))
	}
	id, err := s.readID()
	if err != nil {
		return err
	}
	switch id {
	case idOkay:
		_, err := s.readStatusMessage(id)
		return err
	case idFail:
		msg, err := s.readStatusMessage(id)
		if err != nil {
			return err
		}
		// adbd closes the sync session after any FAIL.
		return s.fail(&Error{Service: "sync:SEND", Message: msg})
	default:
		return s.fail(fmt.Errorf("%w: unexpected %q after send", ErrProtocol, id))
	}
}

// failReason tries to replace a write error (the device closed the stream)
// with the FAIL message the device sent before closing, when there is one.
func (s *Sync) failReason(werr error) error {
	_ = s.conn.nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer s.conn.nc.SetReadDeadline(time.Time{})
	var id [4]byte
	if _, err := io.ReadFull(s.conn.rd, id[:]); err != nil || string(id[:]) != idFail {
		return werr
	}
	var lenb [4]byte
	if _, err := io.ReadFull(s.conn.rd, lenb[:]); err != nil {
		return werr
	}
	n := binary.LittleEndian.Uint32(lenb[:])
	if n > SyncDataMax {
		return werr
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(s.conn.rd, msg); err != nil {
		return werr
	}
	return &Error{Service: "sync:SEND", Message: string(msg)}
}
