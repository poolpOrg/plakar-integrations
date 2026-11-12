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

package common

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/poolpOrg/plakar-integrations/android/adb"
)

// Device is a connected, authorized device pinned by its serial.
type Device struct {
	Client   *adb.Client
	Serial   string
	Info     adb.Device
	Features adb.Features
}

// ErrNoDevice means no device is connected at all.
var ErrNoDevice = errors.New("no Android device connected")

const enableDebuggingHint = "enable USB debugging on the device (Settings > System > Developer options > USB debugging), plug it in and accept the \"Allow USB debugging?\" prompt"

// Connect reaches the adb server, selects the device and reads its
// features.  It never picks a device at random: with several devices and
// no serial, it fails and lists them.
func Connect(ctx context.Context, cfg *Config) (*Device, error) {
	client := adb.NewClient(cfg.ServerAddr)

	devices, err := client.Devices(ctx)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, adb.ErrProtocol) {
			// Something answered, just not an adb server: starting
			// another one would not help.
			return nil, err
		}
		if !cfg.StartServer {
			return nil, fmt.Errorf("%w (run \"adb start-server\" or set start_server=true)", err)
		}
		if serr := StartServer(ctx, cfg.ServerAddr); serr != nil {
			return nil, fmt.Errorf("%w; starting it failed: %w", err, serr)
		}
		devices, err = client.Devices(ctx)
		if err != nil {
			return nil, err
		}
	}

	info, err := SelectDevice(devices, cfg.Serial)
	if err != nil {
		return nil, err
	}

	features, err := client.Features(ctx, info.Serial)
	if err != nil {
		return nil, fmt.Errorf("cannot query device %s: %w", info.Serial, err)
	}

	return &Device{
		Client:   client,
		Serial:   info.Serial,
		Info:     info,
		Features: features,
	}, nil
}

// SelectDevice picks the device with the given serial, or the only
// connected device when serial is empty.
func SelectDevice(devices []adb.Device, serial string) (adb.Device, error) {
	if serial != "" {
		for _, d := range devices {
			if d.Serial == serial {
				return d, stateError(d)
			}
		}
		if len(devices) == 0 {
			return adb.Device{}, fmt.Errorf("device %s not found: %w; %s", serial, ErrNoDevice, enableDebuggingHint)
		}
		return adb.Device{}, fmt.Errorf("device %s not found; connected devices: %s", serial, describeAll(devices))
	}

	switch len(devices) {
	case 0:
		return adb.Device{}, fmt.Errorf("%w: %s", ErrNoDevice, enableDebuggingHint)
	case 1:
		return devices[0], stateError(devices[0])
	default:
		return adb.Device{}, fmt.Errorf("%d devices connected, specify one with android://<serial>: %s", len(devices), describeAll(devices))
	}
}

func describeAll(devices []adb.Device) string {
	parts := make([]string, len(devices))
	for i, d := range devices {
		parts[i] = d.Describe() + " [" + d.State + "]"
	}
	return strings.Join(parts, ", ")
}

// stateError explains why a device cannot be used, with the fix.
func stateError(d adb.Device) error {
	switch d.State {
	case adb.StateDevice:
		return nil
	case adb.StateUnauthorized:
		return fmt.Errorf("device %s is not authorized: unlock it and accept the \"Allow USB debugging?\" prompt (tick \"Always allow from this computer\")", d.Serial)
	case adb.StateAuthorizing, adb.StateConnecting:
		return fmt.Errorf("device %s is still connecting (%s), retry in a moment", d.Serial, d.State)
	case adb.StateOffline:
		return fmt.Errorf("device %s is offline: unplug and replug it, or revoke USB debugging authorizations on the device and authorize again", d.Serial)
	case adb.StateNoPermissions:
		return fmt.Errorf("device %s cannot be opened by the adb server (no permissions): on Linux add a udev rule for the device or run the adb server as a user allowed to access it", d.Serial)
	case adb.StateRecovery, adb.StateSideload, adb.StateBootloader, adb.StateRescue:
		return fmt.Errorf("device %s is in %s mode, boot it to Android first", d.Serial, d.State)
	default:
		return fmt.Errorf("device %s is in state %q and cannot be used", d.Serial, d.State)
	}
}

// Ping checks that the device is still connected and usable.
func (d *Device) Ping(ctx context.Context) error {
	devices, err := d.Client.Devices(ctx)
	if err != nil {
		return err
	}
	for _, dev := range devices {
		if dev.Serial == d.Serial {
			return stateError(dev)
		}
	}
	return fmt.Errorf("device %s disconnected", d.Serial)
}

// StartServer runs "adb start-server" for a server expected on addr.  It
// only does so for a loopback address: a remote server is someone else's
// to start.
func StartServer(ctx context.Context, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("adb server %s is not local", addr)
	}

	bin, err := findADB()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-L", "tcp:"+net.JoinHostPort(host, port), "start-server")
	cmd.Stdout = nil
	var stderr strings.Builder
	cmd.Stderr = &stderr
	// start-server forks the daemon, which may inherit the stderr pipe:
	// do not wait for it to close after the command itself has exited.
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("%s start-server: %w: %s", bin, err, msg)
		}
		return fmt.Errorf("%s start-server: %w", bin, err)
	}
	return nil
}

// findADB locates the adb executable: $PLAKAR_ADB, then $PATH, then the
// usual SDK locations.
func findADB() (string, error) {
	if v := os.Getenv("PLAKAR_ADB"); v != "" {
		return v, nil
	}
	if p, err := exec.LookPath("adb"); err == nil {
		return p, nil
	}
	exe := "adb"
	if runtime.GOOS == "windows" {
		exe = "adb.exe"
	}
	var roots []string
	for _, env := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if v := os.Getenv(env); v != "" {
			roots = append(roots, v)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		switch runtime.GOOS {
		case "darwin":
			roots = append(roots, filepath.Join(home, "Library", "Android", "sdk"))
		case "windows":
			if v := os.Getenv("LOCALAPPDATA"); v != "" {
				roots = append(roots, filepath.Join(v, "Android", "Sdk"))
			}
		default:
			roots = append(roots, filepath.Join(home, "Android", "Sdk"))
		}
	}
	for _, root := range roots {
		p := filepath.Join(root, "platform-tools", exe)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("adb executable not found: install Android platform-tools and put adb in PATH, or start the server yourself with \"adb start-server\"")
}
