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

// Package nextcloud backs up and restores the files of a Nextcloud user
// through the server's WebDAV endpoint.
//
// A snapshot holds the user's files under /files, and optionally the
// trash bin under /trashbin and the kept versions of every file under
// /versions/<path>/<timestamp>, as Nextcloud itself lays them out.
package nextcloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
)

const (
	filesPrefix    = "/files"
	trashPrefix    = "/trashbin"
	versionsPrefix = "/versions"
)

var errNotRegular = errors.New("not a regular file or a directory")

// Nextcloud is both the importer and the exporter.
type Nextcloud struct {
	client      *client
	host        string
	root        string
	trash       bool
	versions    bool
	concurrency int

	mu     sync.Mutex
	userID string
}

// New builds a connector from the location nextcloud://host[:port][/folder]
// and the username, password, prefix, insecure, trash and versions options.
func New(_ context.Context, opts *integration.Options, proto string, config map[string]string) (*Nextcloud, error) {
	if proto != "nextcloud" {
		return nil, fmt.Errorf("unsupported protocol %q", proto)
	}

	loc, err := url.Parse(config["location"])
	if err != nil {
		return nil, fmt.Errorf("parse location: %w", err)
	}
	if loc.Scheme != "nextcloud" || loc.Host == "" {
		return nil, fmt.Errorf("location %q: want nextcloud://host[:port][/folder]", config["location"])
	}
	if config["username"] == "" || config["password"] == "" {
		return nil, errors.New("username and password are required")
	}

	insecure, _ := strconv.ParseBool(config["insecure"])
	trash, _ := strconv.ParseBool(config["trash"])
	versions, _ := strconv.ParseBool(config["versions"])

	base := &url.URL{Scheme: "https", Host: loc.Host, Path: path.Clean("/" + config["prefix"])}
	if insecure {
		base.Scheme = "http"
	}

	return &Nextcloud{
		client: &client{
			http:     &http.Client{},
			base:     base,
			username: config["username"],
			password: config["password"],
		},
		host:        loc.Host,
		root:        path.Clean("/" + loc.Path),
		trash:       trash,
		versions:    versions,
		concurrency: max(opts.MaxConcurrency, 1),
	}, nil
}

// NewImporter is the importer entry point.
func NewImporter(ctx context.Context, opts *integration.Options, proto string, config map[string]string) (integration.Importer, error) {
	return New(ctx, opts, proto, config)
}

// NewExporter is the exporter entry point.
func NewExporter(ctx context.Context, opts *integration.Options, proto string, config map[string]string) (integration.Exporter, error) {
	return New(ctx, opts, proto, config)
}

// Origin is the Nextcloud host.
func (n *Nextcloud) Origin() string { return n.host }

// Type is the connector name.
func (n *Nextcloud) Type() string { return "nextcloud" }

// Root is the folder inside the user's files the connector works on.
func (n *Nextcloud) Root() string { return n.root }

// Flags are none: a remote tree.
func (n *Nextcloud) Flags() location.Flags { return 0 }

// Ping checks the credentials and that the root folder exists.
func (n *Nextcloud) Ping(ctx context.Context) error {
	user, err := n.user(ctx)
	if err != nil {
		return err
	}
	_, err = n.client.propfind(ctx, filesRoot(user)+n.root, 0)
	return err
}

// Close drops the idle connections.
func (n *Nextcloud) Close(context.Context) error {
	n.client.http.CloseIdleConnections()
	return nil
}

func (n *Nextcloud) user(ctx context.Context) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.userID == "" {
		id, err := n.client.userID(ctx)
		if err != nil {
			return "", fmt.Errorf("resolve user id: %w", err)
		}
		n.userID = id
	}
	return n.userID, nil
}

func filesRoot(user string) string { return "/remote.php/dav/files/" + user }
func trashRoot(user string) string { return "/remote.php/dav/trashbin/" + user + "/trash" }
func versionsRoot(user, fileID string) string {
	return "/remote.php/dav/versions/" + user + "/versions/" + fileID
}

// relative maps a server href below root to a snapshot path under prefix.
func relative(prefix, root, href string) string {
	return path.Join(prefix, strings.TrimPrefix(path.Clean(href), path.Clean(root)))
}

func dirRecord(pathname string) *integration.Record {
	fi := integration.FileInfo{Lname: path.Base(pathname), Lmode: fs.ModeDir | 0755, LmodTime: time.Now()}
	return integration.NewRecord(pathname, "", fi, nil, func() (io.ReadCloser, error) {
		return nil, errors.ErrUnsupported
	})
}

func (n *Nextcloud) record(ctx context.Context, pathname string, e entry) *integration.Record {
	fi := integration.FileInfo{Lname: path.Base(pathname), Lsize: e.size, Lmode: 0644, LmodTime: e.modTime}
	if e.isDir {
		fi.Lmode = fs.ModeDir | 0755
		fi.Lsize = 0
	}
	href := e.href
	return integration.NewRecord(pathname, "", fi, nil, func() (io.ReadCloser, error) {
		return n.client.get(ctx, href)
	})
}

// Import walks the user's files, then the trash bin and the file versions
// when asked to.
func (n *Nextcloud) Import(ctx context.Context, records chan<- *integration.Record, _ <-chan *integration.Result) error {
	defer close(records)

	user, err := n.user(ctx)
	if err != nil {
		return err
	}

	send := func(r *integration.Record) {
		select {
		case records <- r:
		case <-ctx.Done():
		}
	}

	var mu sync.Mutex
	files := make(map[string]versioned)

	// the walk reports its root, which is /files itself unless a folder
	// was given; kloset fills in the parents of the first record otherwise,
	// but an explicit /files keeps the layout the same in both cases.
	if n.root != "/" {
		send(dirRecord(filesPrefix))
	}
	err = n.client.walk(ctx, filesRoot(user)+n.root, n.concurrency, func(e entry, err error) {
		pathname := relative(filesPrefix, filesRoot(user), e.href)
		if err != nil {
			send(integration.NewError(pathname, fmt.Errorf("list %s: %w", pathname, err)))
			return
		}
		if n.versions && !e.isDir && e.fileID != "" {
			mu.Lock()
			files[e.fileID] = versioned{pathname: pathname, modTime: e.modTime}
			mu.Unlock()
		}
		send(n.record(ctx, pathname, e))
	})
	if err != nil {
		return fmt.Errorf("list %s: %w", n.root, err)
	}

	if n.trash {
		err = n.client.walk(ctx, trashRoot(user), n.concurrency, func(e entry, err error) {
			pathname := relative(trashPrefix, trashRoot(user), e.href)
			if err != nil {
				send(integration.NewError(pathname, fmt.Errorf("list %s: %w", pathname, err)))
				return
			}
			send(n.record(ctx, pathname, e))
		})
		if err != nil {
			return fmt.Errorf("list trash bin: %w", err)
		}
	}

	if n.versions {
		n.importVersions(ctx, user, files, send)
	}

	return ctx.Err()
}

// versioned is a file seen during the walk whose versions are to be listed.
type versioned struct {
	pathname string
	modTime  time.Time
}

// importVersions lists the kept versions of every file seen during the
// walk.  Nextcloud keys them by file id, one collection per file, so this
// is one request per file.
func (n *Nextcloud) importVersions(ctx context.Context, user string, files map[string]versioned, send func(*integration.Record)) {
	send(dirRecord(versionsPrefix))

	var wg sync.WaitGroup
	sem := make(chan struct{}, n.concurrency)
	for id, file := range files {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			dir := versionsPrefix + strings.TrimPrefix(file.pathname, filesPrefix)
			root := versionsRoot(user, id)
			entries, err := n.client.propfind(ctx, root, 1)
			var se *statusError
			if errors.As(err, &se) && se.Code == http.StatusNotFound {
				return
			}
			if err != nil {
				send(integration.NewError(dir, fmt.Errorf("list versions of %s: %w", file.pathname, err)))
				return
			}

			// the listing also names the current revision, by the
			// file's own mtime; it has no content of its own and a GET
			// on it is a 404
			current := strconv.FormatInt(file.modTime.Unix(), 10)
			var versions []entry
			for _, e := range entries {
				if path.Clean(e.href) != path.Clean(root) && path.Base(e.href) != current {
					versions = append(versions, e)
				}
			}
			if len(versions) == 0 {
				return
			}
			send(dirRecord(dir))
			for _, v := range versions {
				send(n.record(ctx, path.Join(dir, path.Base(v.href)), v))
			}
		}()
	}
	wg.Wait()
}

// contained joins a record pathname onto root and refuses anything that
// would land outside of it: path.Join cleans as it goes, so "/../x" would
// otherwise resolve above the folder we were pointed at.
func contained(root, pathname string) (string, error) {
	joined := path.Join(root, pathname)
	cleanRoot := path.Clean(root)

	if joined != cleanRoot && !strings.HasPrefix(joined, strings.TrimSuffix(cleanRoot, "/")+"/") {
		return "", fmt.Errorf("path %q escapes the restore root %q", pathname, cleanRoot)
	}
	return joined, nil
}

// Export writes the records under the root folder of the user's files.
func (n *Nextcloud) Export(ctx context.Context, records <-chan *integration.Record, results chan<- *integration.Result) error {
	defer close(results)

	user, err := n.user(ctx)
	if err != nil {
		return err
	}
	files := filesRoot(user)

	// the destination folder and its parents may not exist yet
	for i := 1; i < len(n.root); i++ {
		if n.root[i] == '/' {
			if err := n.client.mkcol(ctx, files+n.root[:i]); err != nil {
				return fmt.Errorf("create %s: %w", n.root[:i], err)
			}
		}
	}
	if n.root != "/" {
		if err := n.client.mkcol(ctx, files+n.root); err != nil {
			return fmt.Errorf("create %s: %w", n.root, err)
		}
	}

	for record := range records {
		if record.Err != nil || record.IsXattr {
			results <- record.Ok()
			continue
		}

		dest, err := contained(n.root, record.Pathname)
		if err != nil {
			results <- record.Error(err)
			continue
		}

		switch {
		case record.FileInfo.IsDir():
			err = n.client.mkcol(ctx, files+dest)
		case record.FileInfo.Mode().IsRegular():
			err = n.client.put(ctx, files+dest, record.Reader, record.FileInfo.Size(), record.FileInfo.ModTime())
		default:
			err = errNotRegular
		}
		if err != nil {
			results <- record.Error(fmt.Errorf("%s: %w", dest, err))
			continue
		}
		results <- record.Ok()
	}
	return nil
}
