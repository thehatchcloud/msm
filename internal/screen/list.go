package screen

import (
	"strconv"
	"strings"
)

// SocketState is screen's own description of one socket in `screen -ls`.
type SocketState string

const (
	Detached SocketState = "Detached"
	Attached SocketState = "Attached"
	// Dead is a socket whose screen process no longer exists ("Dead ???").
	// It is reported, never wiped or reused automatically.
	Dead SocketState = "Dead"
	// Unreachable covers every other status screen can print for a socket
	// it cannot talk to ("Remote or dead", "Unreachable", ...). The PID may
	// now belong to an unrelated process, so such a socket is never adopted.
	Unreachable SocketState = "Unreachable"
)

// Session is one socket from `screen -ls`, identified by "<pid>.<name>".
type Session struct {
	ID    string
	PID   int
	Name  string
	State SocketState
}

// Live reports whether screen can still talk to the session.
func (s Session) Live() bool { return s.State == Detached || s.State == Attached }

// parseList reads `screen -ls` output. Screen 4.x and 5.x print one tab-
// indented line per socket: the "<pid>.<name>" ID, an optional start time,
// then a parenthesized status. Header, footer and blank lines are ignored.
// Screen's exit status is not used: 4.x returns 1 when there are no
// sockets, and versions disagree when there are.
func parseList(out string) []Session {
	var sessions []Session
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "\t") {
			continue
		}
		fields := strings.Split(strings.TrimSpace(line), "\t")
		pidText, name, ok := strings.Cut(fields[0], ".")
		pid, err := strconv.Atoi(pidText)
		if !ok || err != nil || pid <= 0 || name == "" {
			continue
		}
		sessions = append(sessions, Session{
			ID: fields[0], PID: pid, Name: name,
			State: parseState(fields[len(fields)-1]),
		})
	}
	return sessions
}

func parseState(field string) SocketState {
	status := strings.TrimSuffix(strings.TrimPrefix(field, "("), ")")
	switch {
	case strings.HasPrefix(status, "Dead"):
		return Dead
	// "Multi, attached" and "Private" variants still name the attach state.
	case strings.Contains(strings.ToLower(status), "detached"):
		return Detached
	case strings.Contains(strings.ToLower(status), "attached"):
		return Attached
	default:
		return Unreachable
	}
}
