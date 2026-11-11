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

// Package server knows the layout of a Minecraft server directory: its
// server.properties, the RCON settings in it, and the session.lock a
// running server holds on each of its worlds.
package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PasswordEnv names the environment variable that overrides the RCON
// password of server.properties, so that it never shows up on a
// command line.
const PasswordEnv = "MINECRAFT_RCON_PASSWORD"

// Properties is the content of a Java properties file.
type Properties map[string]string

// Server is a server directory and what its server.properties says.
type Server struct {
	Dir string

	// Properties is nil when the directory has no server.properties.
	Properties Properties
}

// Open looks at a server directory.  The directory must exist, the
// server.properties in it may not.
func Open(dir string) (*Server, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", dir)
	}

	props, err := LoadProperties(filepath.Join(dir, "server.properties"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return &Server{Dir: dir, Properties: props}, nil
}

// RconAddr returns the address of the RCON console.  override wins
// when set, otherwise the address comes from server.properties, and
// the second value is false when RCON is disabled there.
func (s *Server) RconAddr(override string) (string, bool) {
	if override != "" {
		return override, true
	}
	if s.Properties["enable-rcon"] != "true" {
		return "", false
	}

	host := s.Properties["server-ip"]
	if host == "" {
		host = "127.0.0.1"
	}
	port := s.Properties["rcon.port"]
	if port == "" {
		port = "25575"
	}
	return net.JoinHostPort(host, port), true
}

// RconPassword is the password of the console, from the environment
// when set there, else from server.properties.
func (s *Server) RconPassword() string {
	if pw, ok := os.LookupEnv(PasswordEnv); ok {
		return pw
	}
	return s.Properties["rcon.password"]
}

// Running reports whether a server process holds the lock on the
// session.lock of one of its worlds.  The lock is taken when the
// server opens the world and released when it exits, however it
// exits.
func (s *Server) Running() (bool, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return false, err
	}

	// the worlds live where level-name says, which may be a path, a
	// Bukkit server adds the nether and the end next to them, and
	// older layouts keep everything at the top level
	var dirs []string
	if level := s.Properties["level-name"]; level != "" {
		dirs = append(dirs, level, level+"_nether", level+"_the_end")
	}
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}

	for _, dir := range dirs {
		lock := filepath.Join(s.Dir, dir, "session.lock")
		held, err := locked(lock)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("%s: %w", lock, err)
		}
		if held {
			return true, nil
		}
	}
	return false, nil
}

// LoadProperties reads a properties file.
func LoadProperties(path string) (Properties, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	props, err := ReadProperties(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return props, nil
}

// ReadProperties parses the java.util.Properties format the server
// writes: key=value lines, # and ! comments, backslash escapes and
// continuation lines.
func ReadProperties(r io.Reader) (Properties, error) {
	props := make(Properties)

	sc := bufio.NewScanner(r)
	var pending string
	continued := false
	for sc.Scan() {
		line := strings.TrimLeft(sc.Text(), " \t\f")
		if !continued && (line == "" || line[0] == '#' || line[0] == '!') {
			continue
		}

		if trailingBackslashes(line)%2 == 1 {
			pending += line[:len(line)-1]
			continued = true
			continue
		}

		key, value := splitKeyValue(pending + line)
		props[key] = value
		pending = ""
		continued = false
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if continued {
		key, value := splitKeyValue(pending)
		props[key] = value
	}
	return props, nil
}

func trailingBackslashes(s string) int {
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\\'; i-- {
		n++
	}
	return n
}

func splitKeyValue(line string) (string, string) {
	var key strings.Builder
	i := 0
	for i < len(line) {
		c := line[i]
		if c == '\\' && i+1 < len(line) {
			key.WriteByte(c)
			key.WriteByte(line[i+1])
			i += 2
			continue
		}
		if c == '=' || c == ':' || c == ' ' || c == '\t' || c == '\f' {
			break
		}
		key.WriteByte(c)
		i++
	}

	rest := strings.TrimLeft(line[i:], " \t\f")
	if rest != "" && (rest[0] == '=' || rest[0] == ':') {
		rest = strings.TrimLeft(rest[1:], " \t\f")
	}
	return unescape(key.String()), unescape(rest)
}

func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}

	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'f':
			b.WriteByte('\f')
		case 'u':
			if i+4 < len(s) {
				if r, err := strconv.ParseUint(s[i+1:i+5], 16, 16); err == nil {
					b.WriteRune(rune(r))
					i += 4
					continue
				}
			}
			b.WriteByte('u')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
