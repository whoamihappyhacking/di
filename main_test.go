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
	frame1 := []byte("\x1b[?25l\x1b[?7l\r\x1b[3Atop-0\r\x1b[K\ntop-1\r\x1b[K\nspin-1\r\x1b[K\nstatus-1\r\x1b[K\x1b[?7h\x1b[?25h")
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
	frame2 := []byte("\x1b[?25l\x1b[?7l\r\x1b[3Atop-0\r\x1b[K\ntop-1\r\x1b[K\nspin-2\r\x1b[K\nstatus-2\r\x1b[K\x1b[?7h\x1b[?25h")
	out2, skip2 := c.compact(frame2)
	if skip2 {
		t.Fatal("changed frame must not be skipped")
	}
	if bytes.Contains(out2, []byte("top-0")) || bytes.Contains(out2, []byte("top-1")) {
		t.Fatalf("compacted frame still contained unchanged top lines: %q", out2)
	}
	if !bytes.Contains(out2, []byte("\r\x1b[1Aspin-2\r\x1b[K\nstatus-2")) {
		t.Fatalf("compacted frame did not adjust cursor-up count to 1: %q", out2)
	}

	// Full clear resets compactor state.
	_, _ = c.compact([]byte("\x1b[2J\x1b[3J\x1b[Hfull-reset"))
	out3, skip3 := c.compact(frame2)
	if skip3 || !bytes.Equal(out3, frame2) {
		t.Fatalf("frame after full clear should not be diffed against pre-clear state: skip=%v out=%q", skip3, out3)
	}
}

func TestRedrawTrackerSignalsCheckpointRefreshAfterSoftLimit(t *testing.T) {
	tracker := newRedrawTracker()
	tracker.observe([]byte("\x1b[2J\x1b[3J\x1b[Hinitial-scrollback"))
	select {
	case <-tracker.refreshCh:
		t.Fatal("refreshCh should not fire immediately after clear")
	default:
	}

	chunk := bytes.Repeat([]byte("x"), scrollbackRefreshSoftBytes)
	tracker.observe(chunk)
	select {
	case <-tracker.refreshCh:
	default:
		t.Fatal("refreshCh should fire once bytesSinceClear reaches scrollbackRefreshSoftBytes")
	}

	// Subsequent clear resets bytesSinceClear.
	tracker.observe([]byte("\x1b[2J\x1b[3J\x1b[Hrefreshed"))
	bytesSince, _, _ := tracker.refreshStatus()
	if bytesSince >= scrollbackRefreshSoftBytes {
		t.Fatalf("bytesSinceClear was not reset by clear: %d", bytesSince)
	}
}
