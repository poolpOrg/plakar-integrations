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

package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultEndpoint = "https://photospicker.googleapis.com"

// pickerClient is a minimal client for the Google Photos Picker API.
//
// The Picker API is the only Google Photos API through which a third-party
// app can read media the user did not upload through that same app: the
// user picks items in a Google Photos web page tied to a session, and the
// app then lists and downloads what was picked.
type pickerClient struct {
	http     *http.Client
	endpoint string // overridable for testing; defaults to defaultEndpoint
}

func (c *pickerClient) base() string {
	if c.endpoint != "" {
		return strings.TrimSuffix(c.endpoint, "/")
	}
	return defaultEndpoint
}

// session mirrors the PickingSession resource.
type session struct {
	ID            string `json:"id"`
	PickerURI     string `json:"pickerUri"`
	ExpireTime    string `json:"expireTime"`
	MediaItemsSet bool   `json:"mediaItemsSet"`
	PollingConfig *struct {
		PollInterval string `json:"pollInterval"` // protobuf JSON duration, e.g. "3.5s"
		TimeoutIn    string `json:"timeoutIn"`
	} `json:"pollingConfig"`
}

// pollInterval returns the interval Google recommends between two polls of
// this session, or fallback when none was given.
func (s *session) pollInterval(fallback time.Duration) time.Duration {
	if s.PollingConfig != nil {
		if d, err := time.ParseDuration(s.PollingConfig.PollInterval); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

// timeoutIn returns how long Google is still willing to have this session
// polled. ok is false when the session carries no timeout information.
func (s *session) timeoutIn() (d time.Duration, ok bool) {
	if s.PollingConfig == nil || s.PollingConfig.TimeoutIn == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s.PollingConfig.TimeoutIn)
	if err != nil {
		return 0, false
	}
	return d, true
}

// mediaItem mirrors the subset of the PickedMediaItem resource we use.
type mediaItem struct {
	ID         string `json:"id"`
	CreateTime string `json:"createTime"`
	Type       string `json:"type"` // PHOTO, VIDEO
	MediaFile  struct {
		BaseURL           string `json:"baseUrl"`
		MimeType          string `json:"mimeType"`
		Filename          string `json:"filename"`
		MediaFileMetadata struct {
			Width         int `json:"width"`
			Height        int `json:"height"`
			VideoMetadata *struct {
				ProcessingStatus string `json:"processingStatus"`
			} `json:"videoMetadata"`
		} `json:"mediaFileMetadata"`
	} `json:"mediaFile"`
}

func (m *mediaItem) isVideo() bool {
	return m.Type == "VIDEO" || strings.HasPrefix(m.MediaFile.MimeType, "video/")
}

// ready reports whether the bytes of the item can be downloaded. Videos are
// only downloadable once Google is done processing them.
func (m *mediaItem) ready() bool {
	if !m.isVideo() {
		return true
	}
	vm := m.MediaFile.MediaFileMetadata.VideoMetadata
	if vm == nil {
		return true
	}
	return vm.ProcessingStatus == "" || vm.ProcessingStatus == "READY"
}

func (m *mediaItem) createTime() time.Time {
	t, err := time.Parse(time.RFC3339, m.CreateTime)
	if err != nil {
		return time.Time{}
	}
	return t
}

// downloadURL returns the URL to fetch the bytes of an item from its base
// URL. "=d" asks for the photo with its Exif metadata; "=dv" asks for the
// full-quality video.
func downloadURL(baseURL string, video bool) string {
	if video {
		return baseURL + "=dv"
	}
	return baseURL + "=d"
}

// createSession opens a new picking session. maxItems caps how many items
// the user may pick in it; 0 leaves Google's default (2000).
func (c *pickerClient) createSession(ctx context.Context, maxItems int) (*session, error) {
	body := map[string]any{}
	if maxItems > 0 {
		body["pickingConfig"] = map[string]any{
			"maxItemCount": strconv.Itoa(maxItems),
		}
	}
	var s session
	if err := c.postJSON(ctx, "/v1/sessions", body, &s); err != nil {
		return nil, err
	}
	if s.ID == "" || s.PickerURI == "" {
		return nil, fmt.Errorf("google photos: malformed session response")
	}
	return &s, nil
}

func (c *pickerClient) getSession(ctx context.Context, id string) (*session, error) {
	var s session
	if err := c.getJSON(ctx, "/v1/sessions/"+url.PathEscape(id), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *pickerClient) deleteSession(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base()+"/v1/sessions/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// listMediaItems returns one page of the items picked in a session, and the
// token for the next page ("" when there are no more).
func (c *pickerClient) listMediaItems(ctx context.Context, sessionID, pageToken string) ([]mediaItem, string, error) {
	q := url.Values{}
	q.Set("sessionId", sessionID)
	q.Set("pageSize", "100")
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}

	var out struct {
		MediaItems    []mediaItem `json:"mediaItems"`
		NextPageToken string      `json:"nextPageToken"`
	}
	if err := c.getJSON(ctx, "/v1/mediaItems?"+q.Encode(), &out); err != nil {
		return nil, "", err
	}
	return out.MediaItems, out.NextPageToken, nil
}

// listAllMediaItems drains every page of a session.
func (c *pickerClient) listAllMediaItems(ctx context.Context, sessionID string) ([]mediaItem, error) {
	var all []mediaItem
	pageToken := ""
	for {
		items, next, err := c.listMediaItems(ctx, sessionID, pageToken)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if next == "" {
			return all, nil
		}
		pageToken = next
	}
}

func (c *pickerClient) getJSON(ctx context.Context, pathAndQuery string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+pathAndQuery, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *pickerClient) postJSON(ctx context.Context, path string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *pickerClient) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("google photos: %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, bytes.TrimSpace(data))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
