package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFZFArgsUseFourLineSelectionList(t *testing.T) {
	args := fzfArgs("/tmp/di", 24)
	if !containsArg(args, "--height=100%") {
		t.Fatalf("picker should use the full terminal height: %v", args)
	}
	if !containsArg(args, "--preview-window=down,18,wrap,border-top") {
		t.Fatalf("picker should reserve four lines for selection: %v", args)
	}
	if containsArg(args, "--preview-window=down,60%,wrap,border-top") {
		t.Fatalf("picker should use a fixed four-line selection area: %v", args)
	}
	if !containsArg(args, "--no-hscroll") {
		t.Fatalf("picker should keep aligned columns in view: %v", args)
	}
	if !containsArg(args, "--preview='/tmp/di' --preview {1}") {
		t.Fatalf("preview command should pass the socket field: %v", args)
	}
}

func TestRenderPreviewUsesFinalTerminalScreen(t *testing.T) {
	output := []byte("stale screen\x1b[2J\x1b[Hagent current screen")
	preview := string(renderPreview(output, 80, 8))
	if !strings.Contains(preview, "agent current screen") {
		t.Fatalf("preview = %q, want current screen content", preview)
	}
	if strings.Contains(preview, "stale screen") {
		t.Fatalf("preview kept content cleared by terminal redraw: %q", preview)
	}
}

func TestPreviewSizeUsesFZFDimensions(t *testing.T) {
	t.Setenv("FZF_PREVIEW_COLUMNS", "73")
	t.Setenv("FZF_PREVIEW_LINES", "19")
	cols, rows := previewSize()
	if cols != 73 || rows != 19 {
		t.Fatalf("preview size = %d x %d, want 73 x 19", cols, rows)
	}
}

func TestPreviewSessionWritesOnlyTerminalContent(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "session.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		_, _ = conn.Write([]byte("agent content\n"))
		_ = conn.Close()
	}()

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	previewErr := previewSession([]string{sock})
	_ = writer.Close()
	os.Stdout = oldStdout
	output, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if previewErr != nil {
		t.Fatalf("render preview: %v", previewErr)
	}
	if readErr != nil {
		t.Fatalf("read preview: %v", readErr)
	}
	if string(output) != "agent content\n" {
		t.Fatalf("preview = %q, want terminal content only", output)
	}
}

func TestSessionPreviewOutputReadsHistoryWithoutWaitingForSessionExit(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "session.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("history line\n"))
		time.Sleep(500 * time.Millisecond)
	}()

	output, err := sessionPreviewOutput(sock)
	if err != nil {
		t.Fatalf("read session preview: %v", err)
	}
	if string(output) != "history line\n" {
		t.Fatalf("preview output = %q, want history line", output)
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("preview test server did not finish")
	}
}

func TestSessionPreviewOutputKeepsHistoryWhenSessionCloses(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "session.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		_, _ = conn.Write([]byte("short-lived output\n"))
		_ = conn.Close()
	}()

	output, err := sessionPreviewOutput(sock)
	if err != nil {
		t.Fatalf("read closed session preview: %v", err)
	}
	if string(output) != "short-lived output\n" {
		t.Fatalf("preview output = %q, want short-lived output", output)
	}
}

func TestSessionDisplayLinesUseAlignedColumns(t *testing.T) {
	short := sessionInfo{Sock: "/tmp/short.sock", Meta: sessionMeta{
		Name:    "sh",
		PWD:     "/work",
		Command: []string{"sh"},
	}}
	long := sessionInfo{Sock: "/tmp/long.sock", Meta: sessionMeta{
		Name:    "long-session-name",
		PWD:     "/work/project",
		Command: []string{"go", "run", "./cmd/worker"},
	}}
	shortFields := strings.Split(short.displayLine(100), "\t")
	longFields := strings.Split(long.displayLine(100), "\t")
	if len(shortFields) != 4 || len(longFields) != 4 {
		t.Fatalf("display lines should have four tab-separated fields: %q / %q", shortFields, longFields)
	}
	for field := 1; field < 4; field++ {
		if len(shortFields[field]) != len(longFields[field]) {
			t.Fatalf("field %d is not aligned: %q / %q", field, shortFields[field], longFields[field])
		}
	}
	if !strings.HasPrefix(shortFields[0], "/tmp/short.sock") || !strings.HasPrefix(longFields[0], "/tmp/long.sock") {
		t.Fatal("socket path must remain the first field for attach and preview")
	}
}

func TestSessionDisplayLineKeepsSocketAndMetadataOrder(t *testing.T) {
	line := (sessionInfo{Sock: "/tmp/session.sock", Meta: sessionMeta{
		Name:    "shell",
		PWD:     "/work",
		Command: []string{"bash"},
	}}).displayLine(100)
	if !strings.HasPrefix(line, "/tmp/session.sock\t/work") {
		t.Fatalf("display line = %q, want socket and directory first", line)
	}
}

func TestExpandAliasArgs(t *testing.T) {
	aliases := map[string]string{"app": "app -f /etc/app.conf"}
	want := []string{"app", "-f", "/etc/app.conf", "-x"}
	got := expandAliasArgs([]string{"app", "-x"}, aliases)
	if len(got) != len(want) {
		t.Fatalf("expanded %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expanded %q, want %q", got, want)
		}
	}
	unchanged := expandAliasArgs([]string{"vim", "x"}, aliases)
	if len(unchanged) != 2 || unchanged[0] != "vim" {
		t.Fatalf("unknown command should be unchanged: %q", unchanged)
	}
	empty := expandAliasArgs(nil, aliases)
	if len(empty) != 0 {
		t.Fatalf("empty args should stay empty: %q", empty)
	}
}

func TestExpandAliasArgsAppendsAfterValue(t *testing.T) {
	aliases := map[string]string{"app": "app -f /etc/app.conf"}
	want := []string{"app", "-f", "/etc/app.conf", "extra1", "extra2"}
	got := expandAliasArgs([]string{"app", "extra1", "extra2"}, aliases)
	if len(got) != len(want) {
		t.Fatalf("expanded %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expanded %q, want %q", got, want)
		}
	}
}

func TestSplitAliasWords(t *testing.T) {
	words := splitAliasWords(`cd '/path/with space'`)
	want := []string{"cd", "/path/with space"}
	if len(words) != len(want) || words[0] != want[0] || words[1] != want[1] {
		t.Fatalf("split %q, want %q", words, want)
	}
	one := splitAliasWords("realapp")
	if len(one) != 1 || one[0] != "realapp" {
		t.Fatalf("split %q", one)
	}
}

func TestParseAliasOutputBashAndZshFormats(t *testing.T) {
	out := []byte("alias app='app -f /etc/app.conf'\nalias ls='ls --color=auto'\nll='ls -l'\nnot an alias line\n")
	m := parseAliasOutput(out)
	if m["app"] != "app -f /etc/app.conf" {
		t.Fatalf("app = %q", m["app"])
	}
	if m["ls"] != "ls --color=auto" {
		t.Fatalf("ls = %q", m["ls"])
	}
	if m["ll"] != "ls -l" {
		t.Fatalf("ll = %q", m["ll"])
	}
	if _, ok := m["not"]; ok {
		t.Fatal("non-alias line parsed as an alias")
	}
}

func TestParseAliasOutputSkipsNoise(t *testing.T) {
	out := []byte("\x1b[37C \x1b[1G banner\nsome random line\nx y = z\n")
	m := parseAliasOutput(out)
	if len(m) != 0 {
		t.Fatalf("noise parsed as aliases: %v", m)
	}
}

func TestUnquoteAliasValue(t *testing.T) {
	if got := unquoteAliasValue(`'app -f /etc/app.conf'`); got != "app -f /etc/app.conf" {
		t.Fatalf("single-quoted value = %q", got)
	}
	if got := unquoteAliasValue(`"a \"quoted\" word"`); got != `a "quoted" word` {
		t.Fatalf("double-quoted value = %q", got)
	}
	if got := unquoteAliasValue(`'plain'\''quoted'`); got != "plain'quoted" {
		t.Fatalf("embedded quote value = %q", got)
	}
	if got := unquoteAliasValue("plain"); got != "plain" {
		t.Fatalf("unquoted value = %q", got)
	}
}

func TestQueryShellAliasesRunsShell(t *testing.T) {
	script := filepath.Join(t.TempDir(), "shell")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf \"alias app='app -f /etc/app.conf'\\n\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := queryShellAliases(script)
	if m["app"] != "app -f /etc/app.conf" {
		t.Fatalf("aliases = %v", m)
	}
}

func TestLoadAliasesFromCachesAndSkipsShell(t *testing.T) {
	shell := filepath.Join(t.TempDir(), "shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nprintf \"alias app='app -f /etc/app.conf'\\n\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cacheFile := filepath.Join(t.TempDir(), "aliases.json")
	first := loadAliasesFrom(shell, cacheFile)
	if first["app"] != "app -f /etc/app.conf" {
		t.Fatalf("first load aliases = %v", first)
	}
	_ = os.Remove(shell)
	second := loadAliasesFrom(shell, cacheFile)
	if second["app"] != "app -f /etc/app.conf" {
		t.Fatalf("cached load aliases = %v", second)
	}
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("cache file missing: %v", err)
	}
}

func TestLoadAliasesFromRefetchesOnStaleKey(t *testing.T) {
	shell := filepath.Join(t.TempDir(), "shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nprintf \"alias app='app -f /etc/app.conf'\\n\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cacheFile := filepath.Join(t.TempDir(), "aliases.json")
	if err := os.WriteFile(cacheFile, []byte(`{"key":"stale","aliases":{"app":"old"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadAliasesFrom(shell, cacheFile); got["app"] != "app -f /etc/app.conf" {
		t.Fatalf("stale key should refetch: %v", got)
	}
}

func TestAliasCacheKeyTracksRcMtime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	rc := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(rc, []byte("alias a=b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := aliasCacheKey("/bin/bash")
	time.Sleep(10 * time.Millisecond)
	beforeAgain := aliasCacheKey("/bin/bash")
	if before != beforeAgain {
		t.Fatalf("key must be stable while rc is unchanged: %s / %s", before, beforeAgain)
	}
	if err := os.WriteFile(rc, []byte("alias a='b c'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after := aliasCacheKey("/bin/bash")
	if after == before {
		t.Fatal("key must change when the rc file changes")
	}
}

func TestShellAliasCmdDetachesFromTerminal(t *testing.T) {
	syscalls := shellAliasCmd(context.Background(), "/bin/sh").SysProcAttr
	if syscalls == nil || !syscalls.Setsid {
		t.Fatal("alias query must run in its own session to avoid SIGTTOU suspension")
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestUniqueSocketPathFitsPortableLimit(t *testing.T) {
	dir := t.TempDir()
	sock, err := uniqueSocketPath(dir, strings.Repeat("long-command-", 30))
	if err != nil {
		t.Fatal(err)
	}
	if len(sock) > maxSocketPathLen {
		t.Fatalf("socket path has %d bytes, want at most %d: %s", len(sock), maxSocketPathLen, sock)
	}

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on generated socket path: %v", err)
	}
	_ = ln.Close()
}

func TestUniqueSocketPathRejectsLongDirectory(t *testing.T) {
	dir := filepath.Join("/", strings.Repeat("x", maxSocketPathLen))
	if _, err := uniqueSocketPath(dir, "session"); err == nil {
		t.Fatal("expected an error for a session directory longer than the socket path limit")
	}
}

func TestRunServerReplaysFastCommandOutput(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not available")
	}

	output := runServerAndRead(t, []string{sh, "-c", "printf quick-output"})
	if !bytes.Contains(output, []byte("quick-output")) {
		t.Fatalf("fast command output was not replayed: %q", output)
	}
}

func TestRunServerKeepsPTYForDescendant(t *testing.T) {
	output := runServerAndRead(t, []string{
		os.Args[0],
		"-test.run=^TestPTYDescendantHelper$",
		"--",
		"parent",
	})
	if !bytes.Contains(output, []byte("descendant-output")) {
		t.Fatalf("descendant output was lost after its parent exited: %q", output)
	}
}

func TestClosingClientsStillAllowsReattach(t *testing.T) {
	server := &ptyServer{
		clients:     map[net.Conn]struct{}{},
		clientReady: make(chan struct{}),
	}

	first, firstPeer := net.Pipe()
	defer firstPeer.Close()
	if !server.add(first) {
		t.Fatal("first client was rejected")
	}
	server.closeClients()
	server.clientWG.Done()

	second, secondPeer := net.Pipe()
	defer secondPeer.Close()
	if !server.add(second) {
		t.Fatal("client was rejected after detaching existing clients")
	}
	server.shutdownClients()
	server.clientWG.Done()

	third, thirdPeer := net.Pipe()
	defer thirdPeer.Close()
	if server.add(third) {
		t.Fatal("client was accepted after server shutdown")
	}
}

func TestPTYDescendantHelper(t *testing.T) {
	mode := helperMode(os.Args)
	switch mode {
	case "parent":
		cmd := exec.Command(os.Args[0], "-test.run=^TestPTYDescendantHelper$", "--", "child")
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	case "child":
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(os.Stdout, "descendant-output")
		os.Exit(0)
	}
}

func helperMode(args []string) string {
	for i, arg := range args {
		if arg == "--" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func runServerAndRead(t *testing.T, command []string) []byte {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "session.sock")
	done := make(chan error, 1)
	go func() {
		done <- runServer(append([]string{sock}, command...))
	}()

	if err := waitSocket(sock, serverStartWait); err != nil {
		select {
		case serverErr := <-done:
			t.Fatalf("server exited before creating its socket: %v", serverErr)
		default:
		}
		t.Fatal(err)
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	resize := make([]byte, 8)
	binary.BigEndian.PutUint32(resize[:4], 24)
	binary.BigEndian.PutUint32(resize[4:], 80)
	if err := writeFrame(conn, frameResize, resize); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read server output: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server exited with an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit after the PTY closed")
	}
	return output
}
