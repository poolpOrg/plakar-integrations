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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"

	pb "github.com/poolpOrg/plakar-integrations/flipper/internal/pb/flipper"
	pbstorage "github.com/poolpOrg/plakar-integrations/flipper/internal/pb/storage"
	pbsystem "github.com/poolpOrg/plakar-integrations/flipper/internal/pb/system"
)

const (
	usbVID = "0483"
	usbPID = "5740"

	cliPrompt    = ">: "
	rpcCommand   = "start_rpc_session"
	handshakeMax = 5 * time.Second
	rpcTimeout   = 30 * time.Second

	// the firmware caps the payload of a storage frame
	maxChunk = 512
)

// Serial is a Conn over the Flipper's USB virtual COM port.  The device
// boots in CLI mode; the session switches it to protobuf RPC and
// switches it back on Close so that the next client finds a prompt.
type Serial struct {
	name string
	port serial.Port
	rpc  *rpc
	ln   net.Listener
	sock string

	// one command at a time, the firmware would not have it otherwise
	mu sync.Mutex
}

// Open finds the Flipper named by name (any Flipper when empty) on the
// USB bus, or uses portName when given.  The first caller in the system
// gets the port and an RPC session, later ones get a Conn relayed by
// that owner, and take over when the owner goes away.
func Open(ctx context.Context, name, portName string) (Conn, error) {
	if portName == "" {
		var err error
		portName, err = findPort(name)
		if err != nil {
			return nil, err
		}
	}

	sock, err := socketPath(portName)
	if err != nil {
		return nil, err
	}

	s := &shared{name: name, portName: portName, sock: sock}
	if err := s.connect(); err != nil {
		return nil, err
	}
	return s, nil
}

// connect dials the current owner of the port, or becomes it.
func (s *shared) connect() error {
	if r, err := dial(s.sock); err == nil {
		s.conn = r
		return nil
	}

	owner, err := openSerial(s.name, s.portName)
	if err != nil {
		var perr *serial.PortError
		if errors.As(err, &perr) && perr.Code() == serial.PortBusy {
			// the owner is still in its handshake, or it is not
			// one of ours and the error stands
			if r, rerr := dialRetry(s.sock, handshakeMax); rerr == nil {
				s.conn = r
				return nil
			}
		}
		return err
	}

	// a socket left behind by a crashed owner
	_ = os.Remove(s.sock)
	ln, err := net.Listen("unix", s.sock)
	if err != nil {
		owner.Close()
		return err
	}
	owner.ln, owner.sock = ln, s.sock
	go serve(ln, owner)

	s.conn = owner
	return nil
}

func openSerial(name, portName string) (*Serial, error) {
	port, err := serial.Open(portName, &serial.Mode{BaudRate: 230400})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", portName, err)
	}

	if err := startSession(port); err != nil {
		port.Close()
		return nil, fmt.Errorf("%s: %w", portName, err)
	}

	if err := port.SetReadTimeout(rpcTimeout); err != nil {
		port.Close()
		return nil, err
	}

	s := &Serial{port: port, rpc: newRPC(timeoutReader{port})}

	if _, err := s.rpc.call(&pb.Main{Content: &pb.Main_SystemPingRequest{SystemPingRequest: &pbsystem.PingRequest{}}}); err != nil {
		port.Close()
		return nil, fmt.Errorf("%s: ping: %w", portName, err)
	}

	info, err := s.deviceInfo()
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("%s: device info: %w", portName, err)
	}
	s.name = info["hardware_name"]
	if s.name == "" {
		s.name = strings.TrimPrefix(name, "flip_")
	}
	if name != "" && !sameName(name, s.name) {
		s.Close()
		return nil, fmt.Errorf("%s is %s, not %s", portName, s.name, name)
	}

	return s, nil
}

// sameName matches the name the user typed against what the USB
// descriptor or the firmware reports.  The descriptor says flip_Lc4str1g,
// the firmware says Lc4str1g, and macOS names the port after the
// descriptor with the interface number appended (usbmodemflip_Lc4str1g1),
// so any of these spellings is accepted.
func sameName(want, got string) bool {
	want = strings.ToLower(strings.TrimPrefix(want, "flip_"))
	got = strings.ToLower(strings.TrimPrefix(got, "flip_"))
	return want == got || want == got+"1"
}

func findPort(name string) (string, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return "", err
	}

	var found []*enumerator.PortDetails
	for _, p := range ports {
		if !p.IsUSB || !strings.EqualFold(p.VID, usbVID) || !strings.EqualFold(p.PID, usbPID) {
			continue
		}
		if name != "" && !sameName(name, p.SerialNumber) {
			continue
		}
		found = append(found, p)
	}

	switch len(found) {
	case 0:
		if name != "" {
			return "", fmt.Errorf("no Flipper Zero named %s plugged in", name)
		}
		return "", errors.New("no Flipper Zero plugged in")
	case 1:
		return found[0].Name, nil
	}

	names := make([]string, 0, len(found))
	for _, p := range found {
		names = append(names, strings.TrimPrefix(p.SerialNumber, "flip_"))
	}
	return "", fmt.Errorf("several Flipper Zero plugged in, name one: %s", strings.Join(names, ", "))
}

// startSession drives the CLI to the RPC switch.  Bytes are read one at
// a time on purpose: whatever follows the echoed command belongs to the
// protobuf stream and must stay in the port buffer.
func startSession(port serial.Port) error {
	if err := port.SetReadTimeout(200 * time.Millisecond); err != nil {
		return err
	}

	deadline := time.Now().Add(handshakeMax)
	var buf bytes.Buffer
	b := make([]byte, 1)

	// wake the CLI up and wait for its prompt
	if _, err := port.Write([]byte("\r")); err != nil {
		return err
	}
	for !bytes.HasSuffix(buf.Bytes(), []byte(cliPrompt)) {
		if time.Now().After(deadline) {
			return errors.New("no CLI prompt, is the device already in RPC mode? unplug and plug it back")
		}
		n, err := port.Read(b)
		if err != nil {
			return err
		}
		buf.Write(b[:n])
	}

	if _, err := port.Write([]byte(rpcCommand + "\r")); err != nil {
		return err
	}

	// the CLI echoes the command then a newline, then goes binary
	buf.Reset()
	for {
		if time.Now().After(deadline) {
			return errors.New("timeout waiting for the RPC session to start")
		}
		n, err := port.Read(b)
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		buf.Write(b[:n])
		if b[0] == '\n' && bytes.Contains(buf.Bytes(), []byte(rpcCommand)) {
			return nil
		}
	}
}

func (s *Serial) deviceInfo() (map[string]string, error) {
	res, err := s.rpc.call(&pb.Main{Content: &pb.Main_SystemDeviceInfoRequest{SystemDeviceInfoRequest: &pbsystem.DeviceInfoRequest{}}})
	if err != nil {
		return nil, err
	}
	info := make(map[string]string, len(res))
	for _, m := range res {
		if r, ok := m.Content.(*pb.Main_SystemDeviceInfoResponse); ok {
			info[r.SystemDeviceInfoResponse.Key] = r.SystemDeviceInfoResponse.Value
		}
	}
	return info, nil
}

func (s *Serial) Name() string { return s.name }

func fileOf(f *pbstorage.File) *File {
	return &File{
		Name:  f.Name,
		Size:  int64(f.Size),
		IsDir: f.Type == pbstorage.File_DIR,
		Md5:   f.Md5Sum,
	}
}

func (s *Serial) Stat(path string) (*File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.rpc.call(&pb.Main{Content: &pb.Main_StorageStatRequest{StorageStatRequest: &pbstorage.StatRequest{Path: path}}})
	if err != nil {
		return nil, err
	}
	r, ok := res[0].Content.(*pb.Main_StorageStatResponse)
	if !ok || r.StorageStatResponse.File == nil {
		return nil, errors.New("rpc: unexpected stat response")
	}
	return fileOf(r.StorageStatResponse.File), nil
}

func (s *Serial) List(path string) ([]*File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.rpc.call(&pb.Main{Content: &pb.Main_StorageListRequest{StorageListRequest: &pbstorage.ListRequest{Path: path, IncludeMd5: true}}})
	if err != nil {
		return nil, err
	}
	var files []*File
	for _, m := range res {
		r, ok := m.Content.(*pb.Main_StorageListResponse)
		if !ok {
			return nil, errors.New("rpc: unexpected list response")
		}
		for _, f := range r.StorageListResponse.File {
			files = append(files, fileOf(f))
		}
	}
	return files, nil
}

func (s *Serial) Read(path string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.rpc.call(&pb.Main{Content: &pb.Main_StorageReadRequest{StorageReadRequest: &pbstorage.ReadRequest{Path: path}}})
	if err != nil {
		return nil, err
	}
	var data []byte
	for _, m := range res {
		r, ok := m.Content.(*pb.Main_StorageReadResponse)
		if !ok {
			return nil, errors.New("rpc: unexpected read response")
		}
		if r.StorageReadResponse.File != nil {
			data = append(data, r.StorageReadResponse.File.Data...)
		}
	}
	return data, nil
}

func (s *Serial) Write(path string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	reqs := make([]*pb.Main, 0, len(data)/maxChunk+1)
	for off := 0; ; off += maxChunk {
		end := min(off+maxChunk, len(data))
		reqs = append(reqs, &pb.Main{Content: &pb.Main_StorageWriteRequest{StorageWriteRequest: &pbstorage.WriteRequest{
			Path: path,
			File: &pbstorage.File{Data: data[off:end]},
		}}})
		if end == len(data) {
			break
		}
	}
	_, err := s.rpc.call(reqs...)
	return err
}

func (s *Serial) Mkdir(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.rpc.call(&pb.Main{Content: &pb.Main_StorageMkdirRequest{StorageMkdirRequest: &pbstorage.MkdirRequest{Path: path}}})
	return err
}

func (s *Serial) Delete(path string, recursive bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.rpc.call(&pb.Main{Content: &pb.Main_StorageDeleteRequest{StorageDeleteRequest: &pbstorage.DeleteRequest{Path: path, Recursive: recursive}}})
	return err
}

func (s *Serial) Rename(oldpath, newpath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.rpc.call(&pb.Main{Content: &pb.Main_StorageRenameRequest{StorageRenameRequest: &pbstorage.RenameRequest{OldPath: oldpath, NewPath: newpath}}})
	return err
}

func (s *Serial) Md5(path string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.rpc.call(&pb.Main{Content: &pb.Main_StorageMd5SumRequest{StorageMd5SumRequest: &pbstorage.Md5SumRequest{Path: path}}})
	if err != nil {
		return "", err
	}
	r, ok := res[0].Content.(*pb.Main_StorageMd5SumResponse)
	if !ok {
		return "", errors.New("rpc: unexpected md5sum response")
	}
	return r.StorageMd5SumResponse.Md5Sum, nil
}

// Alert blinks and buzzes the device, the way qFlipper does when a
// transfer completes.
func (s *Serial) Alert() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.rpc.call(&pb.Main{Content: &pb.Main_SystemPlayAudiovisualAlertRequest{SystemPlayAudiovisualAlertRequest: &pbsystem.PlayAudiovisualAlertRequest{}}})
	return err
}

// Close hands the device back to its CLI.  The stop request is sent
// without waiting for an answer: closing the port drops DTR, which
// ends the session on the firmware side anyway, and waiting would hold
// the port for the whole read timeout.
func (s *Serial) Close() error {
	if s.ln != nil {
		s.ln.Close()
		os.Remove(s.sock)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.rpc.notify(&pb.Main{Content: &pb.Main_StopSession{StopSession: &pb.StopSession{}}})
	return s.port.Close()
}
