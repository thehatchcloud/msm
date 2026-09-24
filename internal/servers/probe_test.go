package servers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/process"
	"github.com/thehatchcloud/msm/internal/screen"
	"github.com/thehatchcloud/msm/internal/testutil"
)

type fakeProcs map[int][]screen.ProcInfo // pid -> [self, children...]

func (p fakeProcs) Get(pid int) (screen.ProcInfo, error) {
	if procs, ok := p[pid]; ok {
		return procs[0], nil
	}
	return screen.ProcInfo{}, screen.ErrNoProcess
}

func (p fakeProcs) Children(ppid int) ([]screen.ProcInfo, error) {
	if procs, ok := p[ppid]; ok {
		return procs[1:], nil
	}
	return nil, nil
}

func TestScreenProber(t *testing.T) {
	s, err := legacyconf.ResolveServer(legacyconf.ServerInput{Name: "alpha", Dir: "/srv/alpha"})
	if err != nil {
		t.Fatal(err)
	}
	me := identity.Identity{Username: "me", UID: os.Geteuid(), GID: os.Getegid()}
	invocation := strings.Fields(s.Get("INVOCATION"))
	session := "There is a screen on:\n\t42.msm-alpha\t(Detached)\n1 Socket in /tmp/s.\n"
	two := "There are screens on:\n\t42.msm-alpha\t(Detached)\n\t43.msm-alpha\t(Detached)\n2 Sockets in /tmp/s.\n"
	cases := []struct {
		name   string
		screen string
		out    string
		procs  fakeProcs
		want   StateKind
	}{
		{"no screen installed", "", "", nil, Stopped},
		{"no sessions", "/usr/bin/screen", "No Sockets found in /tmp/s.\n\n", nil, Stopped},
		{"inaccessible", "/usr/bin/screen", "Directory '/run/screen' must have mode 777.\n", nil, Unknown},
		{"running", "/usr/bin/screen", session, fakeProcs{42: {{PID: 42, UID: me.UID}, {PID: 50, PPID: 42, UID: me.UID, Argv: invocation}}}, Running},
		{"something else", "/usr/bin/screen", session, fakeProcs{42: {{PID: 42, UID: me.UID}, {PID: 50, PPID: 42, UID: me.UID, Argv: []string{"bash"}}}}, Occupied},
		{"starting", "/usr/bin/screen", session, fakeProcs{42: {{PID: 42, UID: me.UID}}}, Occupied},
		{"duplicate", "/usr/bin/screen", two, nil, Occupied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ScreenProber{
				Screen: tc.screen, Env: []string{}, Runner: testutil.NewRunner(process.Result{Stdout: tc.out}, nil),
				Terminal: process.ExecRunner{}, Processes: tc.procs, Clock: testutil.NewClock(time.Unix(0, 0)),
			}
			if p.Processes == nil {
				p.Processes = fakeProcs{}
			}
			if got := p.Probe(context.Background(), s, me); got.Kind != tc.want {
				t.Fatalf("Probe = %+v, want %s", got, tc.want)
			}
		})
	}
	if os.Geteuid() != 0 {
		other := identity.Identity{Username: "other", UID: os.Geteuid() + 1, GID: 1}
		p := ScreenProber{Screen: "/usr/bin/screen", Env: []string{}, Runner: testutil.NewRunner(process.Result{}, nil),
			Terminal: process.ExecRunner{}, Processes: fakeProcs{}, Clock: testutil.NewClock(time.Unix(0, 0))}
		if got := p.Probe(context.Background(), s, other); got.Kind != Unknown {
			t.Fatalf("probing another user's sessions without root = %+v, want unknown", got)
		}
	}
}
