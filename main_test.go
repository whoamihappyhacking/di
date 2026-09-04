package main

import (
	"bytes"
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
