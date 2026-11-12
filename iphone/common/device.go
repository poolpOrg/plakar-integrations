/*
 * Copyright (c) 2026 Gilles Chehade <gilles@plakar.io>
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

package common

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// A UDID is either the 40 hex digit form of older devices or the
// 8-16 hex digit form introduced with the 2018 devices.
var udidRe = regexp.MustCompile(`^(?:[0-9A-Fa-f]{40}|[0-9A-Fa-f]{8}-[0-9A-Fa-f]{16})$`)

// ValidateUDID rejects anything that does not look like a device identifier,
// so that a UDID can be used as a directory name and a command argument.
func ValidateUDID(udid string) error {
	if !udidRe.MatchString(udid) {
		return fmt.Errorf("invalid device identifier %q", udid)
	}
	return nil
}

// ErrNoDevice is returned when no device is connected over USB.
var ErrNoDevice = errors.New("no iPhone or iPad connected over USB")

// MultipleDevicesError is returned when several devices are connected and
// none was selected.
type MultipleDevicesError struct {
	UDIDs []string
}

func (e *MultipleDevicesError) Error() string {
	return fmt.Sprintf("several devices are connected, select one with iphone://<udid>: %s", strings.Join(e.UDIDs, ", "))
}

// DeviceNotFoundError is returned when the selected device is not connected.
type DeviceNotFoundError struct {
	UDID      string
	Connected []string
}

func (e *DeviceNotFoundError) Error() string {
	if len(e.Connected) == 0 {
		return fmt.Sprintf("device %s is not connected over USB", e.UDID)
	}
	return fmt.Sprintf("device %s is not connected over USB (connected: %s)", e.UDID, strings.Join(e.Connected, ", "))
}

// PairingError is returned when the host is not trusted by the device. Message
// is what idevicepair printed, which names the exact cause: trust dialog not
// accepted, passcode set, pairing denied.
type PairingError struct {
	UDID    string
	Message string
}

func (e *PairingError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("device %s is not paired with this host", e.UDID)
	}
	return fmt.Sprintf("device %s is not paired with this host: %s", e.UDID, e.Message)
}

// ListDevices returns the UDIDs of the devices connected over USB, in the order
// idevice_id reports them.
func (t *Tools) ListDevices(ctx context.Context) ([]string, error) {
	args := []string{"-l"}
	res, err := t.invoke(ctx, IdeviceIDTool, args, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := check(IdeviceIDTool, args, res); err != nil {
		return nil, err
	}

	var udids []string
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if err := ValidateUDID(line); err != nil {
			return nil, fmt.Errorf("%s: unexpected output line %q", IdeviceIDTool, line)
		}
		udids = append(udids, line)
	}
	return udids, nil
}

// SelectDevice picks the device to work with. With an empty want, exactly one
// device must be connected; otherwise want must be among the connected ones.
func SelectDevice(connected []string, want string) (string, error) {
	if want == "" {
		switch len(connected) {
		case 0:
			return "", ErrNoDevice
		case 1:
			return connected[0], nil
		default:
			return "", &MultipleDevicesError{UDIDs: connected}
		}
	}
	for _, udid := range connected {
		if strings.EqualFold(udid, want) {
			return udid, nil
		}
	}
	return "", &DeviceNotFoundError{UDID: want, Connected: connected}
}

// ValidatePairing checks that the device trusts this host.
func (t *Tools) ValidatePairing(ctx context.Context, udid string) error {
	args := []string{"-u", udid, "validate"}
	res, err := t.invoke(ctx, IdevicePairTool, args, nil, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return &PairingError{UDID: udid, Message: lastLines(res.Output, 1)}
	}
	return nil
}

// Pair asks the device to trust this host. On a device with a passcode this
// makes the Trust dialog appear and returns before the user answers it.
func (t *Tools) Pair(ctx context.Context, udid string) error {
	args := []string{"-u", udid, "pair"}
	res, err := t.invoke(ctx, IdevicePairTool, args, nil, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return &PairingError{UDID: udid, Message: lastLines(res.Output, 1)}
	}
	return nil
}

// Denied reports whether a pairing error is the user answering "Don't Trust"
// on the device, which idevicepair reports as "the user denied the trust
// dialog". There is no retrying that, and every retry would prompt again.
func (e *PairingError) Denied() bool {
	return strings.Contains(e.Message, "denied")
}

// EnsurePaired validates the pairing and, when the device does not trust this
// host yet, requests a pairing and waits up to timeout for the user to accept
// the Trust dialog on the device. Progress goes to stream.
//
// While the dialog is up the device answers every pairing request with
// "response pending" and the pair record only gets written by a request made
// after the user accepted, so the wait retries the pairing itself, not just
// the validation.
func (t *Tools) EnsurePaired(ctx context.Context, udid string, timeout time.Duration, stream io.Writer) error {
	err := t.ValidatePairing(ctx, udid)
	if err == nil {
		return nil
	}
	var perr *PairingError
	if !errors.As(err, &perr) {
		return err
	}

	fmt.Fprintf(stream, "iphone: device %s does not trust this host yet, requesting pairing: accept the Trust dialog on the device\n", udid)

	interval := t.PairPollInterval
	if interval <= 0 {
		interval = DefaultPairPollInterval
	}
	deadline := time.Now().Add(timeout)
	for {
		err := t.Pair(ctx, udid)
		if err == nil {
			return t.ValidatePairing(ctx, udid)
		}
		if !errors.As(err, &perr) || perr.Denied() {
			return err
		}

		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
