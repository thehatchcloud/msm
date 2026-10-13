package console

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/thehatchcloud/msm/internal/logwatch"
	"github.com/thehatchcloud/msm/internal/screen"
)

// Connected lists the players on a server (msm <server> connected). A
// stopped server has none. The answer to the profile's list command is
// read from the log: a header line, and the names on it or on the line
// after it.
func (m *Manager) Connected(ctx context.Context, name string, out Output) error {
	lock, s, err := m.open(name, out)
	if err != nil {
		return err
	}
	defer lock.Release()
	if s.sessions == nil {
		return fmt.Errorf("%w: server %q cannot be inspected", ErrNoScreen, s.name)
	}
	st, err := s.observe(ctx)
	if err != nil {
		return err
	}
	if st.Session == nil {
		s.printf("Server %q is not running. No users are connected.", s.name)
		return nil
	}
	players, err := m.players(ctx, s, st)
	if err != nil {
		return err
	}
	if len(players) == 0 {
		s.printf("No players are connected.")
	} else {
		s.printf("Connected players: %s", strings.Join(players, ", "))
	}
	return nil
}

func (m *Manager) players(ctx context.Context, s *server, st screen.Status) ([]string, error) {
	p, err := s.profile.Prepare("CONNECTED", nil)
	if err != nil {
		return nil, fmt.Errorf("server %q (%s): %w", s.name, s.profile.Name, err)
	}
	format := s.profile.Connected
	header, err := s.profile.Match("(" + format.Header + ")(.*)")
	if err != nil {
		return nil, err
	}
	body, err := s.profile.Body()
	if err != nil {
		return nil, err
	}
	log, err := logwatch.Watch(s.settings.Get("LOG_PATH"))
	if err != nil {
		return nil, fmt.Errorf("server %q: %w", s.name, err)
	}
	defer log.Close()
	if err := s.sessions.Send(ctx, st, p.Line); err != nil {
		return nil, fmt.Errorf("server %q: %w", s.name, err)
	}
	timeout := m.answerTimeout(p.Timeout, true)
	deadline := m.cfg.Clock.Now().Add(timeout)
	awaitNames := false
	for {
		lines, err := log.Lines()
		if err != nil {
			return nil, fmt.Errorf("server %q: %w", s.name, err)
		}
		for _, l := range lines {
			if awaitNames {
				if b := body.FindStringSubmatch(l); b != nil {
					return splitNames(b[len(b)-1]), nil
				}
				continue
			}
			if h := header.FindStringSubmatch(l); h != nil {
				if !format.NextLine {
					return splitNames(h[len(h)-1]), nil
				}
				awaitNames = true
			}
		}
		if !m.cfg.Clock.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: server %q: no player list in %s (see %s)", ErrUnconfirmed, s.name, timeout, s.settings.Get("LOG_PATH"))
		}
		if err := m.cfg.Clock.Wait(ctx, logPoll); err != nil {
			return nil, fmt.Errorf("%w: server %q: the player list was not awaited", ctx.Err(), s.name)
		}
	}
}

var nameSeparator = regexp.MustCompile(`\s*,\s*`)

// splitNames splits "a, b, c" into names, ignoring blanks and control
// characters.
func splitNames(list string) []string {
	var names []string
	for _, n := range nameSeparator.Split(clean(strings.TrimSpace(list)), -1) {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}
