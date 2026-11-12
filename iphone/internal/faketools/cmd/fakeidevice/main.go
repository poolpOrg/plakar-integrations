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

// fakeidevice impersonates idevice_id, idevicepair and idevicebackup2 for
// end-to-end runs of the plugins through a real plakar without a device.
// Install it under those three names (symlinks work) in a directory put
// first in PATH. It pretends one paired device is connected; its UDID comes
// from FAKEIDEVICE_UDID when set.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poolpOrg/plakar-integrations/iphone/common"
	"github.com/poolpOrg/plakar-integrations/iphone/internal/faketools"
)

func main() {
	udid := os.Getenv("FAKEIDEVICE_UDID")
	if udid == "" {
		udid = "00008030-001A2B3C4D5E6F78"
	}
	fake := faketools.New(udid)
	fake.BackupOutput = "Backup directory is \"" + os.Args[len(os.Args)-1] + "\"\nStarted \"com.apple.mobilebackup2\" service on port 49152.\nNegotiated Protocol Version 2.1\n"

	res, err := fake.Run(context.Background(), &common.Invocation{
		Path:   filepath.Base(os.Args[0]),
		Args:   os.Args[1:],
		Env:    nil,
		Stream: os.Stdout,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(255)
	}
	os.Exit(res.ExitCode)
}
