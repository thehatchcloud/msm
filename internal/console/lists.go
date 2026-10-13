package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// The list commands read the files Minecraft keeps. They never write
// them: while a server runs it owns them, and a change goes through a game
// command. Both formats are read: the JSON arrays of Minecraft 1.7.6 and
// later, and the one-name-per-line text of earlier versions.

// Whitelist prints the whitelisted players (msm <server> whitelist list).
func (m *Manager) Whitelist(name string, out Output) error {
	s, names, err := m.readList(name, "WHITELIST_PATH", out)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		s.printf("No players are whitelisted.")
		return nil
	}
	s.printPlain(names)
	return nil
}

// Operators prints the operators (msm <server> operator list).
func (m *Manager) Operators(name string, out Output) error {
	s, names, err := m.readList(name, "OPS_PATH", out)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		s.printf("No players are operators.")
		return nil
	}
	s.printPlain(names)
	return nil
}

// Blacklist prints the banned players and IP addresses (msm <server>
// blacklist list).
func (m *Manager) Blacklist(name string, out Output) error {
	s, players, err := m.readList(name, "BANNED_PLAYERS_PATH", out)
	if err != nil {
		return err
	}
	ips, err := readNames(s.settings.Get("BANNED_IPS_PATH"), "ip")
	if err != nil {
		return fmt.Errorf("server %q: %w", s.name, err)
	}
	if len(players) == 0 && len(ips) == 0 {
		s.printf("The blacklist is empty.")
		return nil
	}
	if len(players) > 0 {
		s.printf("Players:")
		for _, n := range players {
			s.printf("    %s", n)
		}
	}
	if len(ips) > 0 {
		s.printf("IP Addresses:")
		for _, n := range ips {
			s.printf("    %s", n)
		}
	}
	return nil
}

func (s *server) printPlain(names []string) {
	for _, n := range names {
		s.printf("%s", n)
	}
}

func (m *Manager) readList(name, setting string, out Output) (*server, []string, error) {
	set, err := m.cfg.Servers.Settings(name)
	if err != nil {
		return nil, nil, err
	}
	s := &server{name: set.Name, dir: set.Dir, settings: set, out: out}
	key := "name"
	names, err := readNames(set.Get(setting), key)
	if err != nil {
		return nil, nil, fmt.Errorf("server %q: %w", s.name, err)
	}
	return s, names, nil
}

// readNames reads a list file, which may not exist (an empty list). key is
// the JSON field that holds the entry ("name", or "ip" for banned IPs).
func readNames(path, key string) ([]string, error) {
	data, err := readList(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if trimmed := strings.TrimSpace(string(data)); strings.HasPrefix(trimmed, "[") {
		var entries []map[string]any
		if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
			return nil, fmt.Errorf("%w: %s is not a valid list (it may be mid-write): %v", ErrBadList, path, err)
		}
		var names []string
		for _, e := range entries {
			if v, ok := e[key].(string); ok && v != "" {
				names = append(names, clean(v))
			}
		}
		return names, nil
	}
	// Legacy text: one entry per line, # comments and blank lines ignored.
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line != "" && !strings.HasPrefix(line, "#") {
			names = append(names, clean(line))
		}
	}
	return names, nil
}

// readList reads a regular file of bounded size. O_NONBLOCK keeps a FIFO
// put in its place from hanging the command.
func readList(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrBadList, path)
	}
	if info.Size() > maxList {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrBadList, path, maxList)
	}
	return io.ReadAll(io.LimitReader(f, maxList))
}
