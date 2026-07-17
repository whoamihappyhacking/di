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
