package servers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/process"
	"github.com/thehatchcloud/msm/internal/screen"
)

// StateKind is what the manager can prove about a server's process.
type StateKind int

const (
	// Stopped: no live screen session has the server's SCREEN_NAME.
	Stopped StateKind = iota
	// Running: the session runs the server's configured invocation.
	Running
	// Occupied: a live session has the server's SCREEN_NAME, but it is
	// starting, exiting, owned by someone else or running something other
	// than the configured invocation. It is treated as running.
	Occupied
	// Unknown: the manager could not inspect the sessions, for example
	// because it runs as a user other than the server's owner.
	Unknown
)

func (k StateKind) String() string {
	return [...]string{"stopped", "running", "occupied", "unknown"}[k]
}

// State is one observation of a server's process.
type State struct {
	Kind   StateKind
	Detail string
}

// Prober reports whether a server's process is running. It must not
// return Stopped unless it has positively observed that no live session
// holds the server's SCREEN_NAME.
type Prober interface {
	Probe(ctx context.Context, s *legacyconf.ServerSettings, owner identity.Identity) State
}

// ScreenProber inspects GNU screen sessions through internal/screen.
type ScreenProber struct {
	// Screen is the absolute path of the screen executable. Empty means
	// screen is not installed, so no server can be running in it.
	Screen    string
	Env       []string
	Runner    process.Runner
	Terminal  process.Terminal
	Processes screen.ProcessTable
	Clock     clock.Clock
}

// Probe maps a screen observation onto a State. The expected invocation is
// INVOCATION split on white space; P06 replaces this with its launch-argument
// parser. A mismatch never reads as Stopped, only as Occupied, so an
// imprecise split can make a check stricter but never unsafe.
func (p ScreenProber) Probe(ctx context.Context, s *legacyconf.ServerSettings, owner identity.Identity) State {
	if p.Screen == "" {
		return State{Kind: Stopped, Detail: "screen is not installed"}
	}
	backend, err := screen.New(screen.Config{
		Screen: p.Screen, Owner: owner, Env: p.Env, Runner: p.Runner,
		Terminal: p.Terminal, Processes: p.Processes, Clock: p.Clock,
	})
	if err != nil {
		return State{Kind: Unknown, Detail: err.Error()}
	}
	st, err := backend.Status(ctx, s.Get("SCREEN_NAME"), strings.Fields(s.Get("INVOCATION")))
	switch {
	case errors.Is(err, screen.ErrDuplicate):
		return State{Kind: Occupied, Detail: err.Error()}
	case err != nil:
		return State{Kind: Unknown, Detail: err.Error()}
	}
	switch st.Liveness {
	case screen.Stopped:
		return State{Kind: Stopped}
	case screen.Running:
		return State{Kind: Running, Detail: fmt.Sprintf("screen session %s", st.Session.ID)}
	default:
		return State{Kind: Occupied, Detail: fmt.Sprintf("screen session %s is %s: %s", st.Session.ID, st.Liveness, st.Detail)}
	}
}
