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

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
)

// The admin side of the API used by immich+dump://: database dumps, the
// maintenance restore flow and the job queues.

// MaintenanceCookie is the cookie the maintenance login sets; the routes
// of the maintenance worker accept nothing else.
const MaintenanceCookie = "immich_maintenance_token"

// Maintenance actions and the states the worker reports.
const (
	ActionRestoreDatabase       = "restore_database"
	ActionSelectDatabaseRestore = "select_database_restore"
	ActionEnd                   = "end"
	TaskError                   = "error"

	JobBackupDatabase        = "backup-database"
	QueueThumbnailGeneration = "thumbnailGeneration"
)

// Backup is one entry of GET /admin/database-backups.
type Backup struct {
	Filename string `json:"filename"`
	Filesize int64  `json:"filesize"`
	Timezone string `json:"timezone"`
}

// ServerAbout is GET /server/about; only the version is read.
type ServerAbout struct {
	Version string `json:"version"`
}

// ServerConfig is GET /server/config, public on both the server and its
// maintenance worker.
type ServerConfig struct {
	IsInitialized   bool `json:"isInitialized"`
	MaintenanceMode bool `json:"maintenanceMode"`
}

// MaintenanceStatus is GET /admin/maintenance/status.  The server proper
// always answers inactive; the maintenance worker reports its progress.
type MaintenanceStatus struct {
	Active   bool    `json:"active"`
	Action   string  `json:"action"`
	Task     string  `json:"task"`
	Progress float64 `json:"progress"`
	Error    string  `json:"error"`
}

// String is the status as the progress line shows it.
func (s *MaintenanceStatus) String() string {
	if !s.Active {
		return "inactive"
	}
	if s.Task == "" {
		return s.Action
	}
	return fmt.Sprintf("%s: %s %d%%", s.Action, s.Task, int(s.Progress*100))
}

// ServerAbout is GET /server/about.
func (c *Client) ServerAbout(ctx context.Context) (*ServerAbout, error) {
	var a ServerAbout
	if err := c.get(ctx, "/server/about", nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// ServerConfig is GET /server/config.
func (c *Client) ServerConfig(ctx context.Context) (*ServerConfig, error) {
	var cfg ServerConfig
	if err := c.get(ctx, "/server/config", nil, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ListBackups is GET /admin/database-backups.
func (c *Client) ListBackups(ctx context.Context) ([]Backup, error) {
	var resp struct {
		Backups []Backup `json:"backups"`
	}
	if err := c.get(ctx, "/admin/database-backups", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Backups, nil
}

// DownloadBackup streams GET /admin/database-backups/{filename}, bounded
// by size when it is known.
func (c *Client) DownloadBackup(ctx context.Context, filename string, size int64) (io.ReadCloser, error) {
	return c.Download(ctx, "/admin/database-backups/"+url.PathEscape(filename), size)
}

// UploadBackup streams one dump to POST /admin/database-backups/upload;
// the server stores it as uploaded-<filename>.
func (c *Client) UploadBackup(ctx context.Context, filename string, r io.Reader) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(func() error {
			if err := copyPart(mw, "file", filename, func() (io.ReadCloser, error) {
				return io.NopCloser(r), nil
			}); err != nil {
				return err
			}
			if err := mw.Close(); err != nil {
				return fmt.Errorf("close multipart: %w", err)
			}
			return nil
		}())
	}()
	defer func() {
		_ = pr.Close()
		<-done
	}()

	resp, err := c.do(ctx, http.MethodPost, "/admin/database-backups/upload", nil, pr, mw.FormDataContentType(), nil)
	if err != nil {
		return fmt.Errorf("upload %q: %w", filename, err)
	}
	defer closeQuietly(resp.Body)
	return decode(resp, http.MethodPost, "/admin/database-backups/upload", nil)
}

// MaintenanceStatus is GET /admin/maintenance/status, public.
func (c *Client) MaintenanceStatus(ctx context.Context) (*MaintenanceStatus, error) {
	var st MaintenanceStatus
	if err := c.get(ctx, "/admin/maintenance/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// MaintenanceLogin redeems the maintenance token printed in the server log
// with POST /admin/maintenance/login and keeps the cookie it sets for the
// requests that follow.
func (c *Client) MaintenanceLogin(ctx context.Context, token string) error {
	in := struct {
		Token string `json:"token"`
	}{Token: token}
	resp, err := c.doJSON(ctx, http.MethodPost, "/admin/maintenance/login", &in)
	if err != nil {
		return err
	}
	defer closeQuietly(resp.Body)
	if err := decode(resp, http.MethodPost, "/admin/maintenance/login", nil); err != nil {
		return err
	}

	for _, ck := range resp.Cookies() {
		if ck.Name == MaintenanceCookie {
			c.cookie = &http.Cookie{Name: ck.Name, Value: ck.Value}
			return nil
		}
	}
	return errors.New("POST /admin/maintenance/login: no maintenance cookie in the answer")
}

// SetMaintenance is POST /admin/maintenance.  filename is only sent with
// the restore action.
func (c *Client) SetMaintenance(ctx context.Context, action, filename string) error {
	in := struct {
		Action                string `json:"action"`
		RestoreBackupFilename string `json:"restoreBackupFilename,omitempty"`
	}{Action: action, RestoreBackupFilename: filename}
	return c.post(ctx, "/admin/maintenance", &in, nil)
}

// RunJob is POST /jobs: queues one of the server's named jobs.
func (c *Client) RunJob(ctx context.Context, name string) error {
	in := struct {
		Name string `json:"name"`
	}{Name: name}
	return c.post(ctx, "/jobs", &in, nil)
}

// JobCommand is PUT /jobs/{queue}: start, pause, resume or empty a queue.
func (c *Client) JobCommand(ctx context.Context, queue, command string, force bool) error {
	in := struct {
		Command string `json:"command"`
		Force   bool   `json:"force"`
	}{Command: command, Force: force}
	return c.put(ctx, "/jobs/"+url.PathEscape(queue), &in, nil)
}
