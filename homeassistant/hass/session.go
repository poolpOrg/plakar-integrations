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

package hass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// readLimit bounds a single websocket message, backup/info lists every
// backup the instance knows about.
const readLimit = 64 << 20

// restoreTimeout bounds how long we wait for the instance to accept a
// restore before giving up on it.
const restoreTimeout = 2 * time.Minute

// Local backup agents, the ones that keep backups on the instance's own
// disk: core installs expose backup.local, supervised ones hassio.local.
var localAgents = []string{"backup.local", SupervisedAgent}

// SupervisedAgent is the local agent of Home Assistant OS and
// supervised installs, the only ones with add-ons and shared folders.
const SupervisedAgent = "hassio.local"

// Agent is a destination the backup manager can write backups to.
type Agent struct {
	ID   string `json:"agent_id"`
	Name string `json:"name"`
}

// AgentState is what one agent knows about a backup.
type AgentState struct {
	Protected bool  `json:"protected"`
	Size      int64 `json:"size"`
}

// Backup is a backup as listed by the backup manager.
type Backup struct {
	ID        string                `json:"backup_id"`
	Name      string                `json:"name"`
	Date      string                `json:"date"`
	Protected bool                  `json:"protected"` // older cores, newer ones say it per agent
	Agents    map[string]AgentState `json:"agents"`
	Failed    []string              `json:"failed_agent_ids"`
}

// ProtectedOn reports whether the copy held by agentID is encrypted.
func (b *Backup) ProtectedOn(agentID string) bool {
	if st, ok := b.Agents[agentID]; ok {
		return st.Protected
	}
	return b.Protected
}

// GenerateOptions are the knobs of backup/generate.
type GenerateOptions struct {
	AgentID         string
	Name            string
	Password        string
	IncludeDatabase bool
	IncludeAddons   bool     // supervised installs only
	IncludeFolders  []string // supervised installs only: media, share, ssl, addons/local
}

// RestoreOptions are the knobs of backup/restore.
type RestoreOptions struct {
	BackupID string
	AgentID  string
	Password string
	Addons   []string // supervised installs only
	Folders  []string // supervised installs only
}

// Event is a backup manager state event.
type Event struct {
	ManagerState string `json:"manager_state"`
	Stage        string `json:"stage"`
	State        string `json:"state"`
	Reason       string `json:"reason"`
}

type message struct {
	ID      int             `json:"id,omitempty"`
	Type    string          `json:"type"`
	Success *bool           `json:"success,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Event   json.RawMessage `json:"event,omitempty"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Message string `json:"message,omitempty"` // auth_invalid
}

// Session is an authenticated websocket session.  Commands run one at
// a time; events for the subscription made with Subscribe are queued
// while a command waits for its result.
type Session struct {
	conn   *websocket.Conn
	nextID int
	subID  int
	events []Event
}

// Connect opens a websocket session and authenticates with the token.
func (c *Client) Connect(ctx context.Context) (*Session, error) {
	conn, _, err := websocket.Dial(ctx, c.loc.WebsocketURL(), &websocket.DialOptions{HTTPClient: c.http})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.loc, err)
	}
	conn.SetReadLimit(readLimit)

	s := &Session{conn: conn}
	if err := s.auth(ctx, c.token); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("%s: %w", c.loc, err)
	}
	return s, nil
}

func (s *Session) auth(ctx context.Context, token string) error {
	var msg message
	if err := wsjson.Read(ctx, s.conn, &msg); err != nil {
		return err
	}
	if msg.Type != "auth_required" {
		return fmt.Errorf("unexpected websocket greeting %q", msg.Type)
	}
	if err := wsjson.Write(ctx, s.conn, map[string]any{"type": "auth", "access_token": token}); err != nil {
		return err
	}
	if err := wsjson.Read(ctx, s.conn, &msg); err != nil {
		return err
	}
	switch msg.Type {
	case "auth_ok":
		return nil
	case "auth_invalid":
		return fmt.Errorf("authentication refused: %s", msg.Message)
	}
	return fmt.Errorf("unexpected websocket reply %q to auth", msg.Type)
}

// Close ends the session.
func (s *Session) Close() error {
	return s.conn.Close(websocket.StatusNormalClosure, "")
}

// readOne receives one message.  A subscription event is queued and
// reported as nil, so that nothing is lost while a command is in
// flight.
func (s *Session) readOne(ctx context.Context) (*message, error) {
	var msg message
	if err := wsjson.Read(ctx, s.conn, &msg); err != nil {
		return nil, err
	}
	if msg.Type == "event" && s.subID != 0 && msg.ID == s.subID {
		var ev Event
		if err := json.Unmarshal(msg.Event, &ev); err != nil {
			return nil, fmt.Errorf("malformed event: %w", err)
		}
		s.events = append(s.events, ev)
		return nil, nil
	}
	return &msg, nil
}

// read waits for the next message that is not a subscription event.
func (s *Session) read(ctx context.Context) (*message, error) {
	for {
		msg, err := s.readOne(ctx)
		if err != nil || msg != nil {
			return msg, err
		}
	}
}

// call sends a command and decodes its result into out, if not nil.
func (s *Session) call(ctx context.Context, cmd map[string]any, out any) error {
	s.nextID++
	id := s.nextID
	cmd["id"] = id

	kind, _ := cmd["type"].(string)
	if err := wsjson.Write(ctx, s.conn, cmd); err != nil {
		return fmt.Errorf("%s: %w", kind, err)
	}

	for {
		msg, err := s.read(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}
		if msg.Type != "result" || msg.ID != id {
			continue
		}
		if msg.Success == nil || !*msg.Success {
			if msg.Error != nil {
				return fmt.Errorf("%s: %s (%s)", kind, msg.Error.Message, msg.Error.Code)
			}
			return fmt.Errorf("%s: command failed", kind)
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(msg.Result, out); err != nil {
			return fmt.Errorf("%s: malformed result: %w", kind, err)
		}
		return nil
	}
}

// Agents lists the backup agents of the instance.
func (s *Session) Agents(ctx context.Context) ([]Agent, error) {
	var res struct {
		Agents []Agent `json:"agents"`
	}
	if err := s.call(ctx, map[string]any{"type": "backup/agents/info"}, &res); err != nil {
		return nil, err
	}
	return res.Agents, nil
}

// LocalAgent returns the agent keeping backups on the instance itself.
func (s *Session) LocalAgent(ctx context.Context) (string, error) {
	agents, err := s.Agents(ctx)
	if err != nil {
		return "", err
	}
	for _, want := range localAgents {
		if hasAgent(agents, want) {
			return want, nil
		}
	}
	var ids []string
	for _, a := range agents {
		ids = append(ids, a.ID)
	}
	return "", fmt.Errorf("no local backup agent on the instance, set agent to one of: %s", strings.Join(ids, ", "))
}

// Supervised reports whether the instance runs under the supervisor,
// where backups carry add-ons and shared folders.
func (s *Session) Supervised(ctx context.Context) (bool, error) {
	agents, err := s.Agents(ctx)
	if err != nil {
		return false, err
	}
	return hasAgent(agents, SupervisedAgent), nil
}

func hasAgent(agents []Agent, id string) bool {
	for _, a := range agents {
		if a.ID == id {
			return true
		}
	}
	return false
}

// List returns the backups known to the instance.
func (s *Session) List(ctx context.Context) ([]Backup, error) {
	var res struct {
		Backups []Backup `json:"backups"`
	}
	if err := s.call(ctx, map[string]any{"type": "backup/info"}, &res); err != nil {
		return nil, err
	}
	return res.Backups, nil
}

// Get returns one backup, or nil when the instance does not know it.
func (s *Session) Get(ctx context.Context, backupID string) (*Backup, error) {
	var res struct {
		Backup *Backup `json:"backup"`
	}
	if err := s.call(ctx, map[string]any{"type": "backup/details", "backup_id": backupID}, &res); err != nil {
		return nil, err
	}
	return res.Backup, nil
}

// Delete removes a backup from every agent holding it.
func (s *Session) Delete(ctx context.Context, backupID string) error {
	var res struct {
		AgentErrors map[string]string `json:"agent_errors"`
	}
	if err := s.call(ctx, map[string]any{"type": "backup/delete", "backup_id": backupID}, &res); err != nil {
		return err
	}
	var errs []error
	for agent, msg := range res.AgentErrors {
		errs = append(errs, fmt.Errorf("backup/delete: %s: %s", agent, msg))
	}
	return errors.Join(errs...)
}

// Subscribe starts receiving backup manager events.
func (s *Session) Subscribe(ctx context.Context) error {
	s.nextID++
	id := s.nextID
	if err := wsjson.Write(ctx, s.conn, map[string]any{"id": id, "type": "backup/subscribe_events"}); err != nil {
		return fmt.Errorf("backup/subscribe_events: %w", err)
	}
	s.subID = id
	s.events = nil

	for {
		msg, err := s.read(ctx)
		if err != nil {
			return fmt.Errorf("backup/subscribe_events: %w", err)
		}
		if msg.Type == "result" && msg.ID == id {
			if msg.Success == nil || !*msg.Success {
				return errors.New("backup/subscribe_events: command failed")
			}
			// the events replayed before the ack describe the past
			s.events = nil
			return nil
		}
	}
}

// NextEvent returns the next backup manager event.
func (s *Session) NextEvent(ctx context.Context) (Event, error) {
	for len(s.events) == 0 {
		msg, err := s.readOne(ctx)
		if err != nil {
			return Event{}, err
		}
		if msg != nil {
			return Event{}, fmt.Errorf("unexpected %s message while waiting for events", msg.Type)
		}
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}

// Generate asks the instance for a new backup and returns its job id,
// which for core installs is also the backup id.
func (s *Session) Generate(ctx context.Context, opts GenerateOptions) (string, error) {
	cmd := map[string]any{
		"type":                  "backup/generate",
		"agent_ids":             []string{opts.AgentID},
		"include_database":      opts.IncludeDatabase,
		"include_homeassistant": true,
	}
	if opts.Name != "" {
		cmd["name"] = opts.Name
	}
	if opts.Password != "" {
		cmd["password"] = opts.Password
	}
	if opts.IncludeAddons {
		cmd["include_all_addons"] = true
	}
	if len(opts.IncludeFolders) > 0 {
		cmd["include_folders"] = opts.IncludeFolders
	}

	var res struct {
		JobID string `json:"backup_job_id"`
	}
	if err := s.call(ctx, cmd, &res); err != nil {
		return "", err
	}
	return res.JobID, nil
}

// WaitCreated blocks until the backup being created completes.  It
// needs a Subscribe before the Generate.
func (s *Session) WaitCreated(ctx context.Context) error {
	for {
		ev, err := s.NextEvent(ctx)
		if err != nil {
			return fmt.Errorf("waiting for the backup: %w", err)
		}
		if ev.ManagerState != "create_backup" {
			continue
		}
		switch ev.State {
		case "completed":
			return nil
		case "failed":
			reason := ev.Reason
			if reason == "" {
				reason = "unknown error"
			}
			return fmt.Errorf("the instance failed to create the backup: %s", reason)
		}
	}
}

// Find returns the backup a Generate produced: core installs name it
// after the job, supervised ones need a lookup by name.
func (s *Session) Find(ctx context.Context, jobID, name string) (*Backup, error) {
	if b, err := s.Get(ctx, jobID); err != nil {
		return nil, err
	} else if b != nil {
		return b, nil
	}

	backups, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	var found *Backup
	for i := range backups {
		b := &backups[i]
		if b.Name != name {
			continue
		}
		if found == nil || b.Date > found.Date {
			found = b
		}
	}
	if found == nil {
		return nil, fmt.Errorf("the instance reports no backup named %q", name)
	}
	return found, nil
}

// ErrRestoreUnconfirmed is returned when the instance dropped the
// connection after being asked to restore, which is what the restart
// it performs looks like from here.
var ErrRestoreUnconfirmed = errors.New("the instance closed the connection before confirming the restore")

// Restore asks the instance to restore a backup it holds.  The
// instance restarts to apply it, which may drop the connection before
// it answers: that is reported as ErrRestoreUnconfirmed.
func (s *Session) Restore(ctx context.Context, opts RestoreOptions) error {
	cmd := map[string]any{
		"type":                  "backup/restore",
		"backup_id":             opts.BackupID,
		"agent_id":              opts.AgentID,
		"restore_database":      true,
		"restore_homeassistant": true,
	}
	if opts.Password != "" {
		cmd["password"] = opts.Password
	}
	if len(opts.Addons) > 0 {
		cmd["restore_addons"] = opts.Addons
	}
	if len(opts.Folders) > 0 {
		cmd["restore_folders"] = opts.Folders
	}

	ctx, cancel := context.WithTimeout(ctx, restoreTimeout)
	defer cancel()

	err := s.call(ctx, cmd, nil)
	if err == nil {
		return nil
	}
	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrRestoreUnconfirmed
	}
	return err
}
