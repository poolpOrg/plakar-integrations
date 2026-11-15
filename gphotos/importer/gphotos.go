/*
 * Copyright (c) 2026 Plakar
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

// Package importer implements the Google Photos importer.
//
// Since March 2025 the Google Photos Library API no longer lets a
// third-party app enumerate a user's library: the only supported way to read
// media the user did not upload through the app is the Picker API, where the
// user picks items in a Google Photos web page tied to a session. The
// importer therefore works in rounds: for each round it opens a picker
// session, prints the picker URL for the user to open, waits until the user
// is done picking, then streams the picked items into the snapshot.
//
// The importer is interactive by nature: someone has to open the URLs it
// prints. It is not meant for unattended, scheduled backups.
//
// Location syntax:
//
//	gphotos://
//
// Config keys (see plugin/importer/gphotos.json):
//
//	credentials_file  OAuth2 client JSON ("Desktop app") from the Google Cloud console
//	token_file        where the OAuth2 token lives; created by the consent flow on first run
//	sessions          number of picking rounds (default 1)
//	max_items         items per round, capped by Google at 2000 (default 2000)
//	layout            "date" (default) files items under /YYYY/MM, "flat" puts them at the root
package importer

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PlakarKorp/integration-sdk"
	"github.com/PlakarKorp/integration-sdk/location"
	"golang.org/x/oauth2"
)

const (
	maxItemsPerSession  = 2000
	defaultPollInterval = 5 * time.Second
	defaultPollTimeout  = time.Hour // only when Google sends no timeout of its own
	baseURLRefreshAfter = time.Minute
)

type Importer struct {
	out io.Writer // where prompts for the user go; the plugin's stderr reaches the terminal

	// ctx outlives any single RPC: token refreshes and downloads happen
	// outside Import, so they cannot hang off the context of a request.
	ctx    context.Context
	cancel context.CancelFunc

	ts     oauth2.TokenSource
	client *http.Client
	api    *pickerClient

	sessions int    // picking rounds
	maxItems int    // per round
	layout   string // "date" or "flat"

	// refreshAfter is how old a cached listing must be before a failed
	// download is allowed to re-list the session for fresh base URLs, so
	// that a burst of concurrent failures triggers a single re-list.
	refreshAfter time.Duration

	mu       sync.Mutex
	opened   []string                // session ids to delete on Close
	baseURLs map[string]*sessionURLs // per session, refreshed when download URLs expire
}

// sessionURLs caches the base URLs of a session's items. Base URLs are only
// valid for about an hour, so a long backup re-lists the session to get
// fresh ones.
type sessionURLs struct {
	fetched time.Time
	urls    map[string]string // item id -> baseUrl
}

// pickedItem is a media item together with the session it was picked in,
// which is where a fresh base URL comes from when the cached one expires.
type pickedItem struct {
	mediaItem
	sessionID string
}

func init() {
	integration.RegisterImporter("gphotos", location.FLAG_STREAM, NewImporter)
}

func NewImporter(appCtx context.Context, opts *integration.Options, name string, config map[string]string) (integration.Importer, error) {
	rest := strings.TrimPrefix(config["location"], name+"://")
	if strings.Trim(rest, "/") != "" {
		return nil, fmt.Errorf("google photos: unexpected location %q, use %s://", config["location"], name)
	}

	out := io.Writer(os.Stderr)
	if opts != nil && opts.Stderr != nil {
		out = opts.Stderr
	}

	sessions, err := intParam(config, "sessions", 1)
	if err != nil {
		return nil, err
	}
	if sessions < 1 {
		return nil, errors.New("google photos: sessions must be at least 1")
	}

	maxItems, err := intParam(config, "max_items", maxItemsPerSession)
	if err != nil {
		return nil, err
	}
	if maxItems < 1 || maxItems > maxItemsPerSession {
		return nil, fmt.Errorf("google photos: max_items must be between 1 and %d", maxItemsPerSession)
	}

	layout := config["layout"]
	if layout == "" {
		layout = "date"
	}
	if layout != "date" && layout != "flat" {
		return nil, fmt.Errorf("google photos: unknown layout %q, want \"date\" or \"flat\"", layout)
	}

	// appCtx is the context of the Init request and is cancelled as soon as
	// it returns; keep its values but not its cancellation.
	ctx, cancel := context.WithCancel(context.WithoutCancel(appCtx))

	ts, err := tokenSource(ctx, config, out)
	if err != nil {
		cancel()
		return nil, err
	}

	client := oauth2.NewClient(ctx, ts)

	return &Importer{
		out:          out,
		ctx:          ctx,
		cancel:       cancel,
		ts:           ts,
		client:       client,
		api:          &pickerClient{http: client, endpoint: config["endpoint"]},
		sessions:     sessions,
		maxItems:     maxItems,
		layout:       layout,
		refreshAfter: baseURLRefreshAfter,
		baseURLs:     make(map[string]*sessionURLs),
	}, nil
}

func intParam(config map[string]string, key string, def int) (int, error) {
	v, ok := config[key]
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("google photos: %s must be an integer, got %q", key, v)
	}
	return n, nil
}

func (g *Importer) Origin() string        { return "photos.google.com" }
func (g *Importer) Type() string          { return "gphotos" }
func (g *Importer) Root() string          { return "/" }
func (g *Importer) Flags() location.Flags { return location.FLAG_STREAM }

// Ping checks that an access token is at hand, which exercises the refresh
// token against Google when there is one. It deliberately opens no picker
// session, since a session needs a human to complete it.
func (g *Importer) Ping(ctx context.Context) error {
	if _, err := g.ts.Token(); err != nil {
		return fmt.Errorf("google photos: cannot obtain an access token: %w", err)
	}
	return nil
}

func (g *Importer) Import(ctx context.Context, records chan<- *integration.Record, _ <-chan *integration.Result) (map[string]string, error) {
	defer close(records)

	dirs := make(map[string]bool)
	seenIDs := make(map[string]bool)
	seenNames := make(map[string]int)

	if !g.emitDir(ctx, records, dirs, "/", time.Unix(0, 0).UTC()) {
		return nil, ctx.Err()
	}

	for round := 1; round <= g.sessions; round++ {
		items, err := g.pick(ctx, round)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			fmt.Fprintf(g.out, "google photos: nothing picked, stopping\n")
			break
		}

		for _, item := range items {
			if seenIDs[item.ID] {
				continue // picked again in a later round
			}
			seenIDs[item.ID] = true
			if !g.emit(ctx, records, dirs, seenNames, item) {
				return nil, ctx.Err()
			}
		}
	}
	return nil, nil
}

// pick runs one picking round: open a session, hand the picker URL to the
// user, wait for the selection, and list what was picked.
func (g *Importer) pick(ctx context.Context, round int) ([]pickedItem, error) {
	sess, err := g.api.createSession(ctx, g.maxItems)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.opened = append(g.opened, sess.ID)
	g.mu.Unlock()

	what := fmt.Sprintf("pick up to %d photos and videos, then press Done", g.maxItems)
	if g.sessions > 1 {
		what = fmt.Sprintf("round %d of %d: %s", round, g.sessions, what)
	}
	prompt(g.out, what, sess.PickerURI)

	if err := g.waitForSelection(ctx, sess); err != nil {
		return nil, err
	}

	items, err := g.api.listAllMediaItems(ctx, sess.ID)
	if err != nil {
		return nil, err
	}

	// A stable order gives stable names to items that share a filename.
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreateTime != items[j].CreateTime {
			return items[i].CreateTime < items[j].CreateTime
		}
		return items[i].ID < items[j].ID
	})

	urls := make(map[string]string, len(items))
	picked := make([]pickedItem, 0, len(items))
	for _, item := range items {
		urls[item.ID] = item.MediaFile.BaseURL
		picked = append(picked, pickedItem{mediaItem: item, sessionID: sess.ID})
	}
	g.mu.Lock()
	g.baseURLs[sess.ID] = &sessionURLs{fetched: time.Now(), urls: urls}
	g.mu.Unlock()

	fmt.Fprintf(g.out, "google photos: %d items picked\n", len(items))
	return picked, nil
}

// waitForSelection polls the session until the user is done picking,
// honoring the polling interval and timeout Google sends back.
func (g *Importer) waitForSelection(ctx context.Context, sess *session) error {
	deadline := time.Now().Add(defaultPollTimeout)
	if d, ok := sess.timeoutIn(); ok {
		deadline = time.Now().Add(d)
	}

	for {
		if sess.MediaItemsSet {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("google photos: timed out waiting for the selection")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sess.pollInterval(defaultPollInterval)):
		}

		s, err := g.api.getSession(ctx, sess.ID)
		if err != nil {
			return err
		}
		if d, ok := s.timeoutIn(); ok {
			deadline = time.Now().Add(d)
		}
		sess = s
	}
}

// send hands a record to kloset, giving up when the backup is cancelled
// so that Import never blocks on a channel nobody drains anymore.
func send(ctx context.Context, records chan<- *integration.Record, r *integration.Record) bool {
	select {
	case records <- r:
		return true
	case <-ctx.Done():
		return false
	}
}

// emit sends the records for one picked item: the directories it lives in
// (once) and the item itself, with the download deferred until the reader
// is opened. It returns false when the backup was cancelled.
func (g *Importer) emit(ctx context.Context, records chan<- *integration.Record, dirs map[string]bool, seenNames map[string]int, item pickedItem) bool {
	created := item.createTime()

	dir := "/"
	if g.layout == "date" {
		if created.IsZero() {
			dir = "/undated"
			if !g.emitDir(ctx, records, dirs, dir, time.Unix(0, 0).UTC()) {
				return false
			}
		} else {
			year := "/" + created.Format("2006")
			if !g.emitDir(ctx, records, dirs, year, time.Date(created.Year(), 1, 1, 0, 0, 0, 0, time.UTC)) {
				return false
			}
			dir = year + "/" + created.Format("01")
			if !g.emitDir(ctx, records, dirs, dir, time.Date(created.Year(), created.Month(), 1, 0, 0, 0, 0, time.UTC)) {
				return false
			}
		}
	}

	name := uniqueName(seenNames, dir, item.MediaFile.Filename, item.ID)
	fullpath := path.Join(dir, name)

	if !item.ready() {
		return send(ctx, records, integration.NewError(fullpath, errors.New("google photos: video is still being processed by Google, pick it again later")))
	}

	fi := integration.FileInfo{
		Lname:    name,
		Lsize:    -1, // unknown until downloaded; the API does not report it
		Lmode:    0644,
		LmodTime: created,
		Ldev:     1,
		Lino:     inode(item.ID),
		Lnlink:   1,
	}

	id, sid, video := item.ID, item.sessionID, item.isVideo()
	return send(ctx, records, integration.NewRecord(fullpath, "", fi, nil, func() (io.ReadCloser, error) {
		return g.download(sid, id, video)
	}))
}

func (g *Importer) emitDir(ctx context.Context, records chan<- *integration.Record, dirs map[string]bool, dir string, mtime time.Time) bool {
	if dirs[dir] {
		return true
	}
	dirs[dir] = true
	name := path.Base(dir)
	if dir == "/" {
		name = "/"
	}
	fi := integration.FileInfo{
		Lname:    name,
		Lmode:    os.ModeDir | 0755,
		LmodTime: mtime,
		Ldev:     1,
		Lino:     inode("dir:" + dir),
		Lnlink:   1,
	}
	return send(ctx, records, integration.NewRecord(dir, "", fi, nil, nil))
}

// download fetches the bytes of an item. When the cached base URL has gone
// stale, the session is listed again for a fresh one and the download is
// retried once. Downloads run under the importer's own context: kloset
// opens readers after Import has returned.
func (g *Importer) download(sessionID, id string, video bool) (io.ReadCloser, error) {
	baseURL, err := g.baseURL(sessionID, id, false)
	if err != nil {
		return nil, err
	}

	rc, status, err := g.get(downloadURL(baseURL, video))
	if err != nil {
		return nil, err
	}
	if rc != nil {
		return rc, nil
	}

	// Base URLs expire after about an hour. Anything that is not a plain
	// success gets one retry with a freshly listed URL.
	baseURL, err = g.baseURL(sessionID, id, true)
	if err != nil {
		return nil, fmt.Errorf("google photos: download returned %s and the base URL could not be refreshed: %w", status, err)
	}
	rc, status, err = g.get(downloadURL(baseURL, video))
	if err != nil {
		return nil, err
	}
	if rc == nil {
		return nil, fmt.Errorf("google photos: download of media item returned %s", status)
	}
	return rc, nil
}

// get performs the download request. A nil reader with a nil error means a
// non-200 status, reported in status.
func (g *Importer) get(url string) (io.ReadCloser, string, error) {
	req, err := http.NewRequestWithContext(g.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, resp.Status, nil
	}
	return resp.Body, resp.Status, nil
}

// baseURL returns the base URL of an item, re-listing its session when
// refresh is set and the cached listing is older than refreshAfter.
func (g *Importer) baseURL(sessionID, id string, refresh bool) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	cache := g.baseURLs[sessionID]
	if cache == nil || (refresh && time.Since(cache.fetched) > g.refreshAfter) {
		items, err := g.api.listAllMediaItems(g.ctx, sessionID)
		if err != nil {
			return "", err
		}
		urls := make(map[string]string, len(items))
		for _, item := range items {
			urls[item.ID] = item.MediaFile.BaseURL
		}
		cache = &sessionURLs{fetched: time.Now(), urls: urls}
		g.baseURLs[sessionID] = cache
	}

	u, ok := cache.urls[id]
	if !ok || u == "" {
		return "", fmt.Errorf("google photos: media item %s is no longer part of session %s", id, sessionID)
	}
	return u, nil
}

// uniqueName returns a filename unique within dir, case-insensitively, since
// Google Photos does not guarantee unique filenames. Collisions get a
// numeric suffix; an item without a filename is named after its id.
func uniqueName(seen map[string]int, dir, filename, id string) string {
	name := filename
	if name == "" {
		name = id
	}
	name = strings.ReplaceAll(name, "/", "_")

	key := strings.ToLower(path.Join(dir, name))
	n, ok := seen[key]
	if !ok {
		seen[key] = 1
		return name
	}
	seen[key] = n + 1
	ext := path.Ext(name)
	return fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), n, ext)
}

// inode derives a stable pseudo inode from an item id so that kloset's
// cache recognises an unchanged item across backups.
func inode(id string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(id))
	return h.Sum64()
}

// Close deletes the picker sessions opened during Import, since Google
// limits how many sessions an app may hold per user, then stops any
// download still running.
func (g *Importer) Close(ctx context.Context) error {
	defer g.cancel()

	g.mu.Lock()
	ids := g.opened
	g.opened = nil
	g.mu.Unlock()

	var firstErr error
	for _, id := range ids {
		if err := g.api.deleteSession(ctx, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
