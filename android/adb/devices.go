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
	"fmt"
	"strings"
)

// Device states reported by "host:devices-l".
const (
	StateDevice        = "device"
	StateOffline       = "offline"
	StateUnauthorized  = "unauthorized"
	StateAuthorizing   = "authorizing"
	StateConnecting    = "connecting"
	StateNoPermissions = "no permissions"
	StateRecovery      = "recovery"
	StateSideload      = "sideload"
	StateBootloader    = "bootloader"
	StateRescue        = "rescue"
)

// Device is one line of "host:devices-l".
type Device struct {
	Serial      string
	State       string
	Product     string
	Model       string
	Name        string
	TransportID string
}

// Usable reports whether the device accepts services (it is online and
// authorized).
func (d Device) Usable() bool {
	return d.State == StateDevice
}

// Describe returns a short human description of the device.
func (d Device) Describe() string {
	if d.Model != "" {
		return fmt.Sprintf("%s (%s)", d.Serial, strings.ReplaceAll(d.Model, "_", " "))
	}
	return d.Serial
}

// ParseDevices parses the reply of "host:devices-l".  Each line is:
//
//	<serial padded>  <state> [product:X] [model:Y] [device:Z] [transport_id:N]
//
// The state may contain spaces ("no permissions (...)"), so every token
// that is not a known attribute belongs to the state.
func ParseDevices(out string) ([]Device, error) {
	var devices []Device
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("%w: malformed device line %q", ErrProtocol, line)
		}
		d := Device{Serial: fields[0]}
		if err := ValidateSerial(d.Serial); err != nil {
			return nil, fmt.Errorf("%w: malformed device line %q", ErrProtocol, line)
		}
		var state []string
		for _, tok := range fields[1:] {
			key, value, ok := strings.Cut(tok, ":")
			if ok {
				switch key {
				case "product":
					d.Product = value
					continue
				case "model":
					d.Model = value
					continue
				case "device":
					d.Name = value
					continue
				case "transport_id":
					d.TransportID = value
					continue
				case "usb":
					continue
				}
			}
			state = append(state, tok)
		}
		d.State = strings.Join(state, " ")
		if strings.HasPrefix(d.State, StateNoPermissions) {
			d.State = StateNoPermissions
		}
		devices = append(devices, d)
	}
	return devices, nil
}

// Features is the set of protocol features a device advertises.
type Features map[string]bool

// Feature names used by this package.
const (
	FeatureShellV2  = "shell_v2"
	FeatureStatV2   = "stat_v2"
	FeatureLsV2     = "ls_v2"
	FeatureSendRecv = "sendrecv_v2"
)

// ParseFeatures parses the comma separated reply of "host-serial:X:features".
func ParseFeatures(out string) Features {
	f := make(Features)
	for _, name := range strings.Split(strings.TrimSpace(out), ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			f[name] = true
		}
	}
	return f
}

// Has reports whether the feature is advertised.
func (f Features) Has(name string) bool {
	return f != nil && f[name]
}
