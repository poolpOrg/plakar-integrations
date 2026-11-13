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
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	pb "github.com/poolpOrg/plakar-integrations/flipper/internal/pb/flipper"
)

// rpc speaks the Flipper protobuf RPC: varint length-prefixed Main
// messages in both directions, requests and responses paired by
// command_id, multi-part transfers chained with has_next.  The firmware
// serves one command at a time, so the client is synchronous and the
// caller serializes access.
type rpc struct {
	rd *bufio.Reader
	wr *bufio.Writer
	id uint32
}

func newRPC(rw io.ReadWriter) *rpc {
	return &rpc{
		rd: bufio.NewReaderSize(rw, 64<<10),
		wr: bufio.NewWriterSize(rw, 64<<10),
	}
}

// StatusError is a command the device refused.
type StatusError struct {
	Status pb.CommandStatus
}

func (e *StatusError) Error() string {
	return "flipper: " + e.Status.String()
}

func statusError(status pb.CommandStatus) error {
	switch status {
	case pb.CommandStatus_ERROR_STORAGE_NOT_EXIST:
		return ErrNotExist
	case pb.CommandStatus_ERROR_STORAGE_EXIST:
		return ErrExist
	}
	return &StatusError{Status: status}
}

// call sends the frames of one command and collects the responses
// until the device clears has_next.
func (r *rpc) call(reqs ...*pb.Main) ([]*pb.Main, error) {
	r.id++
	if r.id == 0 {
		r.id = 1
	}

	for i, req := range reqs {
		req.CommandId = r.id
		req.HasNext = i < len(reqs)-1
		if err := r.send(req); err != nil {
			return nil, err
		}
	}
	if err := r.wr.Flush(); err != nil {
		return nil, err
	}

	var res []*pb.Main
	for {
		m, err := r.recv()
		if err != nil {
			return nil, err
		}
		// command_id 0 is unsolicited (screen frames), anything
		// else that is not ours is a leftover from a dead call
		if m.CommandId != r.id {
			continue
		}
		if m.CommandStatus != pb.CommandStatus_OK {
			return nil, statusError(m.CommandStatus)
		}
		res = append(res, m)
		if !m.HasNext {
			return res, nil
		}
	}
}

// notify sends a command without waiting for its answer.
func (r *rpc) notify(m *pb.Main) error {
	r.id++
	m.CommandId = r.id
	if err := r.send(m); err != nil {
		return err
	}
	return r.wr.Flush()
}

func (r *rpc) send(m *pb.Main) error {
	data, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	frame := protowire.AppendVarint(nil, uint64(len(data)))
	if _, err := r.wr.Write(frame); err != nil {
		return err
	}
	_, err = r.wr.Write(data)
	return err
}

func (r *rpc) recv() (*pb.Main, error) {
	n, err := binary.ReadUvarint(r.rd)
	if err != nil {
		return nil, fmt.Errorf("rpc: %w", err)
	}
	if n > 1<<20 {
		return nil, fmt.Errorf("rpc: frame of %d bytes, stream is out of sync", n)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r.rd, data); err != nil {
		return nil, fmt.Errorf("rpc: %w", err)
	}
	var m pb.Main
	if err := proto.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("rpc: %w", err)
	}
	return &m, nil
}

var errReadTimeout = errors.New("timeout waiting for the device")

// timeoutReader turns the serial port's silent zero-length read on
// timeout into an error, bufio would otherwise spin on it.
type timeoutReader struct {
	io.ReadWriter
}

func (t timeoutReader) Read(p []byte) (int, error) {
	n, err := t.ReadWriter.Read(p)
	if n == 0 && err == nil {
		return 0, errReadTimeout
	}
	return n, err
}
