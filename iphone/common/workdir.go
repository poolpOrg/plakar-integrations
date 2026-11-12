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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config keys shared by the importer and the exporter.
const (
	ConfigDeviceID         = "device_id"
	ConfigWorkingDirectory = "working_directory"
	ConfigPairingTimeout   = "pairing_timeout"
)

// DefaultPairingTimeout is how long a run waits for the user to accept the
// Trust dialog on the device.
const DefaultPairingTimeout = 60 * time.Second

// DefaultWorkdir is where device backups are staged when the configuration
// does not say: <user cache dir>/plakar/iphone. The staging state is
// reconstructible (losing it only forces the next backup to be a full one),
// which is what a cache directory is for.
func DefaultWorkdir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve cache directory: %w", err)
	}
	return filepath.Join(cache, "plakar", "iphone"), nil
}

// ResolveWorkdir returns the working directory to use for the given
// configuration value, creating it when needed.
func ResolveWorkdir(configured string) (string, error) {
	dir := configured
	if dir == "" {
		var err error
		if dir, err = DefaultWorkdir(); err != nil {
			return "", err
		}
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ConfigWorkingDirectory, err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("%s: %w", ConfigWorkingDirectory, err)
	}
	return dir, nil
}

// ParseLocation extracts the optional UDID from an iphone:// location. The
// location arrives as "<proto>://<rest>" (kloset normalizes it); rest is
// either empty or a UDID, with an optional trailing slash.
func ParseLocation(proto, location string) (string, error) {
	rest := strings.TrimPrefix(location, proto+"://")
	rest = strings.TrimPrefix(rest, proto+":")
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		return "", nil
	}
	if err := ValidateUDID(rest); err != nil {
		return "", fmt.Errorf("%s: expected %s:// or %s://<udid>: %w", location, proto, proto, err)
	}
	return rest, nil
}

// ResolveDeviceID merges the UDID from the location with the device_id config
// key; both may be set only when they agree.
func ResolveDeviceID(fromLocation, fromConfig string) (string, error) {
	if fromConfig != "" {
		if err := ValidateUDID(fromConfig); err != nil {
			return "", fmt.Errorf("%s: %w", ConfigDeviceID, err)
		}
		if fromLocation != "" && !strings.EqualFold(fromLocation, fromConfig) {
			return "", fmt.Errorf("location names device %s but %s is %s", fromLocation, ConfigDeviceID, fromConfig)
		}
		return fromConfig, nil
	}
	return fromLocation, nil
}

// ParsePairingTimeout parses the pairing_timeout config key.
func ParsePairingTimeout(value string) (time.Duration, error) {
	if value == "" {
		return DefaultPairingTimeout, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", ConfigPairingTimeout, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: must not be negative", ConfigPairingTimeout)
	}
	return d, nil
}
