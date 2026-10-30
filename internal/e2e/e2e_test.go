// Package e2e drives the built msm executable against a real GNU screen and a
// real JVM. The "Minecraft server" is testdata/FakeServer.java, compiled with
// the JDK on the machine and started with "java -jar", so what is exercised is
// the manager, screen, the operating system and the JVM, not Mojang's code
// (docs/validation.md says what that does and does not prove).
//
// The tests only run when MSM_E2E=1, which CI sets: they need screen, a JDK
// and time, and they start real processes. With MSM_E2E=1 a missing
// requirement is a failure, never a skip, so a runner that cannot validate
// stays an explicit blocker. MSM_E2E_BIN and MSM_E2E_JAR name an msm
// executable and a server JAR built earlier (CI builds them once and runs
// the root-user checks from a prebuilt test binary); MSM_E2E_RAMDIR is a
// directory on a RAM volume; MSM_E2E_EVIDENCE is a directory that receives
// the evidence file release.md asks for.
package e2e

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thehatchcloud/msm/internal/evidence"
)

func enabled() bool { return os.Getenv("MSM_E2E") == "1" }

// shared holds what is built once for every test.
var shared struct {
	once              sync.Once
	err               error
	dir, bin, jar     string
	java, screenV, jv string
}

// results are recorded for the evidence file.
var (
	resultsMu sync.Mutex
	results   []evidence.Result
)

func run(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// need fails the test (the suite is enabled, so nothing may be skipped) when
// a requirement is missing.
func need(t *testing.T, program string) string {
	t.Helper()
	p, err := exec.LookPath(program)
	if err != nil {
		t.Fatalf("MSM_E2E=1 but %s is not installed: this runner cannot validate", program)
	}
	return p
}

var noise = regexp.MustCompile(`(?m)^Picked up .*\n`)

// build prepares the executable and the server JAR once.
func build(t *testing.T) {
	t.Helper()
	shared.once.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				shared.err = fmt.Errorf("%v", r)
			}
		}()
		var err error
		if shared.dir, err = os.MkdirTemp("", "msme2e-build"); err != nil {
			shared.err = err
			return
		}
		env := os.Environ()
		shared.bin = os.Getenv("MSM_E2E_BIN")
		if shared.bin == "" {
			shared.bin = filepath.Join(shared.dir, "msm")
			cmd := exec.Command("go", "build", "-trimpath", "-o", shared.bin, "./cmd/msm")
			cmd.Dir = filepath.Join("..", "..")
			cmd.Env = append(env, "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				shared.err = fmt.Errorf("build msm: %v\n%s", err, out)
				return
			}
		}
		shared.jar = os.Getenv("MSM_E2E_JAR")
		if shared.jar == "" {
			javac, err := exec.LookPath("javac")
			if err != nil {
				shared.err = fmt.Errorf("MSM_E2E=1 but javac is not installed (a JDK is required to build the stand-in server)")
				return
			}
			jar, err := exec.LookPath("jar")
			if err != nil {
				shared.err = fmt.Errorf("MSM_E2E=1 but jar is not installed (a JDK is required)")
				return
			}
			src, _ := filepath.Abs(filepath.Join("testdata", "FakeServer.java"))
			if out, err := exec.Command(javac, "--release", "17", "-d", shared.dir, src).CombinedOutput(); err != nil {
				shared.err = fmt.Errorf("javac: %v\n%s", err, out)
				return
			}
			shared.jar = filepath.Join(shared.dir, "server.jar")
			if out, err := exec.Command(jar, "cfe", shared.jar, "FakeServer", "-C", shared.dir, "FakeServer.class").CombinedOutput(); err != nil {
				shared.err = fmt.Errorf("jar: %v\n%s", err, out)
				return
			}
		}
		if out, err := exec.Command("java", "-version").CombinedOutput(); err == nil {
			shared.java = strings.TrimSpace(noise.ReplaceAllString(string(out), ""))
		}
		if out, err := exec.Command("screen", "-v").CombinedOutput(); err == nil {
			shared.screenV = strings.TrimSpace(string(out))
		}
	})
	if shared.err != nil {
		t.Fatal(shared.err)
	}
}

// installation is one isolated manager installation in a temporary
// directory, with its own screen socket directory.
type installation struct {
	t      *testing.T
	dir    string
	conf   string
	env    []string
	owner  string
	screen string
}

type outcome struct {
	code     int
	out, err string
}

func newInstallation(t *testing.T, ramDir string, extraConf string) *installation {
	t.Helper()
	return newInstallationFor(t, os.Getenv("MSM_E2E_USER"), ramDir, extraConf)
}

// newInstallationFor is an installation whose manager user is owner (the
// current user when empty).
func newInstallationFor(t *testing.T, owner, ramDir, extraConf string) *installation {
	t.Helper()
	if !enabled() {
		t.Skip("set MSM_E2E=1 to run the native end-to-end tests")
	}
	need(t, "screen")
	need(t, "java")
	build(t)
	// Not t.TempDir: its parent is private to the user running the test, and
	// the manager may have to act as another user inside the directory.
	base, err := os.MkdirTemp("", "msme2e-inst")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	in := &installation{t: t, dir: base}
	// A short socket directory keeps within the sun_path limit on macOS.
	sock, err := os.MkdirTemp("/tmp", "msme2e")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sock, 0o700); err != nil {
		t.Fatal(err)
	}
	in.screen = sock
	me := owner
	if me == "" {
		me = currentUser(t)
	}
	in.owner = me
	conf := fmt.Sprintf("USERNAME=%q\nDEFAULT_USERNAME=%q\nSERVER_STORAGE_PATH=%q\nJAR_STORAGE_PATH=%q\n"+
		"WORLD_ARCHIVE_PATH=%q\nBACKUP_ARCHIVE_PATH=%q\nLOG_ARCHIVE_PATH=%q\nDEFAULT_RAM=\"128\"\nDEFAULT_STOP_DELAY=\"1\"\n",
		me, me, filepath.Join(in.dir, "servers"), filepath.Join(in.dir, "jars"),
		filepath.Join(in.dir, "archives", "worlds"), filepath.Join(in.dir, "archives", "backups"), filepath.Join(in.dir, "archives", "logs"))
	if ramDir != "" {
		conf += fmt.Sprintf("RAMDISK_STORAGE_ENABLED=\"true\"\nRAMDISK_STORAGE_PATH=%q\n", ramDir)
	}
	conf += extraConf
	for _, d := range []string{"servers", "jars", "archives"} {
		if err := os.MkdirAll(filepath.Join(in.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	in.conf = filepath.Join(in.dir, "msm.conf")
	if err := os.WriteFile(in.conf, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	// Everything the manager creates must belong to the manager user.
	if os.Geteuid() == 0 {
		if err := exec.Command("chown", "-R", me, in.dir).Run(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(sock, uidOf(t, me), -1); err != nil {
			t.Fatal(err)
		}
	}
	in.env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + in.dir, "SCREENDIR=" + sock, "MSM_CONF=" + in.conf,
		"LANG=C.UTF-8", "TERM=vt100", "TMPDIR=" + in.dir}
	t.Cleanup(func() {
		// Stop whatever a failed test left running.
		cmd := exec.Command("screen", "-ls")
		cmd.Env = in.env
		o, _ := cmd.CombinedOutput()
		for _, m := range regexp.MustCompile(`(?m)^\s+(\d+\.\S+)`).FindAllStringSubmatch(string(o), -1) {
			q := exec.Command("screen", "-S", m[1], "-X", "quit")
			q.Env = in.env
			_ = q.Run()
		}
		_ = os.RemoveAll(sock)
	})
	return in
}

func currentUser(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func uidOf(t *testing.T, name string) int {
	t.Helper()
	out, err := exec.Command("id", "-u", name).Output()
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// msm runs the executable and returns its exit status and output.
func (in *installation) msm(args ...string) outcome {
	in.t.Helper()
	return in.msmIn("", args...)
}

func (in *installation) msmIn(stdin string, args ...string) outcome {
	in.t.Helper()
	cmd := exec.Command(shared.bin, args...)
	cmd.Env, cmd.Dir = in.env, in.dir
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		in.t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			in.t.Fatal(err)
		}
		return outcome{code, stdout.String(), stderr.String()}
	case <-time.After(3 * time.Minute):
		_ = cmd.Process.Kill()
		in.t.Fatalf("msm %s did not finish in 3 minutes\nstdout: %s\nstderr: %s", strings.Join(args, " "), stdout.String(), stderr.String())
		return outcome{}
	}
}

func (in *installation) must(args ...string) string {
	in.t.Helper()
	o := in.msm(args...)
	if o.code != 0 {
		in.t.Fatalf("msm %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), o.code, o.out, o.err)
	}
	return o.out
}

// server creates a server running the stand-in JAR.
func (in *installation) server(name string, props string) string {
	in.t.Helper()
	in.must("server", "create", name)
	dir := filepath.Join(in.dir, "servers", name)
	jar, err := os.ReadFile(shared.jar)
	if err != nil {
		in.t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "server.jar")); err != nil && !os.IsNotExist(err) {
		in.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.jar"), jar, 0o644); err != nil {
		in.t.Fatal(err)
	}
	p := "msm-version=minecraft/1.21.0\n" + props
	if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte(p), 0o644); err != nil {
		in.t.Fatal(err)
	}
	// A world the way the legacy layout keeps it: in worldstorage, linked
	// into the server directory by "worlds load", which is where the
	// server writes it.
	world := filepath.Join(dir, "worldstorage", "world")
	if err := os.MkdirAll(world, 0o755); err != nil {
		in.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world, "level.dat"), []byte("new world"), 0o644); err != nil {
		in.t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		_ = exec.Command("chown", "-R", in.owner, dir).Run()
	}
	in.must(name, "worlds", "load")
	return dir
}

// writeWorld has the stand-in write text into its world. The console takes
// the line asynchronously; "save all" is answered only after the lines
// before it, so the write is done when this returns.
func (in *installation) writeWorld(name, text string) {
	in.t.Helper()
	in.must(name, "cmd", "world-write", text)
	in.must(name, "save", "all")
}

func (in *installation) logOf(dir string) string {
	data, _ := os.ReadFile(filepath.Join(dir, "logs", "latest.log"))
	return string(data)
}

func (in *installation) running(name string) bool {
	in.t.Helper()
	out := in.must(name, "status")
	switch {
	case strings.Contains(out, "is running"):
		return true
	case strings.Contains(out, "is stopped"):
		return false
	}
	in.t.Fatalf("unrecognized status output %q", out)
	return false
}

func (in *installation) active(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, "active"))
	return err == nil
}

var pidRE = regexp.MustCompile(`pid=(\d+)`)

// pid is the process ID the stand-in logged at its latest start.
func (in *installation) pid(dir string) int {
	in.t.Helper()
	m := pidRE.FindStringSubmatch(in.logOf(dir))
	if m == nil {
		in.t.Fatalf("no pid in the log:\n%s", in.logOf(dir))
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func eventually(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// scenario runs fn as a named subtest and records it for the evidence file.
func scenario(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	start := time.Now()
	ok := t.Run(name, fn)
	resultsMu.Lock()
	results = append(results, evidence.Result{Name: name, Passed: ok, Seconds: time.Since(start).Seconds()})
	resultsMu.Unlock()
}

func TestMain(m *testing.M) {
	code := m.Run()
	if dir := os.Getenv("MSM_E2E_EVIDENCE"); dir != "" && enabled() && len(results) > 0 {
		writeEvidence(dir)
	}
	if shared.dir != "" {
		_ = os.RemoveAll(shared.dir)
	}
	os.Exit(code)
}

var javaMajor = regexp.MustCompile(`version "(?:1\.)?(\d+)`)

// writeEvidence records what ran: the executable (by checksum, so a release
// check can tie the evidence to the exact release executable), the JVM,
// screen, and every scenario's outcome.
func writeEvidence(dir string) {
	f := evidence.File{Schema: 1, OS: runtime.GOOS, Arch: runtime.GOARCH, Go: runtime.Version(),
		Java: shared.java, Screen: shared.screenV, Commit: os.Getenv("MSM_E2E_COMMIT"),
		Time: time.Now().UTC().Format(time.RFC3339), Root: os.Geteuid() == 0, Results: results}
	if m := javaMajor.FindStringSubmatch(shared.java); m != nil {
		f.JavaMajor, _ = strconv.Atoi(m[1])
	}
	if data, err := os.ReadFile(shared.bin); err == nil {
		sum := sha256.Sum256(data)
		f.BinarySHA256 = hex.EncodeToString(sum[:])
	}
	if out, err := exec.Command(shared.bin, "version").Output(); err == nil {
		f.Version = strings.TrimSpace(string(out))
	}
	data, _ := json.MarshalIndent(f, "", "  ")
	name := fmt.Sprintf("e2e-%s-%s-java%d", f.OS, f.Arch, f.JavaMajor)
	if f.Root {
		name += "-root"
	}
	if err := os.MkdirAll(dir, 0o755); err == nil {
		_ = os.WriteFile(filepath.Join(dir, name+".json"), append(data, '\n'), 0o644)
	}
}

// ---- scenarios ----

func TestLifecycle(t *testing.T) {
	in := newInstallation(t, "", "")
	dir := in.server("alpha", "")

	scenario(t, "start", func(t *testing.T) {
		in.must("alpha", "start")
		if !in.running("alpha") {
			t.Fatal("not running after start")
		}
		if !in.active(dir) {
			t.Error("start did not mark the server active")
		}
		if !strings.Contains(in.logOf(dir), "java=") {
			t.Errorf("the log does not show a JVM:\n%s", in.logOf(dir))
		}
		if o := in.msm("alpha", "start"); o.code != 0 || !strings.Contains(o.out, "already running") {
			t.Errorf("a second start = %+v", o)
		}
	})

	scenario(t, "commands", func(t *testing.T) {
		in.must("alpha", "say", "hello from the end-to-end test")
		eventually(t, "the say to reach the log", 10*time.Second, func() bool {
			return strings.Contains(in.logOf(dir), "[Server] hello from the end-to-end test")
		})
		in.writeWorld("alpha", "progress-1")
		if !strings.Contains(in.logOf(dir), "Saved the game") {
			t.Error("save all was not confirmed in the log")
		}
		in.must("alpha", "whitelist", "on")
		if o := in.msm("alpha", "connected"); o.code != 0 {
			t.Errorf("connected = %+v", o)
		}
	})

	scenario(t, "attach-detach", func(t *testing.T) {
		primary, replica, err := openPTY()
		if err != nil {
			t.Fatal(err)
		}
		defer primary.Close()
		cmd := exec.Command(shared.bin, "alpha", "console")
		cmd.Env, cmd.Dir = in.env, in.dir
		cmd.Stdin, cmd.Stdout, cmd.Stderr = replica, replica, replica
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		replica.Close()
		var screenText bytes.Buffer
		go func() { _, _ = io.Copy(&screenText, primary) }()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		defer func() { _ = cmd.Process.Kill() }()

		// Typing into the attached console reaches the server; Ctrl-A d
		// detaches and leaves it running.
		time.Sleep(1500 * time.Millisecond)
		_, _ = primary.Write([]byte("say typed on the console\r"))
		eventually(t, "the typed line to reach the log", 15*time.Second, func() bool {
			return strings.Contains(in.logOf(dir), "[Server] typed on the console")
		})
		_, _ = primary.Write([]byte{0x01, 'd'})
		select {
		case err := <-done:
			if err != nil {
				t.Logf("console exited: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("Ctrl-A d did not detach; screen output:\n%s", screenText.String())
		}
		if !in.running("alpha") {
			t.Fatal("the server stopped when the console detached")
		}
		in.must("alpha", "say", "still here after detach")
		eventually(t, "the server to answer after the detach", 10*time.Second, func() bool {
			return strings.Contains(in.logOf(dir), "still here after detach")
		})
	})

	scenario(t, "graceful-stop", func(t *testing.T) {
		in.must("alpha", "stop")
		log := in.logOf(dir)
		warn := strings.Index(log, "SERVER SHUTTING DOWN IN 1 SECONDS!")
		save := strings.LastIndex(log, "Saved the game")
		stop := strings.Index(log, "Stopping server")
		if warn < 0 || save < warn || stop < save {
			t.Errorf("stop did not warn, then save, then stop:\n%s", log)
		}
		if in.running("alpha") {
			t.Error("still running after stop")
		}
		if in.active(dir) {
			t.Error("an explicit stop left the server active")
		}
		if o := in.msm("alpha", "stop"); o.code != 0 || !strings.Contains(o.out, "is not running") {
			t.Errorf("stopping a stopped server = %+v", o)
		}
	})

	scenario(t, "backup-and-restore", func(t *testing.T) {
		// The server is stopped and holds "progress-1". Back it up, play
		// on, then restore the backup into a new directory as the
		// documentation describes (a manual unzip).
		in.must("alpha", "backup")
		archives, _ := filepath.Glob(filepath.Join(in.dir, "archives", "backups", "alpha", "*.zip"))
		if len(archives) != 1 {
			t.Fatalf("complete backups = %v", archives)
		}
		in.must("alpha", "worlds", "backup")
		world, _ := filepath.Glob(filepath.Join(in.dir, "archives", "worlds", "alpha", "*", "*.zip"))
		if len(world) == 0 {
			t.Error("no world backup was written")
		}
		in.must("alpha", "start")
		in.writeWorld("alpha", "progress-2-after-the-backup")
		in.must("alpha", "stop")
		restored := filepath.Join(in.dir, "restored")
		unzip(t, archives[0], restored)
		got, err := os.ReadFile(filepath.Join(restored, "alpha", "worldstorage", "world", "level.dat"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "progress-1" {
			t.Errorf("restored world = %q, want the backed-up progress-1", got)
		}
		now, _ := os.ReadFile(filepath.Join(dir, "worldstorage", "world", "level.dat"))
		if string(now) != "progress-2-after-the-backup" {
			t.Errorf("the live world = %q", now)
		}
	})
}

func unzip(t *testing.T, archive, dest string) {
	t.Helper()
	r, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, f := range r.File {
		path := filepath.Join(dest, filepath.FromSlash(f.Name))
		if rel, err := filepath.Rel(dest, path); err != nil || strings.HasPrefix(rel, "..") {
			t.Fatalf("entry %q leaves the restore directory", f.Name)
		}
		switch {
		case f.FileInfo().IsDir():
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
		case f.Mode()&os.ModeSymlink != 0:
			// A link is restored as a link; the test reads regular files.
		default:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestGlobalIntentAndCrashRecovery(t *testing.T) {
	in := newInstallation(t, "", "")
	a := in.server("alpha", "")
	b := in.server("beta", "")

	scenario(t, "global-intent", func(t *testing.T) {
		in.must("alpha", "start")
		in.must("beta", "start")
		// A global stop (what the boot service does at shutdown) stops
		// everything but leaves the intent: both come back at the next start.
		in.must("stop")
		if in.running("alpha") || in.running("beta") {
			t.Fatal("a server survived the global stop")
		}
		if !in.active(a) || !in.active(b) {
			t.Fatalf("the global stop changed the intent: alpha active=%v beta active=%v", in.active(a), in.active(b))
		}
		in.must("start")
		if !in.running("alpha") || !in.running("beta") {
			t.Fatal("the global start did not bring the active servers back")
		}
		// An explicit stop of one clears its intent.
		in.must("alpha", "stop")
		in.must("stop")
		in.must("start")
		if in.running("alpha") || !in.running("beta") {
			t.Errorf("after alpha was stopped on purpose: alpha running=%v beta running=%v", in.running("alpha"), in.running("beta"))
		}
	})

	scenario(t, "crash-recovery", func(t *testing.T) {
		// beta is running and active. Kill the JVM the way the OOM killer
		// would; the hourly recover job brings it back, and does not start
		// the server that was stopped on purpose.
		old := in.pid(b)
		if err := syscall.Kill(old, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		eventually(t, "screen to notice the crash", 20*time.Second, func() bool { return !in.running("beta") })
		if !in.active(b) {
			t.Error("a crash cleared the intent")
		}
		in.must("maintenance", "run", "recover")
		if !in.running("beta") {
			t.Fatal("the recover job did not restart the crashed server")
		}
		if in.pid(b) == old {
			t.Error("the restarted server has the old process ID")
		}
		if in.running("alpha") {
			t.Error("the recover job started a server that was stopped on purpose")
		}
		in.must("stop")
	})
}

func TestRAMWorlds(t *testing.T) {
	if !enabled() {
		t.Skip("set MSM_E2E=1 to run the native end-to-end tests")
	}
	ram := os.Getenv("MSM_E2E_RAMDIR")
	if ram == "" {
		t.Fatal("MSM_E2E=1 but MSM_E2E_RAMDIR (a directory on a RAM volume) is not set: this runner cannot validate RAM worlds")
	}
	// A directory of its own on the RAM volume, removed afterwards: a copy a
	// failed run left behind would (rightly) be refused as an unknown RAM copy.
	if err := os.MkdirAll(ram, 0o755); err != nil {
		t.Fatal(err)
	}
	ram, err := os.MkdirTemp(ram, "run")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ram, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ram) })
	in := newInstallation(t, ram, "")
	dir := in.server("ramsrv", "")
	scenario(t, "ram-sync", func(t *testing.T) {
		in.must("ramsrv", "worlds", "ram", "world")
		in.must("ramsrv", "start")
		in.writeWorld("ramsrv", "played-in-ram")
		inRAM := filepath.Join(ram, "ramsrv", "world", "level.dat")
		data, err := os.ReadFile(inRAM)
		if err != nil || string(data) != "played-in-ram" {
			t.Fatalf("the world is not served from RAM: %q, %v", data, err)
		}
		disk := filepath.Join(dir, "worldstorage", "world", "level.dat")
		if got, _ := os.ReadFile(disk); string(got) == "played-in-ram" {
			t.Fatal("the disk copy already has the RAM progress, so nothing proves the sync")
		}
		in.must("all", "worlds", "todisk")
		if got, _ := os.ReadFile(disk); string(got) != "played-in-ram" {
			t.Errorf("todisk left the disk copy at %q", got)
		}
		// A stop writes RAM to disk too.
		in.writeWorld("ramsrv", "last-words")
		in.must("ramsrv", "stop")
		if got, _ := os.ReadFile(disk); string(got) != "last-words" {
			t.Errorf("stop left the disk copy at %q", got)
		}
		in.must("ramsrv", "worlds", "ram", "world")
		if _, err := os.Stat(filepath.Join(ram, "ramsrv", "world")); err == nil {
			t.Error("the RAM copy was not removed when the flag was cleared")
		}
	})
}

func TestUserPermissions(t *testing.T) {
	if !enabled() {
		t.Skip("set MSM_E2E=1 to run the native end-to-end tests")
	}
	if os.Geteuid() == 0 {
		// As root the manager acts as the configured owner.
		other := os.Getenv("MSM_E2E_USER")
		if other == "" {
			other = "nobody"
		}
		in := newInstallationFor(t, other, "", "")
		scenario(t, "root-acts-as-the-owner", func(t *testing.T) {
			dir := in.server("owned", "")
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if uid := int(info.Sys().(*syscall.Stat_t).Uid); uid != uidOf(t, other) {
				t.Errorf("the server directory belongs to uid %d, want %s (%d)", uid, other, uidOf(t, other))
			}
			in.must("owned", "start")
			if !in.running("owned") {
				t.Fatal("not running")
			}
			out, _ := exec.Command("ps", "-o", "user=", "-p", strconv.Itoa(in.pid(dir))).Output()
			if got := strings.TrimSpace(string(out)); got != other && !strings.HasPrefix(other, got) {
				t.Errorf("the server runs as %q, want %q", got, other)
			}
			in.must("owned", "stop")
		})
		return
	}
	// As anyone else, a command for an installation owned by another user
	// is refused and changes nothing.
	in := newInstallation(t, "", "")
	scenario(t, "other-user-is-refused", func(t *testing.T) {
		in.must("server", "create", "mine")
		// Rewrite the configuration so another account owns the installation.
		data, _ := os.ReadFile(in.conf)
		other := "root"
		if in.owner == "root" {
			other = "nobody"
		}
		changed := strings.ReplaceAll(string(data), fmt.Sprintf("USERNAME=%q", in.owner), fmt.Sprintf("USERNAME=%q", other))
		if err := os.WriteFile(in.conf, []byte(changed), 0o644); err != nil {
			t.Fatal(err)
		}
		o := in.msm("mine", "start")
		if o.code != 5 {
			t.Errorf("start as the wrong user = exit %d, want 5 (refused); stderr %q", o.code, o.err)
		}
		if _, err := os.Lstat(filepath.Join(in.dir, "servers", "mine", "active")); err == nil {
			t.Error("a refused start still marked the server active")
		}
		if o := in.msm("server", "delete", "--yes", "mine"); o.code == 0 {
			t.Error("deleting another user's server succeeded")
		}
	})
}
