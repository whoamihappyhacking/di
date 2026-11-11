package main

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// isolateRestore gives a test its own session and state directories.
func isolateRestore(t *testing.T) (sessions, records string) {
	t.Helper()
	run, err := os.MkdirTemp("", "di-r-") // short: socket paths have a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(run) })
	t.Setenv("XDG_RUNTIME_DIR", run)
	t.Setenv("XDG_STATE_HOME", filepath.Join(run, "state"))
	sessions = filepath.Join(run, "di")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	records, _ = restoreDir()
	return sessions, records
}

// startRecordedServer starts a server the way d does, with session metadata
// in place, so it keeps a restore record.
func startRecordedServer(t *testing.T, args ...string) *testServer {
	t.Helper()
	sessions, _ := isolateRestore(t)
	s := &testServer{sock: filepath.Join(sessions, "rec-aaaaaaaaaa-1.sock"), done: make(chan error, 1)}
	if err := writeSessionMeta(s.sock, args); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DI_INITIAL_ROWS", "12")
	t.Setenv("DI_INITIAL_COLS", "60")
	go func() { s.done <- runServer(append([]string{s.sock}, args...)) }()
	if err := waitSocket(s.sock, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	return s
}

func recordFor(t *testing.T, sock string) string {
	t.Helper()
	path, err := restorePath(sessionName(sock))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func waitNoFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s is still there", path)
}

func TestServerKeepsRestoreRecordWhileCommandRuns(t *testing.T) {
	s := startRecordedServer(t, "sh", "-c", `read -r x; exit 0`)
	path := recordFor(t, s.sock)
	rec, err := readRestoreRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Sock != s.sock || rec.BootID != bootID() || rec.Adopted || strings.Join(rec.Command, " ") != "sh -c read -r x; exit 0" || rec.PWD == "" {
		t.Fatalf("record = %+v", rec)
	}
	if st, err := querySession(s.sock, 0, 0); err != nil || !st.Restore {
		t.Fatalf("server must say it keeps its own record: %+v %v", st, err)
	}
	if list := collectInterrupted(); len(list) != 0 {
		t.Fatalf("a running session is not interrupted: %+v", list)
	}

	c := attachTestClient(t, s.sock, 12, 60)
	c.readUntil(t, "\x1b[?2026l")
	c.send(t, "done\r")
	c.readToEnd(t)
	s.waitExit(t)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("record kept after the command ended")
	}
}

func TestServerDropsRestoreRecordWhenExitIsUnseen(t *testing.T) {
	s := startRecordedServer(t, "sh", "-c", `read -r x; exit 4`)
	path := recordFor(t, s.sock)
	c := attachTestClient(t, s.sock, 12, 60)
	c.readUntil(t, "\x1b[?2026l")
	c.send(t, "go\r")
	c.conn.Close()
	// The server lingers to show the exit, but a reboot now must not bring
	// the finished command back.
	waitNoFile(t, path, 5*time.Second)
	if st, _ := querySession(s.sock, 0, 0); st.State != "exited" {
		t.Fatalf("state = %+v", st)
	}
	viewer := attachTestClient(t, s.sock, 12, 60)
	viewer.readToEnd(t)
	s.waitExit(t)
}

func TestServerKillDropsRestoreRecord(t *testing.T) {
	s := startRecordedServer(t, "sh", "-c", `printf 'up\n'; sleep 30`)
	path := recordFor(t, s.sock)
	c := attachTestClient(t, s.sock, 12, 60)
	c.readUntil(t, "up")
	if err := killSession(s.sock); err != nil {
		t.Fatal(err)
	}
	c.readToEnd(t)
	s.waitExit(t)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("record kept after the session was killed")
	}
}

func writeRecordFile(t *testing.T, dir, name string, rec restoreRecord, mtime time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec.Name = name
	data, _ := json.Marshal(rec)
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCollectInterrupted(t *testing.T) {
	sessions, records := isolateRestore(t)
	now := time.Now()
	cmd := sessionMeta{Command: []string{"claude", "--dangerously-skip-permissions"}, PWD: "/srv/proj"}
	gone := func(name string) string { return filepath.Join(sessions, name+".sock") }

	live := gone("live-aaaaaaaaaa-1")
	ln, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	writeRecordFile(t, records, "live-aaaaaaaaaa-1", restoreRecord{sessionMeta: cmd, Sock: live, BootID: bootID()}, now)
	rebooted := writeRecordFile(t, records, "rebooted-aaaaaaaaaa-2", restoreRecord{sessionMeta: cmd, Sock: gone("rebooted-aaaaaaaaaa-2"), BootID: "an-earlier-boot"}, now.Add(-2*time.Hour))
	crashed := writeRecordFile(t, records, "crashed-aaaaaaaaaa-3", restoreRecord{sessionMeta: cmd, Sock: gone("crashed-aaaaaaaaaa-3"), BootID: bootID()}, now.Add(-time.Minute))
	adoptedOld := writeRecordFile(t, records, "adopted-aaaaaaaaaa-4", restoreRecord{sessionMeta: cmd, Sock: gone("adopted-aaaaaaaaaa-4"), BootID: "an-earlier-boot", Adopted: true}, now.Add(-time.Hour))
	adoptedNow := writeRecordFile(t, records, "ended-aaaaaaaaaa-5", restoreRecord{sessionMeta: cmd, Sock: gone("ended-aaaaaaaaaa-5"), BootID: bootID(), Adopted: true}, now)
	expired := writeRecordFile(t, records, "expired-aaaaaaaaaa-6", restoreRecord{sessionMeta: cmd, Sock: gone("expired-aaaaaaaaaa-6"), BootID: "an-earlier-boot"}, now.Add(-8*24*time.Hour))
	broken := filepath.Join(records, "broken.json")
	_ = os.WriteFile(broken, []byte("{"), 0o600)

	list := collectInterrupted()
	var got []string
	for _, e := range list {
		got = append(got, e.name()+"="+e.Status.Summary)
		if e.Status.State != "interrupted" || e.Meta.PWD != "/srv/proj" {
			t.Errorf("entry = %+v", e)
		}
	}
	want := "crashed-aaaaaaaaaa-3=会话进程意外结束,adopted-aaaaaaaaaa-4=系统重启前在运行,rebooted-aaaaaaaaaa-2=系统重启前在运行"
	if strings.Join(got, ",") != want {
		t.Fatalf("interrupted (most recent first) =\n  %v\nwant\n  %v", got, want)
	}
	for _, path := range []string{adoptedNow, expired, broken} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s should have been cleared", filepath.Base(path))
		}
	}
	for _, path := range []string{rebooted, crashed, adoptedOld} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s should be kept: %v", filepath.Base(path), err)
		}
	}

	// Dismissing from the picker removes the record.
	if err := confirmKillSession(rebooted); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rebooted); !os.IsNotExist(err) {
		t.Fatal("Ctrl-X did not dismiss the interrupted session")
	}
	if line := list[0].columns(100, false, now); !strings.Contains(line, "已中断") || !strings.Contains(line, "claude") {
		t.Errorf("picker line = %q", line)
	}
}

// fakeOlderServer answers status queries like a server from a build that
// keeps no restore record.
func fakeOlderServer(t *testing.T, sock, state string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _, _ = readFrame(conn)
			_ = writeFrame(conn, frameReply, []byte(`{"state":"`+state+`"}`))
			_ = conn.Close()
		}
	}()
}

func TestSessionsFromOlderBuildsAreAdopted(t *testing.T) {
	sessions, _ := isolateRestore(t)
	sock := filepath.Join(sessions, "codex---yolo-aaaaaaaaaa-1.sock")
	if err := writeSessionMeta(sock, []string{"codex", "--yolo"}); err != nil {
		t.Fatal(err)
	}
	fakeOlderServer(t, sock, "idle")
	if _, err := collectSessions(""); err != nil {
		t.Fatal(err)
	}
	path := recordFor(t, sock)
	rec, err := readRestoreRecord(path)
	if err != nil || !rec.Adopted || rec.Sock != sock || rec.BootID != bootID() || rec.Command[0] != "codex" {
		t.Fatalf("adopted record = %+v %v", rec, err)
	}

	// Its server going away in the same boot says nothing about why.
	_ = os.Remove(sock)
	list, _ := collectSessions("")
	if len(list) != 0 {
		t.Fatalf("list = %+v", list)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stale adopted record kept")
	}
}

func TestAdoptedRecordGoesWhenOlderSessionExits(t *testing.T) {
	sessions, _ := isolateRestore(t)
	sock := filepath.Join(sessions, "make-aaaaaaaaaa-1.sock")
	_ = writeSessionMeta(sock, []string{"make"})
	path := recordFor(t, sock)
	writeRecordFile(t, filepath.Dir(path), sessionName(sock), restoreRecord{sessionMeta: sessionMeta{Command: []string{"make"}}, Sock: sock, Adopted: true}, time.Now())
	fakeOlderServer(t, sock, "exited")
	if _, err := collectSessions(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("record kept for a command that has ended")
	}
}

func TestMatchSessionName(t *testing.T) {
	names := []string{"claude-aaaaaaaaaa-1", "claude-bbbbbbbbbb-2", "codex---yolo-cccccccccc-3"}
	if got, err := matchSessionName(names, "codex"); err != nil || got != names[2] {
		t.Fatalf("prefix: %q %v", got, err)
	}
	if got, err := matchSessionName(names, "claude-bbbbbbbbbb-2"); err != nil || got != names[1] {
		t.Fatalf("exact: %q %v", got, err)
	}
	if _, err := matchSessionName(names, "claude"); err == nil || !strings.Contains(err.Error(), "several") {
		t.Fatalf("ambiguous: %v", err)
	}
	if _, err := matchSessionName(names, "nope"); err == nil || err.Error() != `d: no session matches "nope"` {
		t.Fatalf("no match: %v", err)
	}
}

// TestEndToEndRestoreAfterShutdown drives the real binary through what a
// system shutdown does to a session, then restores it.
func TestEndToEndRestoreAfterShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and drives the real binary")
	}
	tmp, err := os.MkdirTemp("", "di-e2e-r-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	bin := filepath.Join(tmp, "bin", "d")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	sessions := filepath.Join(tmp, "di")
	records := filepath.Join(tmp, "state", "di", "restore")
	defer func() {
		socks, _ := filepath.Glob(filepath.Join(sessions, "*.sock"))
		for _, sock := range socks {
			_ = exec.Command(bin, "--kill", sock).Run()
		}
	}()
	work := filepath.Join(tmp, "work")
	_ = os.MkdirAll(work, 0o700)
	env := append(os.Environ(), "XDG_RUNTIME_DIR="+tmp, "XDG_STATE_HOME="+filepath.Join(tmp, "state"), "SHELL=/bin/sh")
	d := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env, cmd.Dir = env, dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("d %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	// The command writes where it runs, so a restore in the wrong directory shows.
	script := `echo started >> ran; exec sleep 60`
	if out := d(work, "sh", "-c", script); !strings.Contains(out, "in the background") {
		t.Fatalf("start: %s", out)
	}
	socks, _ := filepath.Glob(filepath.Join(sessions, "*.sock"))
	if len(socks) != 1 {
		t.Fatalf("sockets = %v", socks)
	}
	sock := socks[0]
	name := sessionName(sock)
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	var childPID int
	waitFor("the command to start", func() bool {
		st, err := querySession(sock, 0, 0)
		childPID = st.PID
		return err == nil && st.PID > 0
	})
	server, _ := legacyServerProcesses(sock)
	if server <= 0 {
		t.Fatal("server process not found")
	}

	// A shutdown signals every process at once, and the command may well be
	// gone before the server hears of it.
	_ = syscall.Kill(-childPID, syscall.SIGTERM)
	time.Sleep(50 * time.Millisecond)
	_ = syscall.Kill(server, syscall.SIGTERM)
	waitFor("the server to go", func() bool { return !isSocket(sock) && syscall.Kill(server, 0) != nil })
	if _, err := os.Stat(filepath.Join(records, name+".json")); err != nil {
		t.Fatalf("record lost on shutdown: %v", err)
	}
	list := d(tmp, "--list")
	if !strings.Contains(list, "已中断") || !strings.Contains(list, name) {
		t.Fatalf("list after shutdown:\n%s", list)
	}

	if out := d(tmp, "--restore", "sh"); !strings.Contains(out, "in the background") {
		t.Fatalf("restore: %s", out)
	}
	waitFor("the command to run again in its directory", func() bool {
		b, _ := os.ReadFile(filepath.Join(work, "ran"))
		return string(b) == "started\nstarted\n"
	})
	if _, err := os.Stat(filepath.Join(records, name+".json")); !os.IsNotExist(err) {
		t.Fatal("old record kept after restoring")
	}
	if list := d(tmp, "--list"); strings.Contains(list, "已中断") || strings.Count(list, "\n") != 1 {
		t.Fatalf("list after restore:\n%s", list)
	}

	// Ending the restored session leaves nothing to restore; neither does
	// dismissing an interrupted one.
	d(tmp, "--kill", "sh")
	waitFor("records to clear", func() bool {
		left, _ := filepath.Glob(filepath.Join(records, "*.json"))
		return len(left) == 0
	})
	if out := d(work, "sh", "-c", script); !strings.Contains(out, "in the background") {
		t.Fatalf("start: %s", out)
	}
	socks, _ = filepath.Glob(filepath.Join(sessions, "*.sock"))
	server, _ = legacyServerProcesses(socks[0])
	_ = syscall.Kill(server, syscall.SIGKILL)
	waitFor("the killed server's session to show as interrupted", func() bool {
		return strings.Contains(d(tmp, "--list"), "会话进程意外结束")
	})
	d(tmp, "--kill", "sh")
	if left, _ := filepath.Glob(filepath.Join(records, "*.json")); len(left) != 0 {
		t.Fatalf("records left: %v", left)
	}
}

// staleSocket leaves a socket file nobody listens on, as a server that was
// killed does.
func staleSocket(t *testing.T, sock string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
}

func TestKilledOlderServerLeavesAnInterruption(t *testing.T) {
	sessions, _ := isolateRestore(t)
	sock := filepath.Join(sessions, "agy-aaaaaaaaaa-1.sock")
	_ = writeSessionMeta(sock, []string{"agy"})
	if _, _, err := writeRestoreRecord(sock, sessionMeta{Command: []string{"agy"}, PWD: "/srv"}, true); err != nil {
		t.Fatal(err)
	}
	staleSocket(t, sock)
	list, err := collectSessions("")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Status.State != "interrupted" || list[0].name() != sessionName(sock) {
		t.Fatalf("list = %+v", list)
	}
	if isSocket(sock) {
		t.Fatal("stale socket kept")
	}

	// Killing a session that is already gone leaves nothing to restore.
	sock2 := filepath.Join(sessions, "agy-bbbbbbbbbb-2.sock")
	_, _, _ = writeRestoreRecord(sock2, sessionMeta{Command: []string{"agy"}, PWD: "/srv"}, false)
	staleSocket(t, sock2)
	if err := killSession(sock2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(recordFor(t, sock2)); !os.IsNotExist(err) {
		t.Fatal("killing a dead session kept its record")
	}
}
