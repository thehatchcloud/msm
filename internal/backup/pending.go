package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thehatchcloud/msm/internal/atomicfile"
)

// pendingName is the durable marker a backup keeps in the server directory
// while it runs. It exists so that a manager killed with saving disabled
// (SIGKILL, power loss, an OOM kill) can be recovered by the next backup,
// which holds the server's lock and so knows the marker's writer is gone.
// Complete backups leave the marker out of the archive.
const pendingName = ".msm-backup-pending"

// pending is the marker's contents.
type pending struct {
	Version   int       `json:"version"`
	Operation string    `json:"operation"`
	Server    string    `json:"server"`
	PID       int       `json:"pid"`
	Started   time.Time `json:"started"`
	// SavingOff is true from just before the first save-off is sent until
	// the save-on is confirmed.
	SavingOff bool `json:"saving_off"`
}

func pendingPath(serverDir string) string { return filepath.Join(serverDir, pendingName) }

func writePending(serverDir string, p pending) error {
	p.Version = 1
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(pendingPath(serverDir), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("record the pending backup: %w", err)
	}
	return nil
}

func removePending(serverDir string) error {
	if err := os.Remove(pendingPath(serverDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the pending-backup marker: %w", err)
	}
	return nil
}

// readPending returns the marker, or ok=false when there is none. A marker
// that cannot be understood is returned as one with SavingOff set: saving
// may have been left off, and re-enabling it is harmless.
func readPending(serverDir string) (p pending, ok bool, err error) {
	data, err := os.ReadFile(pendingPath(serverDir))
	if errors.Is(err, fs.ErrNotExist) {
		return pending{}, false, nil
	}
	if err != nil {
		return pending{}, false, fmt.Errorf("read the pending-backup marker: %w", err)
	}
	if json.Unmarshal(data, &p) != nil || p.Version != 1 {
		return pending{Operation: "unknown", SavingOff: true}, true, nil
	}
	return p, true, nil
}

// sweep removes what an interrupted backup left in an archive directory:
// the staging directory, unfinished archives and unfinished snapshots. It removes only names this
// package creates, below the directories the settings name; it never acts
// on a path read from the marker, which the server's owner can write. base
// is the server's archive directory; world archive directories are its
// children.
func sweep(base string) (removed []string, err error) {
	var errs []error
	try := func(path string, all bool) {
		var rerr error
		if all {
			rerr = os.RemoveAll(path)
		} else {
			rerr = os.Remove(path)
		}
		if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			errs = append(errs, rerr)
			return
		}
		removed = append(removed, path)
	}
	entries, rerr := os.ReadDir(base)
	if errors.Is(rerr, fs.ErrNotExist) {
		return nil, nil
	}
	if rerr != nil {
		return nil, rerr
	}
	for _, e := range entries {
		path := filepath.Join(base, e.Name())
		switch {
		case strings.HasPrefix(e.Name(), stagingPrefix):
			makeRemovable(path)
			try(path, true)
		case snapshotPartial.MatchString(e.Name()) && e.IsDir():
			makeRemovable(path)
			try(path, true)
		case strings.HasPrefix(e.Name(), tmpPrefix) && strings.HasSuffix(e.Name(), tmpSuffix):
			try(path, false)
		case e.IsDir() && e.Type()&fs.ModeSymlink == 0:
			subs, serr := os.ReadDir(path)
			if serr != nil {
				continue
			}
			for _, s := range subs {
				sub := filepath.Join(path, s.Name())
				switch {
				case snapshotPartial.MatchString(s.Name()) && s.IsDir():
					makeRemovable(sub)
					try(sub, true)
				case strings.HasPrefix(s.Name(), tmpPrefix) && strings.HasSuffix(s.Name(), tmpSuffix) && !s.IsDir():
					try(sub, false)
				}
			}
		}
	}
	return removed, errors.Join(errs...)
}

const stagingPrefix = tmpPrefix + "staging-"
