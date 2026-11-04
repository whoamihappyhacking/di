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

	"github.com/creack/pty"
)

func TestAttachStatusCleanup(t *testing.T) {
	for _, mode := range []string{"local-detach", "remote-detach", "eof"} {
		t.Run(mode, func(t *testing.T) {
			sock := filepath.Join(t.TempDir(), "session.sock")
			listener, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_, _ = io.WriteString(conn, "\x1b]7501;state=working\x07STATUS_READY")
				if mode != "local-detach" {
					return
				}
				_, _ = io.Copy(io.Discard, conn)
			}()
			cmd := exec.Command(os.Args[0], "-test.run=^TestAttachStatusHelper$", "--", sock)
			cmd.Env = append(os.Environ(), "DI_ATTACH_TEST=1", "D_DETACH=^]")
			master, err := pty.Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			outputDone := make(chan []byte, 1)
			go func() {
				var output bytes.Buffer
				buffer := make([]byte, 4096)
				detached := false
				for {
					n, err := master.Read(buffer)
					output.Write(buffer[:n])
					if mode == "local-detach" && !detached && bytes.Contains(output.Bytes(), []byte("STATUS_READY")) {
						detached = true
						_, _ = master.Write([]byte{0x1d})
					}
					if err != nil {
						outputDone <- output.Bytes()
						return
					}
				}
			}()
			waitDone := make(chan error, 1)
			go func() { waitDone <- cmd.Wait() }()
			select {
			case err := <-waitDone:
				if err != nil {
					t.Fatalf("attach helper: %v", err)
				}
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("attach did not exit")
			}
			output := <-outputDone
			working := bytes.LastIndex(output, []byte("\x1b]7501;state=working\x07"))
			reset := bytes.LastIndex(output, []byte(terminalStatusReset))
			if working < 0 || reset <= working {
				t.Fatalf("missing ordered status cleanup: %q", output)
			}
			if bytes.Contains(output[reset:], []byte("state=done")) {
				t.Fatal("detach must not report task completion")
			}
			<-serverDone
		})
	}
}

func TestAttachStatusHelper(t *testing.T) {
	if os.Getenv("DI_ATTACH_TEST") != "1" {
		return
	}
	if err := attach(os.Args[len(os.Args)-1]); err != nil {
		t.Fatal(err)
	}
}

func TestAppendHistoryDeduplicatesInPlaceRedrawsAndPreservesScrollback(t *testing.T) {
	s := &ptyServer{}
	// Scrollback push: moves up 2 lines, writes 5 lines (down 4 > up 2).
	s.appendHistory([]byte("\r\x1b[2Ascrollback-line-1\r\nscrollback-line-2\r\nscrollback-line-3\r\nviewport-1\r\nviewport-2"))
	// Consecutive in-place spinner redraws: moves up 2 lines, writes 3 lines (down 2 <= up 2).
	for i := 0; i < 50; i++ {
		s.appendHistory([]byte(fmt.Sprintf("\r\x1b[2A\x1b[Kspinner-tick-%d\r\n\x1b[Kprompt-line-%d\r\n\x1b[Kstatus", i, i)))
	}
	hist := string(s.history)
	if !strings.Contains(hist, "scrollback-line-1") || !strings.Contains(hist, "scrollback-line-3") {
		t.Fatalf("scrollback lines were evicted by in-place redraws: %q", hist)
	}
	if strings.Contains(hist, "spinner-tick-0") || strings.Contains(hist, "spinner-tick-48") {
		t.Fatalf("stale in-place redraw frame remained in history: %q", hist)
	}
	if !strings.Contains(hist, "spinner-tick-49") {
		t.Fatalf("latest in-place redraw frame missing from history: %q", hist)
	}

	// Full screen clear resets history to the clear point.
	s.appendHistory([]byte("before-clear\x1b[2J\x1b[Hfresh-history-after-resize"))
	histAfterClear := string(s.history)
	if strings.Contains(histAfterClear, "scrollback-line-1") || strings.Contains(histAfterClear, "before-clear") {
		t.Fatalf("history kept content prior to full screen clear: %q", histAfterClear)
	}
	if !strings.HasPrefix(histAfterClear, "\x1b[2J\x1b[3J\x1b[Hfresh-history-after-resize") {
		t.Fatalf("full screen clear was not normalized to clear scrollback: %q", histAfterClear)
	}
}

func TestForwardSessionOutputCoalescesAndSynchronizesRedraws(t *testing.T) {
	connReader, connWriter := net.Pipe()
	defer connReader.Close()

	go func() {
		// Send an unwrapped multi-line redraw split across two chunks, plus a full clear.
		_, _ = connWriter.Write([]byte("\r\x1b[3A\x1b[Jline-1\r\n"))
		_, _ = connWriter.Write([]byte("\x1b[Kline-2\r\n\x1b[Kline-3\x1b[2J\x1b[H"))
		_ = connWriter.Close()
	}()

	var dst bytes.Buffer
	forwardSessionOutput(&dst, connReader)
	got := dst.String()
	if !strings.HasPrefix(got, "\x1b[?2026h") || !strings.HasSuffix(got, "\x1b[?2026l") {
		t.Fatalf("multi-line redraw was not wrapped in synchronized output markers: %q", got)
	}
	if !strings.Contains(got, "\x1b[2J\x1b[3J\x1b[H") {
		t.Fatalf("full screen clear was not upgraded to clear scrollback: %q", got)
	}
}

func TestRunServerAnswersSyncOutputQueryAndReattachesWithSameSizeBounce(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not available")
	}

	// Child queries DECRQM 2026, prints whether it received 2026;2$y, and prints WINCH on SIGWINCH.
	script := `trap 'printf "\nWINCH\n"' WINCH; stty -echo raw; printf '\033[?2026$p'; dd bs=1 count=11 2>/dev/null; sleep 0.3`
	output := runServerAndReadWithSameSizeReattach(t, []string{sh, "-c", script})
	if !bytes.Contains(output, []byte("\x1b[?2026;2$y")) {
		t.Fatalf("server did not reply to DECRQM 2026 query: %q", output)
	}
	if !bytes.Contains(output, []byte("WINCH")) {
		t.Fatalf("server did not trigger SIGWINCH bounce on re-attach: %q", output)
	}
}

func runServerAndReadWithSameSizeReattach(t *testing.T, command []string) []byte {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "session.sock")
	done := make(chan error, 1)
	go func() {
		done <- runServer(append([]string{sock}, command...))
	}()

	if err := waitSocket(sock, serverStartWait); err != nil {
		t.Fatal(err)
	}

	resize := make([]byte, 8)
	binary.BigEndian.PutUint32(resize[:4], 24)
	binary.BigEndian.PutUint32(resize[4:], 80)

	// First attach sets 24x80, then detaches.
	conn1, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(conn1, frameResize, resize); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	_ = conn1.Close()

	// Second attach sends the exact same 24x80 size; server should bounce cols to trigger SIGWINCH.
	conn2, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	if err := writeFrame(conn2, frameResize, resize); err != nil {
		t.Fatal(err)
	}
	if err := conn2.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(conn2)
	if err != nil {
		t.Fatalf("read server output: %v", err)
	}
	<-done
	return output
}

func TestFZFArgsReserveExpandedSelectionListAndBindSwitchKey(t *testing.T) {
	args := fzfArgs("/tmp/di", 24)
	if !containsArg(args, "--height=100%") {
		t.Fatalf("picker should use the full terminal height: %v", args)
	}
	if !containsArg(args, "--preview-window=down,13,wrap,border-top") {
		t.Fatalf("picker should reserve expanded selection rows: %v", args)
	}
	if !containsArg(args, `--bind=ctrl-\:down`) {
		t.Fatalf("picker should bind Ctrl-\\ to move down: %v", args)
	}
	if !containsArg(args, "--no-hscroll") {
		t.Fatalf("picker should keep aligned columns in view: %v", args)
	}
	if !containsArg(args, "--preview='/tmp/di' --preview {1}") {
		t.Fatalf("preview command should pass the socket field: %v", args)
	}
}

func TestSwitchKeyInput(t *testing.T) {
	if !isSwitchInput([]byte{0x1c}) {
		t.Fatal("default Ctrl-\\ (0x1c) was not recognized as switch input")
	}
	if !isSwitchInput([]byte("\x1b[92;5u")) {
		t.Fatal("CSI u Ctrl-\\ sequence was not recognized as switch input")
	}
	if isSwitchInput([]byte{0x1d}) {
		t.Fatal("Ctrl-] detach key must not trigger switch input")
	}
}

func TestExtractSessionSummaryAndStatus(t *testing.T) {
	rule := strings.Repeat("─", 94)
	busyOutput := []byte(fmt.Sprintf(
		"  上一轮对话内容\r\n  真正有价值的最近回复摘要\r\n\r\n%s\r\n>\r\n%s\r\n⣽  Read\r\n? for shortcuts",
		rule, rule,
	))
	status, summary := extractSessionSummary(busyOutput)
	if status != "●" {
		t.Fatalf("status = %q, want ● for active spinner", status)
	}
	if !strings.Contains(summary, "[Read]") || !strings.Contains(summary, "真正有价值的最近回复摘要") {
		t.Fatalf("summary = %q, want tool name and latest conversation line", summary)
	}

	ompWorking := []byte("\x1b]7501;state=working:app=omp\x1b\\working line\r\n")
	if st, _ := extractSessionSummary(ompWorking); st != "●" {
		t.Fatalf("omp working status = %q, want ●", st)
	}
	ompDone := []byte("\x1b]7501;state=working:app=omp\x1b\\working line\r\n\x1b]7501;state=done:app=omp\x1b\\")
	if st, _ := extractSessionSummary(ompDone); st != "○" {
		t.Fatalf("omp done status = %q, want ○", st)
	}
}

func TestRenderPreviewCollapsesPaddingAndPreservesConversation(t *testing.T) {
	// Simulate a 33-row viewport redraw with 20 blank padding lines above the spinner.
	var frame strings.Builder
	frame.WriteString("对话第一行内容\r\n对话第二行内容\r\n")
	for i := 0; i < 20; i++ {
		frame.WriteString("\r\n")
	}
	frame.WriteString("⣟  Read\r\n")
	preview := string(renderPreview([]byte(frame.String()), 80, 10))
	if !strings.Contains(preview, "对话第一行内容") || !strings.Contains(preview, "⣟  Read") {
		t.Fatalf("preview lost conversation text due to blank padding lines: %q", preview)
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

func TestRunWindowSizeBounceWaitsForHeavyRedrawFlush(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	tracker := newRedrawTracker()
	stop := make(chan struct{})
	defer close(stop)

	var dst bytes.Buffer
	outputDone := make(chan struct{})
	go func() {
		forwardSessionOutputWithTracker(&dst, clientConn, tracker)
		close(outputDone)
	}()

	type resizeEvent struct {
		rows uint16
		cols uint16
		at   time.Time
	}
	var events []resizeEvent
	serverDone := make(chan error, 1)
	go func() {
		defer close(serverDone)
		// Initial history burst (no full screen clear).
		_, _ = serverConn.Write([]byte("initial-viewport\r\n"))
		for i := 0; i < 2; i++ {
			typ, payload, err := readFrame(serverConn)
			if err != nil {
				serverDone <- err
				return
			}
			if typ != frameResize || len(payload) != 8 {
				serverDone <- fmt.Errorf("unexpected frame %c len=%d", typ, len(payload))
				return
			}
			rows := uint16(binary.BigEndian.Uint32(payload[:4]))
			cols := uint16(binary.BigEndian.Uint32(payload[4:]))
			events = append(events, resizeEvent{rows: rows, cols: cols, at: time.Now()})
			// Simulate heavy agy render (~180ms CPU before \x1b[2J, 60ms stream flush).
			time.Sleep(180 * time.Millisecond)
			_, _ = serverConn.Write([]byte(fmt.Sprintf("\x1b[?25l\x1b[2J\x1b[Hscrollback-cols-%d\r\n", cols)))
			time.Sleep(60 * time.Millisecond)
			_, _ = serverConn.Write([]byte(fmt.Sprintf("viewport-cols-%d\x1b[?25h", cols)))
		}
		_ = serverConn.Close()
	}()

	runWindowSizeBounce(clientConn, 41, 94, tracker, stop)
	if err := <-serverDone; err != nil {
		t.Fatalf("server error: %v", err)
	}
	<-outputDone

	if len(events) != 2 {
		t.Fatalf("got %d resize events, want 2", len(events))
	}
	if events[0].rows != 41 || events[0].cols != 93 {
		t.Fatalf("first resize = %dx%d, want 41x93", events[0].rows, events[0].cols)
	}
	if events[1].rows != 41 || events[1].cols != 94 {
		t.Fatalf("second resize = %dx%d, want 41x94", events[1].rows, events[1].cols)
	}
	if gap := events[1].at.Sub(events[0].at); gap < 240*time.Millisecond {
		t.Fatalf("second resize sent after %v, before first 240ms heavy redraw finished flushing", gap)
	}
	out := dst.String()
	if !strings.Contains(out, "scrollback-cols-94") || !strings.Contains(out, "viewport-cols-94") {
		t.Fatalf("restored width redraw missing from output: %q", out)
	}
}

func TestTrimHistorySliceStartsAtLineBoundary(t *testing.T) {
	raw := []byte("a52e824;file:///home/c/veloren/foo.rs\x07foo.rs\x1b]8;;\x07\r\nclean-line-start\r\n")
	trimmed := string(trimHistorySlice(raw))
	if trimmed != "clean-line-start\r\n" {
		t.Fatalf("trimHistorySlice = %q, want %q", trimmed, "clean-line-start\r\n")
	}
}

func TestFullScreenScrollbackDumpIsNotWrappedInSyncOutput(t *testing.T) {
	dump := []byte("\x1b[?25l\x1b[2J\x1b[3J\x1b[Hline-1\r\n\x1b[Kline-2\r\n\x1b[J\x1b[?25h")
	if shouldWrapSyncOutput(dump) {
		t.Fatal("full-screen scrollback dump should not be wrapped in DEC 2026 synchronized output")
	}
	inPlace := []byte("\x1b[?25l\r\x1b[12A\x1b[Kline-1\r\n\x1b[J\x1b[?25h")
	if !shouldWrapSyncOutput(inPlace) {
		t.Fatal("in-place cursor-up redraw should be wrapped in DEC 2026 synchronized output")
	}
}

func TestInPlaceCompactorStripsUnchangedTopLinesAndSkipsDuplicateFrames(t *testing.T) {
	var c inPlaceCompactor
	frame1 := []byte("\x1b[?25l\x1b[?7l\r\x1b[3A\x1b[Jtop-0\r\ntop-1\r\nspin-1\r\nstatus-1\x1b[?7h\x1b[?25h")
	out1, skip1 := c.compact(frame1)
	if skip1 || !bytes.Equal(out1, frame1) {
		t.Fatalf("first frame should pass through unchanged: skip=%v out=%q", skip1, out1)
	}

	// Exact duplicate frame should be skipped completely.
	_, skipDup := c.compact(frame1)
	if !skipDup {
		t.Fatal("identical consecutive in-place frame should be skipped")
	}

	// Frame where top-0 and top-1 are unchanged, and only spin-2 + status-2 change.
	frame2 := []byte("\x1b[?25l\x1b[?7l\r\x1b[3A\x1b[Jtop-0\r\ntop-1\r\nspin-2\r\nstatus-2\x1b[?7h\x1b[?25h")
	out2, skip2 := c.compact(frame2)
	if skip2 {
		t.Fatal("changed frame must not be skipped")
	}
	if bytes.Contains(out2, []byte("top-0")) || bytes.Contains(out2, []byte("top-1")) {
		t.Fatalf("compacted frame still contained unchanged top lines: %q", out2)
	}
	if !bytes.Contains(out2, []byte("\r\x1b[1A\x1b[Jspin-2\r\nstatus-2")) {
		t.Fatalf("compacted frame did not adjust cursor-up count to 1 with \\x1b[J clear: %q", out2)
	}
	if !shouldWrapSyncOutput(out2) {
		t.Fatalf("compacted frame should still qualify for DEC 2026 synchronized output: %q", out2)
	}

	// Full clear resets compactor state.
	_, _ = c.compact([]byte("\x1b[2J\x1b[3J\x1b[Hfull-reset"))
	out3, skip3 := c.compact(frame2)
	if skip3 || !bytes.Equal(out3, frame2) {
		t.Fatalf("frame after full clear should not be diffed against pre-clear state: skip=%v out=%q", skip3, out3)
	}
}

func TestVTermAndBlitEncoderDiffsInPlaceSpinnerWithoutFlickerOrClears(t *testing.T) {
	var out bytes.Buffer
	r := newClientSessionRenderer(&out, 10, 60, nil)
	defer r.Close()

	// Push 25 lines into scrollback + 10 lines on screen.
	var initBuf bytes.Buffer
	for i := 1; i <= 35; i++ {
		fmt.Fprintf(&initBuf, "history-line-%02d\r\n", i)
	}
	if err := r.ProcessChunk(initBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if r.vt.ScrollbackLen() < 25 {
		t.Fatalf("scrollback len = %d, want >= 25", r.vt.ScrollbackLen())
	}

	// Simulate an agy-style 9-line in-place redraw frame.
	buildFrame := func(tick int) []byte {
		var b bytes.Buffer
		b.WriteString("\x1b[?25l\r\x1b[8A\x1b[J")
		for i := 1; i <= 7; i++ {
			fmt.Fprintf(&b, "conversation-row-%d\r\n", i)
		}
		fmt.Fprintf(&b, "⠋ Running tool tick-%d\r\n", tick)
		b.WriteString("? for shortcuts\x1b[?25h")
		return b.Bytes()
	}

	if err := r.ProcessChunk(buildFrame(1)); err != nil {
		t.Fatal(err)
	}
	out.Reset()

	// Exact duplicate redraw must emit 0 bytes to the terminal.
	if err := r.ProcessChunk(buildFrame(1)); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("duplicate redraw emitted %d bytes, want 0: %q", out.Len(), out.String())
	}

	// Next spinner tick only changes "tick-1" -> "tick-2" (1 character!).
	if err := r.ProcessChunk(buildFrame(2)); err != nil {
		t.Fatal(err)
	}
	diff := out.String()
	if !strings.HasPrefix(diff, "\x1b[?2026h") || !strings.Contains(diff, "\x1b[?2026l") {
		t.Fatalf("diff update not wrapped in DEC 2026 sync markers: %q", diff)
	}
	if strings.Contains(diff, "\x1b[2J") || strings.Contains(diff, "\x1b[3J") {
		t.Fatalf("diff update must not clear screen or scrollback: %q", diff)
	}
	if strings.Contains(diff, "conversation-row-1") {
		t.Fatalf("diff update re-emitted unchanged conversation rows: %q", diff)
	}
	if r.vt.ScrollbackLen() < 25 {
		t.Fatalf("scrollback lost after in-place spinner redraws: got %d", r.vt.ScrollbackLen())
	}
}

func TestVTermPreservesScrollbackAcrossBare3JAndReplacesOnFullDump(t *testing.T) {
	vt := newVTerm(6, 40)
	for i := 1; i <= 20; i++ {
		vt.Write([]byte(fmt.Sprintf("scroll-row-%02d\r\n", i)))
	}
	initialSB := vt.ScrollbackLen()
	if initialSB < 14 {
		t.Fatalf("initial scrollback = %d, want >= 14", initialSB)
	}

	// 1. Bare \x1b[2J\x1b[3J\x1b[H followed by only a 3-line screen (no history re-dump).
	vt.Write([]byte("\x1b[?25l\x1b[2J\x1b[3J\x1b[Hscreen-only-1\r\nscreen-only-2\x1b[?25h"))
	if vt.ScrollbackLen() < initialSB {
		t.Fatalf("bare \\x1b[3J destroyed scrollback: got %d, want >= %d", vt.ScrollbackLen(), initialSB)
	}

	// 2. Full history re-dump on resize (\x1b[2J\x1b[3J\x1b[H + 20 fresh lines).
	var dump bytes.Buffer
	dump.WriteString("\x1b[?25l\x1b[2J\x1b[3J\x1b[H")
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&dump, "rewrapped-row-%02d\r\n", i)
	}
	dump.WriteString("\x1b[?25h")
	vt.Write(dump.Bytes())

	if !vt.ConsumeFullScrollbackReplaced() {
		t.Fatal("expected fullScrollbackReplaced=true after full history dump")
	}
	lines := vt.DetectionLines(50)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "rewrapped-row-01") {
		t.Fatalf("missing rewrapped scrollback after full dump: %q", joined)
	}
	if strings.Contains(joined, "scroll-row-01") {
		t.Fatalf("old scrollback was duplicated after full dump: %q", joined)
	}
}

func TestVTermRecoversScrolledOffLinesFromInPlaceSlidingWindowRedraw(t *testing.T) {
	vt := newVTerm(6, 40)
	vt.Write([]byte("\x1b[Hline-A\r\nline-B\r\nline-C\r\nline-D\r\nline-E\r\nline-F"))
	if vt.ScrollbackLen() != 0 {
		t.Fatalf("expected 0 initial scrollback, got %d", vt.ScrollbackLen())
	}

	// Shift content up by 2 lines in-place using \x1b[H without bottom linefeeds.
	vt.Write([]byte("\x1b[H\x1b[2Kline-C\r\n\x1b[2Kline-D\r\n\x1b[2Kline-E\r\n\x1b[2Kline-F\r\n\x1b[2Kline-G\r\n\x1b[2Kline-H"))
	if vt.ScrollbackLen() != 2 {
		t.Fatalf("sliding window scrollback len = %d, want 2", vt.ScrollbackLen())
	}
	all := strings.Join(vt.DetectionLines(20), "\n")
	if !strings.Contains(all, "line-A") || !strings.Contains(all, "line-B") {
		t.Fatalf("scrolled-off top lines were not recovered into scrollback: %q", all)
	}
}

func TestVTermWideCJKOverwriteAndScrollbarOverlay(t *testing.T) {
	var out bytes.Buffer
	r := newClientSessionRenderer(&out, 6, 20, nil)
	defer r.Close()

	for i := 1; i <= 15; i++ {
		_ = r.ProcessChunk([]byte(fmt.Sprintf("历史记录第%02d行\r\n", i)))
	}
	// Overwrite second column of wide character '历' at row 0 col 0 with ASCII 'X'.
	_ = r.ProcessChunk([]byte("\x1b[1;2HX"))
	row0 := r.vt.ViewportRow(0).plainText()
	if !strings.HasPrefix(row0, " X史记录") {
		t.Fatalf("wide character orphan half was not cleared on overwrite: %q", row0)
	}

	out.Reset()
	// Press PageUp (\x1b[5~) to scroll back into history.
	if !r.HandleInput([]byte("\x1b[5~")) {
		t.Fatal("PageUp should be consumed by scrollback viewport")
	}
	if !r.vt.IsScrolledBack() {
		t.Fatal("vterm should be in scrolled-back state after PageUp")
	}
	scrolledOut := out.String()
	if !strings.ContainsRune(scrolledOut, scrollbarTrackChar) && !strings.ContainsRune(scrolledOut, scrollbarThumbChar) {
		t.Fatalf("scrolled view did not render herdr scrollbar characters: %q", scrolledOut)
	}

	// Normal keystroke snaps scrollback back to 0 and is not consumed.
	if r.HandleInput([]byte("a")) {
		t.Fatal("normal keystroke should be forwarded to session")
	}
	if r.vt.IsScrolledBack() {
		t.Fatal("normal keystroke should reset scrollOffset to 0")
	}
}

func TestCursorSettleDebouncesTransientJumpsAndHides(t *testing.T) {
	cs := newCursorSettleState()
	t0 := time.Now()

	// Initial position settles immediately at preferredRow=5, col=10.
	r, c, vis := cs.Observe(5, 10, true, t0)
	if r != 5 || c != 10 || !vis {
		t.Fatalf("initial settle = (%d,%d,%v), want (5,10,true)", r, c, vis)
	}

	// Transient cursor hide (\x1b[?25l) during redraw is held visible for 100ms.
	_, _, vis = cs.Observe(0, 0, false, t0.Add(5*time.Millisecond))
	if !vis {
		t.Fatal("transient cursor hide should be held visible during redraw")
	}

	// Transient jump to row 1 during redraw holds settled position (5, 10).
	r, c, vis = cs.Observe(1, 2, true, t0.Add(10*time.Millisecond))
	if r != 5 || c != 10 || !vis {
		t.Fatalf("transient row jump = (%d,%d,%v), want held (5,10,true)", r, c, vis)
	}

	// After 100ms hold deadline expires, cursor advances to (1, 2).
	if !cs.Advance(t0.Add(120 * time.Millisecond)) {
		t.Fatal("expected CursorSettleState.Advance to report change after deadline")
	}
	r, c, vis = cs.Observe(1, 2, true, t0.Add(120*time.Millisecond))
	if r != 1 || c != 2 || !vis {
		t.Fatalf("post-deadline settle = (%d,%d,%v), want (1,2,true)", r, c, vis)
	}
}

func TestPTYServerPreservesScrollbackAcrossFullClearsOnReattach(t *testing.T) {
	s := &ptyServer{
		clients:     map[net.Conn]struct{}{},
		clientReady: make(chan struct{}),
		lastRows:    6,
		lastCols:    40,
	}
	for i := 1; i <= 15; i++ {
		s.appendHistory([]byte(fmt.Sprintf("pre-clear-scroll-%02d\r\n", i)))
	}
	// Full screen clear wipes s.history, but s.vt retains all scrollback lines!
	s.appendHistory([]byte("\x1b[2J\x1b[Hpost-clear-screen-line"))

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	var replay bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&replay, clientConn)
		close(done)
	}()

	if !s.add(serverConn) {
		t.Fatal("s.add rejected connection")
	}
	_ = serverConn.Close()
	s.clientWG.Done()
	<-done

	var clientOut bytes.Buffer
	renderer := newClientSessionRenderer(&clientOut, 6, 40, nil)
	defer renderer.Close()
	if err := renderer.ProcessChunk(replay.Bytes()); err != nil {
		t.Fatal(err)
	}
	if !renderer.HasServerScrollback() {
		t.Fatal("client renderer did not detect server vterm scrollback")
	}
	lines := strings.Join(renderer.vt.DetectionLines(30), "\n")
	if !strings.Contains(lines, "pre-clear-scroll-01") || !strings.Contains(lines, "post-clear-screen-line") {
		t.Fatalf("reattached client missing pre-clear scrollback or post-clear screen: %q", lines)
	}
}

func TestLightOSWebShellSameSizeSIGWINCHRepaintsFullScrollbackFromVTerm(t *testing.T) {
	var out bytes.Buffer
	r := newClientSessionRenderer(&out, 6, 40, nil)
	defer r.Close()

	for i := 1; i <= 15; i++ {
		_ = r.ProcessChunk([]byte(fmt.Sprintf("webshell-scroll-%02d\r\n", i)))
	}
	_ = r.ProcessChunk([]byte("webshell-active-prompt"))
	out.Reset()

	// Simulate LightOS WebShell reopen with identical 6x40 dimensions.
	sizeChanged, hasScrollback, err := r.ResizeAndRepaint(6, 40)
	if err != nil {
		t.Fatal(err)
	}
	if sizeChanged {
		t.Fatal("expected sizeChanged=false for identical 6x40 dimensions")
	}
	if !hasScrollback {
		t.Fatal("expected hasScrollback=true")
	}
	repainted := out.String()
	if !strings.Contains(repainted, "webshell-scroll-01") || !strings.Contains(repainted, "webshell-active-prompt") {
		t.Fatalf("same-size WebShell reopen did not repaint full scrollback and screen: %q", repainted)
	}
}
