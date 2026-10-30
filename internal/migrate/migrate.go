package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thehatchcloud/msm/internal/atomicfile"
	"github.com/thehatchcloud/msm/internal/filelock"
	"github.com/thehatchcloud/msm/internal/install"
)

var (
	// ErrBlocked: the preflight found something that must be fixed first.
	ErrBlocked = errors.New("migrate: the preflight found blockers")
	// ErrMigrated: a migration is already recorded.
	ErrMigrated = errors.New("migrate: this installation was already migrated")
	// ErrInterrupted: an earlier run did not finish; it must be rolled back.
	ErrInterrupted = errors.New("migrate: an earlier run did not finish")
	// ErrNoMigration: nothing is recorded to roll back.
	ErrNoMigration = errors.New("migrate: there is no migration to roll back")
	// ErrNotStopped: a server is running or cannot be proven stopped.
	ErrNotStopped = errors.New("migrate: the maintenance window is not stopped")
	// ErrNoBackup: apply was not told how the server data is backed up.
	ErrNoBackup = errors.New("migrate: no data backup")
	// ErrPrivilege: the files are root's.
	ErrPrivilege = errors.New("migrate: this needs root")
	// ErrUnsupported: this system has no legacy installation to migrate.
	ErrUnsupported = errors.New("migrate: unsupported on this system")
)

// Commands runs the external programs a migration needs: systemctl, and
// nothing else. They are started with fixed arguments, without a shell.
type Commands interface {
	// Have reports whether the program exists.
	Have(name string) bool
	// Run runs it and returns its combined output.
	Run(name string, args ...string) (string, error)
}

// Journal is the record of a migration, written to the state directory
// before and after every step so an interrupted run can be rolled back.
type Journal struct {
	Version int          `json:"version"`
	ID      string       `json:"id"`
	Phase   string       `json:"phase"`
	Started string       `json:"started"`
	Layout  Layout       `json:"layout"`
	Steps   []StepRecord `json:"steps"`
	Backup  []BackupFile `json:"backup"`
	Facts   Facts        `json:"facts"`
}

// StepRecord is one step's progress: "started", "done" or "undone".
type StepRecord struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Facts are what the steps learned and wrote, which rollback and the
// verification read back.
type Facts struct {
	ServiceKnown      bool                   `json:"service_known"`
	ServiceWasEnabled bool                   `json:"service_was_enabled"`
	InitMode          uint32                 `json:"init_mode"`
	Servers           []string               `json:"servers"`
	ActiveServers     []string               `json:"active_servers"`
	Fingerprints      map[string]Fingerprint `json:"fingerprints"`
	Written           map[string]string      `json:"written"`
	DataBackups       []string               `json:"data_backups,omitempty"`
	CreatedDirs       []string               `json:"created_dirs,omitempty"`
	ExternalBackup    bool                   `json:"external_backup,omitempty"`
}

// Phases of a journal.
const (
	PhaseApplying    = "applying"
	PhaseApplied     = "applied"
	PhaseRollingBack = "rolling-back"
	PhaseRolledBack  = "rolled-back"
)

func journalPath(l Layout) string { return filepath.Join(l.P(l.StateDir), "journal.json") }
func lockPath(l Layout) string    { return filepath.Join(l.P(l.StateDir), "lock") }

// ReadJournal loads the journal; ErrNoMigration if there is none.
func ReadJournal(l Layout) (*Journal, error) {
	data, err := os.ReadFile(journalPath(l))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoMigration
	}
	if err != nil {
		return nil, err
	}
	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("migrate: %s is damaged: %w", journalPath(l), err)
	}
	return &j, nil
}

func (j *Journal) save() error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(journalPath(j.Layout), append(data, '\n'), 0o600)
}

func (j *Journal) mark(name, status string) error {
	for i := range j.Steps {
		if j.Steps[i].Name == name {
			j.Steps[i].Status = status
			return j.save()
		}
	}
	j.Steps = append(j.Steps, StepRecord{name, status})
	return j.save()
}

func (j *Journal) status(name string) string {
	for _, s := range j.Steps {
		if s.Name == name {
			return s.Status
		}
	}
	return ""
}

// Options is how apply and rollback run.
type Options struct {
	Env      Env
	Commands Commands
	// Binary is the Go executable to install (apply).
	Binary string
	// DataBackup makes a complete backup of one stopped server before
	// anything changes; ExternalBackup says the administrator has made
	// their own and checked it. Apply needs one of them.
	DataBackup     func(ctx context.Context, server string, out io.Writer) error
	ExternalBackup bool
	// AcceptUnsupported accepts losing the legacy cron lines that cannot be
	// translated (they stay in the backup and as comments).
	AcceptUnsupported bool
	// DryRun reports the plan and changes nothing.
	DryRun bool
	// EUID is the effective user ID.
	EUID int
	Out  io.Writer
	Now  func() time.Time
}

func (o *Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Options) acks() map[string]bool {
	if o.AcceptUnsupported {
		return map[string]bool{CodeUnsupportedSchedule: true}
	}
	return nil
}

func (o *Options) printf(format string, args ...any) {
	if o.Out != nil {
		fmt.Fprintf(o.Out, format, args...)
	}
}

func (o *Options) needRoot() error {
	if o.EUID != 0 && o.Env.Layout.Root == "" {
		return fmt.Errorf("%w: it changes /etc and the services; run it with sudo, or rehearse on a copy with --root", ErrPrivilege)
	}
	return nil
}

// run is one apply or rollback in progress.
type run struct {
	o   *Options
	l   Layout
	inv *Inventory
	j   *Journal
}

type step struct {
	name string
	desc string
	do   func(context.Context, *run) error
	undo func(context.Context, *run) error
}

func steps() []step {
	return []step{
		{"backup", "copy the legacy control files (configuration, cron file, init script, link, unit) into the state directory and check the copies", doBackup, nil},
		{"fingerprint", "record a fingerprint of every world directory, to prove later that nothing wrote to it", doFingerprint, nil},
		{"data-backup", "make a complete backup of every stopped server (or record that you made your own)", doDataBackup, nil},
		{"disable-scheduler", "remove the legacy cron file, so cron stops running the legacy manager", doDisableScheduler, undoDisableScheduler},
		{"disable-service", "disable the legacy boot service", doDisableService, undoDisableService},
		{"disable-executable", "make the legacy init script non-executable and remove the msm command link", doDisableExecutable, undoDisableExecutable},
		{"verify-legacy-disabled", "check that the legacy scheduler, service and executable are all out of action", doVerifyDisabled, nil},
		{"install-binary", "install the Go executable where the msm command was", doInstallBinary, undoInstallBinary},
		{"install-schedule", "write the Go cron file from the legacy one", doInstallSchedule, undoInstallSchedule},
		{"install-service", "write and enable the Go service (it is not started)", doInstallService, undoInstallService},
		{"verify", "check the Go installation sees the same configuration, servers, active flags and untouched worlds", doVerify, nil},
	}
}

// Plan describes what Apply would do.
type Plan struct {
	Inventory *Inventory
	Schedule  Schedule
	Steps     []string
}

// Preflight collects the inventory and the plan without changing anything.
func Preflight(ctx context.Context, o Options) (*Plan, error) {
	inv, err := Collect(ctx, o.Env)
	if err != nil {
		return nil, err
	}
	p := &Plan{Inventory: inv}
	p.Schedule = BuildSchedule(inv.Cron, o.Env.Layout.GoBinary, o.Env.Layout.Conf)
	for i, s := range steps() {
		p.Steps = append(p.Steps, fmt.Sprintf("%d. %s: %s", i+1, s.name, s.desc))
	}
	return p, nil
}

// Apply migrates the installation. It stops at the first problem and, once it
// has changed anything, rolls back what it did before returning the error.
func Apply(ctx context.Context, o Options) (err error) {
	l := o.Env.Layout
	if err := o.needRoot(); err != nil {
		return err
	}
	if old, err := ReadJournal(l); err == nil {
		if err := refuseUnlessRolledBack(old); err != nil {
			return err
		}
	} else if !errors.Is(err, ErrNoMigration) {
		return err
	}
	inv, err := preflightForApply(ctx, &o)
	if err != nil {
		return err
	}
	if !o.ExternalBackup && o.DataBackup == nil {
		return fmt.Errorf("%w: pass --backup-servers to have msm make a complete backup of every server, or --external-backup after making and checking your own", ErrNoBackup)
	}
	if o.Binary == "" {
		return errors.New("migrate: the Go executable to install is not named")
	}
	if o.DryRun {
		return nil
	}

	if err := os.MkdirAll(l.P(l.StateDir), 0o700); err != nil {
		return fmt.Errorf("migrate: create %s: %w", l.StateDir, err)
	}
	lock, err := filelock.TryAcquire(lockPath(l))
	if errors.Is(err, filelock.ErrBusy) {
		return fmt.Errorf("migrate: another migration is running: %w", err)
	}
	if err != nil {
		return err
	}
	defer lock.Release()

	// Checked again under the lock: another run may have finished meanwhile,
	// and a server may have been started since the first look.
	if inv, err = preflightForApply(ctx, &o); err != nil {
		return err
	}
	if old, err := ReadJournal(l); err == nil {
		if err := refuseUnlessRolledBack(old); err != nil {
			return err
		}
		if err := archiveJournal(l, old); err != nil {
			return err
		}
	} else if !errors.Is(err, ErrNoMigration) {
		return err
	}

	r := &run{o: &o, l: l, inv: inv, j: &Journal{
		Version: 1, ID: o.now().UTC().Format("20060102T150405Z"), Phase: PhaseApplying,
		Started: o.now().UTC().Format(time.RFC3339), Layout: l,
		Facts: Facts{Written: map[string]string{}, ExternalBackup: o.ExternalBackup},
	}}
	if err := r.j.save(); err != nil {
		return err
	}
	for _, s := range steps() {
		o.printf("%s ...\n", s.name)
		if err := r.j.mark(s.name, "started"); err != nil {
			return r.fail(ctx, err)
		}
		if err := s.do(ctx, r); err != nil {
			return r.fail(ctx, fmt.Errorf("%s: %w", s.name, err))
		}
		if err := r.j.mark(s.name, "done"); err != nil {
			return r.fail(ctx, err)
		}
	}
	r.j.Phase = PhaseApplied
	if err := r.j.save(); err != nil {
		return err
	}
	o.printf("Migrated. Nothing was started: start the servers with \"msm start\" or \"systemctl start msm\" when the window ends.\n")
	return nil
}

func preflightForApply(ctx context.Context, o *Options) (*Inventory, error) {
	inv, err := Collect(ctx, o.Env)
	if err != nil {
		return nil, err
	}
	if blockers := inv.Blocked(o.acks()); len(blockers) > 0 {
		return nil, fmt.Errorf("%w: %d; run \"msm migrate preflight\" to read them", ErrBlocked, len(blockers))
	}
	return inv, nil
}

// stoppedWindow refuses unless every server is proven stopped and the
// configuration that says which servers exist can be read.
func stoppedWindow(ctx context.Context, env Env) (*Inventory, error) {
	inv, err := Collect(ctx, env)
	if err != nil {
		return nil, err
	}
	var stuck []string
	for _, f := range inv.Findings {
		if f.Severity == Blocker && (f.Area == "screen" || f.Area == "config") {
			stuck = append(stuck, f.Subject+": "+f.Message)
		}
	}
	if len(stuck) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotStopped, strings.Join(stuck, "; "))
	}
	return inv, nil
}

// refuseUnlessRolledBack: a recorded migration blocks a new one until it is
// rolled back.
func refuseUnlessRolledBack(j *Journal) error {
	switch j.Phase {
	case PhaseRolledBack:
		return nil
	case PhaseApplied:
		return fmt.Errorf("%w (%s); roll it back first to run it again", ErrMigrated, j.ID)
	}
	return fmt.Errorf("%w (%s, %s): run \"msm migrate rollback\"", ErrInterrupted, j.ID, j.Phase)
}

// fail undoes what was done and returns cause, noting if the undo itself failed.
func (r *run) fail(ctx context.Context, cause error) error {
	r.o.printf("failed: %v\nRolling back what was done ...\n", cause)
	if err := r.undoAll(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("%w; and the automatic rollback failed: %v; run \"msm migrate rollback\" to finish it", cause, err)
	}
	r.j.Phase = PhaseRolledBack
	_ = r.j.save()
	return fmt.Errorf("%w; everything it changed was restored", cause)
}

// undoAll undoes every started step, newest first.
func (r *run) undoAll(ctx context.Context) error {
	all := steps()
	var errs []error
	for i := len(all) - 1; i >= 0; i-- {
		s := all[i]
		st := r.j.status(s.name)
		if st != "started" && st != "done" {
			continue
		}
		if s.undo != nil {
			r.o.printf("undo %s ...\n", s.name)
			if err := s.undo(ctx, r); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", s.name, err))
				continue
			}
		}
		if err := r.j.mark(s.name, "undone"); err != nil {
			errs = append(errs, err)
		}
	}
	for i := len(r.j.Facts.CreatedDirs) - 1; i >= 0; i-- {
		// Only directories apply made, and only while they are empty.
		_ = os.Remove(r.l.P(r.j.Facts.CreatedDirs[i]))
	}
	return errors.Join(errs...)
}

// mkdirAll creates logical and its missing parents, remembering which it
// made so rollback can remove exactly those.
func (r *run) mkdirAll(logical string) error {
	var missing []string
	for d := logical; d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Stat(r.l.P(d)); err == nil {
			break
		}
		missing = append([]string{d}, missing...)
	}
	for _, d := range missing {
		if err := os.Mkdir(r.l.P(d), 0o755); err != nil {
			return err
		}
		r.j.Facts.CreatedDirs = append(r.j.Facts.CreatedDirs, d)
		// Saved at once: a crash before the step ends must still be able to
		// remove what the step made.
		if err := r.j.save(); err != nil {
			return err
		}
	}
	return nil
}

// Rollback restores the legacy control files from the backup. It needs the
// same stopped window as apply, takes the same lock, and never writes inside
// a server directory.
func Rollback(ctx context.Context, o Options) error {
	l := o.Env.Layout
	if err := o.needRoot(); err != nil {
		return err
	}
	j, err := ReadJournal(l)
	if err != nil {
		return err
	}
	if j.Phase == PhaseRolledBack {
		return fmt.Errorf("%w: %s was already rolled back", ErrNoMigration, j.ID)
	}
	// The journal's layout is the one apply used, so the rollback restores
	// the files apply changed even if the flags differ now.
	jl := j.Layout
	jl.Root = l.Root
	jl.StateDir = l.StateDir
	j.Layout = jl
	env := o.Env
	env.Layout = jl
	if _, err := stoppedWindow(ctx, env); err != nil {
		return err
	}
	if o.DryRun {
		return nil
	}
	lock, err := filelock.TryAcquire(lockPath(jl))
	if err != nil {
		return fmt.Errorf("migrate: another migration is running: %w", err)
	}
	defer lock.Release()
	inv, err := stoppedWindow(ctx, env)
	if err != nil {
		return err
	}

	r := &run{o: &o, l: jl, inv: inv, j: j}
	j.Phase = PhaseRollingBack
	if err := j.save(); err != nil {
		return err
	}
	if err := r.undoAll(ctx); err != nil {
		return err
	}
	// The configuration is read by both managers and was never changed by
	// apply; if something else changed it, put the backed-up one back.
	if err := restoreChanged(r, jl.Conf); err != nil {
		return err
	}
	for _, p := range []string{jl.Profile, jl.Completion} {
		if err := restoreChanged(r, p); err != nil {
			return err
		}
	}
	j.Phase = PhaseRolledBack
	if err := j.save(); err != nil {
		return err
	}
	o.printf("Rolled back. The legacy manager's cron file, service and command are back; world data was not touched. Copies of what was removed are in %s.\n", jl.savedDir())
	return nil
}

// restoreChanged restores logical from the backup when it differs from it.
func restoreChanged(r *run, logical string) error {
	bf, ok := findBackup(r.j.Backup, logical)
	if !ok {
		return nil
	}
	phys := r.l.P(logical)
	switch bf.Kind {
	case "file":
		if data, err := os.ReadFile(phys); err == nil && hashBytes(data) == bf.SHA256 {
			return nil
		}
	case "absent":
		if _, err := os.Lstat(phys); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
	default:
		return nil
	}
	r.o.printf("restoring %s, which changed during the migration\n", logical)
	return restore(r.l, bf, r.o.EUID)
}

func archiveJournal(l Layout, j *Journal) error {
	dir := filepath.Join(l.P(l.StateDir), "history", j.ID)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	for _, name := range []string{"backup", "saved"} {
		src := filepath.Join(l.P(l.StateDir), name)
		if _, err := os.Stat(src); err == nil {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			if err := os.Rename(src, filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Rename(journalPath(l), filepath.Join(dir, "journal.json"))
}

// ---- steps ----

func doBackup(_ context.Context, r *run) error {
	files, err := backupControl(r.l)
	if err != nil {
		return err
	}
	r.j.Backup = files
	if err := verifyBackup(r.l, files); err != nil {
		return err
	}
	if bf, ok := findBackup(files, r.l.Init); ok && bf.Kind == "file" {
		r.j.Facts.InitMode = bf.Mode
	}
	inv, err := json.MarshalIndent(r.inv, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(r.l.backupDir(), "inventory.json"), append(inv, '\n'), 0o600)
}

func doFingerprint(_ context.Context, r *run) error {
	fp, err := FingerprintWorlds(r.l, r.inv)
	if err != nil {
		return err
	}
	r.j.Facts.Fingerprints = fp
	for _, s := range r.inv.Servers {
		r.j.Facts.Servers = append(r.j.Facts.Servers, s.Name)
		if s.Active {
			r.j.Facts.ActiveServers = append(r.j.Facts.ActiveServers, s.Name)
		}
	}
	return nil
}

func doDataBackup(ctx context.Context, r *run) error {
	if r.o.DataBackup == nil {
		r.o.printf("  skipped: you said you made and checked your own backup\n")
		return nil
	}
	for _, s := range r.inv.Servers {
		r.o.printf("  backing up server %s\n", s.Name)
		if err := r.o.DataBackup(ctx, s.Name, r.o.Out); err != nil {
			return fmt.Errorf("complete backup of %s: %w", s.Name, err)
		}
		r.j.Facts.DataBackups = append(r.j.Facts.DataBackups, s.Name)
	}
	return nil
}

func doDisableScheduler(_ context.Context, r *run) error {
	bf, ok := findBackup(r.j.Backup, r.l.Cron)
	if !ok || bf.Kind == "absent" {
		return nil
	}
	if err := os.Remove(r.l.P(r.l.Cron)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func undoDisableScheduler(_ context.Context, r *run) error {
	bf, ok := findBackup(r.j.Backup, r.l.Cron)
	if !ok || bf.Kind == "absent" {
		return nil
	}
	return restore(r.l, bf, r.o.EUID)
}

const serviceName = "msm.service"

func doDisableService(_ context.Context, r *run) error {
	c := r.o.Commands
	if c == nil || !c.Have("systemctl") {
		r.o.printf("  no systemctl: the legacy init script is made non-executable below, which stops a SysV runlevel script from starting it\n")
		return nil
	}
	if out, err := c.Run("systemctl", "is-enabled", serviceName); err == nil {
		r.j.Facts.ServiceKnown = true
		r.j.Facts.ServiceWasEnabled = strings.TrimSpace(out) == "enabled" || strings.TrimSpace(out) == "enabled-runtime"
	}
	unit, haveUnit := findBackup(r.j.Backup, r.l.LegacyUnit)
	if r.j.Facts.ServiceWasEnabled {
		if _, err := c.Run("systemctl", "disable", serviceName); err != nil {
			return err
		}
	}
	if haveUnit && unit.Kind == "file" {
		if err := os.Remove(r.l.P(r.l.LegacyUnit)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if _, err := c.Run("systemctl", "daemon-reload"); err != nil {
			return err
		}
	}
	return nil
}

func undoDisableService(_ context.Context, r *run) error {
	c := r.o.Commands
	unit, haveUnit := findBackup(r.j.Backup, r.l.LegacyUnit)
	var errs []error
	if haveUnit && unit.Kind == "file" {
		if err := restore(r.l, unit, r.o.EUID); err != nil {
			errs = append(errs, err)
		}
	}
	if c != nil && c.Have("systemctl") {
		if haveUnit && unit.Kind == "file" {
			if _, err := c.Run("systemctl", "daemon-reload"); err != nil {
				errs = append(errs, err)
			}
		}
		if r.j.Facts.ServiceWasEnabled {
			if _, err := c.Run("systemctl", "enable", serviceName); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func doDisableExecutable(_ context.Context, r *run) error {
	if bf, ok := findBackup(r.j.Backup, r.l.Init); ok && bf.Kind == "file" {
		if err := os.Chmod(r.l.P(r.l.Init), fs.FileMode(bf.Mode)&^0o111); err != nil {
			return err
		}
	}
	if bf, ok := findBackup(r.j.Backup, r.l.Command); ok && bf.Kind != "absent" {
		if err := os.Remove(r.l.P(r.l.Command)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func undoDisableExecutable(_ context.Context, r *run) error {
	var errs []error
	if bf, ok := findBackup(r.j.Backup, r.l.Init); ok && bf.Kind == "file" {
		if err := os.Chmod(r.l.P(r.l.Init), fs.FileMode(bf.Mode)); err != nil {
			errs = append(errs, err)
		}
	}
	// The command link is restored by undoInstallBinary, which runs first
	// and puts back whatever was at the Go executable's path; when the Go
	// executable was never installed, restore the link here.
	if bf, ok := findBackup(r.j.Backup, r.l.Command); ok && bf.Kind == "symlink" {
		if _, err := os.Lstat(r.l.P(r.l.Command)); err != nil {
			if err := restore(r.l, bf, r.o.EUID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// doVerifyDisabled is the point of no concurrent writers: the Go manager is
// installed only after the legacy one can no longer be run by cron, by the
// boot service, or by name.
func doVerifyDisabled(_ context.Context, r *run) error {
	var left []string
	if _, err := os.Lstat(r.l.P(r.l.Cron)); err == nil {
		left = append(left, r.l.Cron+" still exists")
	}
	if info, err := os.Lstat(r.l.P(r.l.Init)); err == nil && info.Mode()&0o111 != 0 {
		left = append(left, r.l.Init+" is still executable")
	}
	if bf, ok := findBackup(r.j.Backup, r.l.LegacyUnit); ok && bf.Kind == "file" && r.l.LegacyUnit != r.l.GoUnit {
		if _, err := os.Lstat(r.l.P(r.l.LegacyUnit)); err == nil {
			left = append(left, r.l.LegacyUnit+" still exists")
		}
	}
	if _, err := os.Lstat(r.l.P(r.l.Command)); err == nil {
		if bf, ok := findBackup(r.j.Backup, r.l.Command); ok && bf.Kind != "absent" {
			left = append(left, r.l.Command+" still exists")
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("the legacy manager is not disabled (%s); the Go manager was not installed", strings.Join(left, "; "))
	}
	return nil
}

func doInstallBinary(_ context.Context, r *run) error {
	data, err := os.ReadFile(r.o.Binary)
	if err != nil {
		return fmt.Errorf("read the Go executable: %w", err)
	}
	if len(data) < 4 || string(data[:2]) == "#!" {
		return fmt.Errorf("%s looks like a script, not the Go executable", r.o.Binary)
	}
	dest := r.l.P(r.l.GoBinary)
	if info, err := os.Lstat(dest); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s exists and is not a regular file", r.l.GoBinary)
		}
		if old, err := os.ReadFile(dest); err != nil || hashBytes(old) != hashBytes(data) {
			return fmt.Errorf("%w: %s already exists with different content", install.ErrExists, r.l.GoBinary)
		}
	}
	if err := r.mkdirAll(filepath.Dir(r.l.GoBinary)); err != nil {
		return err
	}
	if err := atomicfile.Write(dest, data, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(dest, 0o755); err != nil {
		return err
	}
	r.j.Facts.Written[r.l.GoBinary] = hashBytes(data)
	return nil
}

func undoInstallBinary(_ context.Context, r *run) error {
	phys := r.l.P(r.l.GoBinary)
	if info, err := os.Lstat(phys); err == nil && info.Mode().IsRegular() {
		if err := saveAside(r.l, r.l.GoBinary); err != nil {
			return err
		}
		if err := os.Remove(phys); err != nil {
			return err
		}
	}
	if bf, ok := findBackup(r.j.Backup, r.l.GoBinary); ok && bf.Kind == "symlink" {
		return restore(r.l, bf, r.o.EUID)
	}
	return nil
}

func doInstallSchedule(_ context.Context, r *run) error {
	if len(r.inv.Cron) == 0 {
		r.o.printf("  there was no legacy cron file; no scheduler was written\n")
		return nil
	}
	s := BuildSchedule(r.inv.Cron, r.l.GoBinary, r.l.Conf)
	// Both end up in a cron file, which cron reads line by line.
	if err := install.CheckPath("msm executable", r.l.GoBinary); err != nil {
		return err
	}
	if err := install.CheckPath("configuration file", r.l.Conf); err != nil {
		return err
	}
	for _, n := range s.Notes {
		r.o.printf("  note: %s\n", n)
	}
	for _, d := range s.Dropped {
		r.o.printf("  NOT MIGRATED (line %d): %s\n", d.Line, d.Raw)
	}
	if len(s.Dropped) > 0 && !r.o.AcceptUnsupported {
		return errors.New("the legacy cron file has lines that cannot be migrated")
	}
	if err := atomicfile.Write(r.l.P(r.l.GoCron), []byte(s.Text), 0o644); err != nil {
		return err
	}
	if err := os.Chmod(r.l.P(r.l.GoCron), 0o644); err != nil {
		return err
	}
	if r.o.EUID == 0 {
		if err := os.Lchown(r.l.P(r.l.GoCron), 0, 0); err != nil {
			return err
		}
	}
	r.j.Facts.Written[r.l.GoCron] = hashBytes([]byte(s.Text))
	return nil
}

func undoInstallSchedule(_ context.Context, r *run) error {
	phys := r.l.P(r.l.GoCron)
	if _, err := os.Lstat(phys); err != nil {
		return nil
	}
	if err := saveAside(r.l, r.l.GoCron); err != nil {
		return err
	}
	return os.Remove(phys)
}

func (r *run) serviceOptions() install.ServiceOptions {
	o := install.ServiceOptions{Binary: r.l.GoBinary}
	if r.l.Conf != "/etc/msm.conf" {
		o.Conf = r.l.Conf
	}
	return o
}

func doInstallService(_ context.Context, r *run) error {
	text, _, enable, err := install.Definition("linux", r.serviceOptions())
	if err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(r.l.GoUnit)); err != nil {
		return err
	}
	var run install.Runner
	if c := r.o.Commands; c != nil && c.Have("systemctl") {
		run = func(name string, args ...string) error { _, err := c.Run(name, args...); return err }
	} else {
		r.o.printf("  no systemctl: the unit was written but could not be enabled; enable it with your init system\n")
	}
	if _, err := install.WriteService(text, r.l.P(r.l.GoUnit), false, enable, run); err != nil {
		return err
	}
	r.j.Facts.Written[r.l.GoUnit] = hashBytes([]byte(text))
	return nil
}

func undoInstallService(_ context.Context, r *run) error {
	var errs []error
	c := r.o.Commands
	phys := r.l.P(r.l.GoUnit)
	_, statErr := os.Lstat(phys)
	if c != nil && c.Have("systemctl") && statErr == nil {
		if _, err := c.Run("systemctl", "disable", serviceName); err != nil {
			errs = append(errs, err)
		}
	}
	if statErr == nil {
		if err := saveAside(r.l, r.l.GoUnit); err != nil {
			errs = append(errs, err)
		} else if err := os.Remove(phys); err != nil {
			errs = append(errs, err)
		}
		if c != nil && c.Have("systemctl") {
			if _, err := c.Run("systemctl", "daemon-reload"); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// doVerify checks the result from the files, the way the Go manager will
// read them. It does not start anything.
func doVerify(ctx context.Context, r *run) error {
	var problems []string
	env := r.o.Env
	after, err := collectForVerify(ctx, env, r.inv)
	if err != nil {
		return err
	}
	if got := names(after.Servers); strings.Join(got, ",") != strings.Join(r.j.Facts.Servers, ",") {
		problems = append(problems, fmt.Sprintf("servers changed from %v to %v", r.j.Facts.Servers, got))
	}
	var active []string
	for _, s := range after.Servers {
		if s.Active {
			active = append(active, s.Name)
		}
	}
	if strings.Join(active, ",") != strings.Join(r.j.Facts.ActiveServers, ",") {
		problems = append(problems, fmt.Sprintf("active servers changed from %v to %v", r.j.Facts.ActiveServers, active))
	}
	fp, err := FingerprintWorlds(r.l, after)
	if err != nil {
		return err
	}
	for name, before := range r.j.Facts.Fingerprints {
		if fp[name] != before {
			problems = append(problems, fmt.Sprintf("the worlds of %s changed (fingerprint differs)", name))
		}
	}
	for logical, want := range r.j.Facts.Written {
		data, err := os.ReadFile(r.l.P(logical))
		if err != nil || hashBytes(data) != want {
			problems = append(problems, logical+" is not what was written")
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// collectForVerify re-reads the installation without the preflight's checks
// that no longer apply once the Go files are in place.
func collectForVerify(ctx context.Context, env Env, before *Inventory) (*Inventory, error) {
	inv, err := Collect(ctx, env)
	if err != nil {
		return nil, err
	}
	for _, f := range inv.Findings {
		if f.Severity == Blocker && f.Area == "config" {
			return nil, fmt.Errorf("the configuration no longer loads: %s", f.Message)
		}
	}
	if inv.ManagerUser != before.ManagerUser || inv.ServerRoot != before.ServerRoot {
		return nil, errors.New("the configuration reads differently after the migration")
	}
	return inv, nil
}

func names(s []ServerInfo) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.Name
	}
	return out
}
