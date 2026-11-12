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

// Package common holds what the android importer and exporter share:
// configuration, device selection, shared storage discovery and the pool
// of sync sessions.
package common

import (
	"fmt"
	"net"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/poolpOrg/plakar-integrations/android/adb"
)

// Config is the parsed configuration of a connector.
type Config struct {
	// Serial pins the device; empty means "the only connected device".
	Serial string
	// Path is the device path from the location or the *_path option;
	// empty means the device's shared storage.
	Path string
	// ServerAddr is the adb server host:port.
	ServerAddr string
	// StartServer allows running "adb start-server" when no server answers.
	StartServer bool
	// Streams is the number of sync sessions used in parallel.
	Streams int
}

const (
	defaultStreams = 4
	maxStreams     = 16
)

// ParseLocation splits "android://[serial][/path]" (already normalised by
// kloset to start with name + "://").
func ParseLocation(name, location string) (serial, devicePath string, err error) {
	rest, ok := strings.CutPrefix(location, name+"://")
	if !ok {
		return "", "", fmt.Errorf("invalid location %q: expected %s://[serial][/path]", location, name)
	}
	if rest == "" {
		return "", "", nil
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		serial, devicePath = rest[:i], rest[i:]
	} else {
		serial = rest
	}
	if serial != "" {
		if err := adb.ValidateSerial(serial); err != nil {
			return "", "", err
		}
	}
	if devicePath != "" {
		devicePath, err = CleanDevicePath(devicePath)
		if err != nil {
			return "", "", err
		}
	}
	return serial, devicePath, nil
}

// CleanDevicePath validates an absolute device path.
func CleanDevicePath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("device path %q is not absolute", p)
	}
	if strings.IndexByte(p, 0) >= 0 {
		return "", fmt.Errorf("device path contains a NUL byte")
	}
	clean := path.Clean(p)
	if len(clean) > adb.MaxPathLength {
		return "", adb.ErrNameTooLong
	}
	return clean, nil
}

// ParseConfig builds a Config from the connector configuration.  pathKey
// names the option that may carry the device path ("source_path" or
// "destination_path"); it takes precedence over the location's path.
func ParseConfig(name string, config map[string]string, pathKey string) (*Config, error) {
	serial, devicePath, err := ParseLocation(name, config["location"])
	if err != nil {
		return nil, err
	}

	if v := config["serial"]; v != "" {
		if err := adb.ValidateSerial(v); err != nil {
			return nil, err
		}
		if serial != "" && serial != v {
			return nil, fmt.Errorf("conflicting device serials %q (location) and %q (serial option)", serial, v)
		}
		serial = v
	}

	if v := config[pathKey]; v != "" {
		p, err := CleanDevicePath(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", pathKey, err)
		}
		devicePath = p
	}

	cfg := &Config{
		Serial:      serial,
		Path:        devicePath,
		ServerAddr:  DefaultServerAddr(),
		StartServer: true,
		Streams:     defaultStreams,
	}

	if v := config["adb_server"]; v != "" {
		if _, _, err := net.SplitHostPort(v); err != nil {
			return nil, fmt.Errorf("adb_server: expected host:port, got %q", v)
		}
		cfg.ServerAddr = v
	}

	if v, ok := config["start_server"]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("start_server: %w", err)
		}
		cfg.StartServer = b
	}

	if v, ok := config["streams"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxStreams {
			return nil, fmt.Errorf("streams: expected an integer between 1 and %d, got %q", maxStreams, v)
		}
		cfg.Streams = n
	}

	return cfg, nil
}

// DefaultServerAddr honours the adb tool's own environment variables so
// that plakar and adb agree on which server to talk to.
func DefaultServerAddr() string {
	host := "127.0.0.1"
	port := "5037"
	if v := os.Getenv("ANDROID_ADB_SERVER_ADDRESS"); v != "" {
		host = v
	}
	if v := os.Getenv("ANDROID_ADB_SERVER_PORT"); v != "" {
		if _, err := strconv.Atoi(v); err == nil {
			port = v
		}
	}
	return net.JoinHostPort(host, port)
}
