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

// fakeadbd runs the test fake adb server as a standalone process, so that
// plakar itself can be exercised against a fake device:
//
//	fakeadbd -listen 127.0.0.1:5555 -dir ./sample &
//	plakar at /tmp/store backup -o adb_server=127.0.0.1:5555 android://FAKE0001
//
// The device's shared storage (/storage/emulated/0) is loaded in memory
// from -dir, or from a small built-in sample when -dir is empty.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/poolpOrg/plakar-integrations/android/adb/adbtest"
)

const storageRoot = "/storage/emulated/0"

// maxMirrorBytes caps what -dir loads in memory.
const maxMirrorBytes = 1 << 30

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "address to listen on")
	serial := flag.String("serial", "FAKE0001", "serial of the fake device")
	dir := flag.String("dir", "", "host directory mirrored as the device's shared storage")
	legacy := flag.Bool("legacy", false, "advertise no features, like an Android 6 device")
	flag.Parse()

	srv, err := adbtest.Listen(*listen)
	if err != nil {
		log.Fatal(err)
	}
	dev := &adbtest.Device{Serial: *serial, Model: "Fake_Phone", Product: "fake", Name: "fakedev"}
	if *legacy {
		dev.Features = adbtest.LegacyFeatures
	}
	srv.AddDevice(dev)
	dev.FS.NoSymlinks = true

	if *dir != "" {
		if err := mirror(dev.FS, *dir); err != nil {
			log.Fatalf("mirroring %s: %v", *dir, err)
		}
	} else {
		sample(dev.FS)
	}
	dev.FS.Symlink("/sdcard", storageRoot, time.Now())

	fmt.Println(srv.Addr)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	srv.Close()
}

func sample(dfs *adbtest.FS) {
	ts := time.Unix(1700000000, 0)
	dfs.MkdirAll(storageRoot+"/DCIM/Camera", ts)
	dfs.WriteFile(storageRoot+"/DCIM/Camera/IMG_0001.jpg", []byte(strings.Repeat("jpeg", 300*1024)), 0o660, ts)
	dfs.WriteFile(storageRoot+"/DCIM/Camera/IMG_0002.jpg", []byte("second photo"), 0o660, ts.Add(time.Hour))
	dfs.WriteFile(storageRoot+"/Download/report final (2).pdf", []byte("pdf"), 0o660, ts)
	dfs.WriteFile(storageRoot+"/Download/fichier é 日本.txt", []byte("unicode"), 0o660, ts)
	dfs.WriteFile(storageRoot+"/Download/empty.txt", nil, 0o660, ts)
	dfs.MkdirAll(storageRoot+"/Pictures/Screenshots", ts)
	dfs.MkdirAll(storageRoot+"/Android/data", ts)
}

// mirror loads a host directory into the fake device's shared storage.
func mirror(dfs *adbtest.FS, dir string) error {
	var total int64
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		target := storageRoot
		if rel != "." {
			target = path.Join(storageRoot, filepath.ToSlash(rel))
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			dfs.MkdirAll(target, info.ModTime())
		case info.Mode().IsRegular():
			total += info.Size()
			if total > maxMirrorBytes {
				return fmt.Errorf("directory too large to hold in memory (over %d bytes)", maxMirrorBytes)
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			dfs.WriteFile(target, data, 0o660, info.ModTime())
		}
		return nil
	})
}
