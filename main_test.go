package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestMain points the session and state directories at a private location so
// no test can ever list, query, attach to or restore the user's real sessions.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "di-test-run-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_RUNTIME_DIR", dir)
	os.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	// Run inside a di session, d would ask the user's terminal to switch over
	// to whatever session a test starts.
	os.Unsetenv("DI_SESSION")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// ---- emulator and snapshot fidelity ----

type screenState struct {
	alt                 bool
	rows                []string
	styles              [][]vtermStyle
	curR, curC          int
	pendingWrap         bool
	cursorVis           bool
	scrollTop, scrollBo int
	originMode          bool
	autoWrap            bool
	savedR, savedC      int
	kitty               []uint8
	altKitty            []uint8
	bracketed, focus    bool
	mouse               int
	mouseSGR            bool
	appCursor, keypad   bool
	cursorShape         uint8
	style               vtermStyle
	title               string
	charsets            string
}

func captureState(vt *vterm) screenState {
	scr := vt.activeScreen()
	st := screenState{
		alt:         vt.inAltScreen,
		curR:        scr.curR,
		curC:        scr.curC,
		pendingWrap: scr.pendingWrap,
		cursorVis:   scr.cursorVis,
		scrollTop:   scr.scrollTop,
		scrollBo:    scr.scrollBottom,
		originMode:  scr.originMode,
		autoWrap:    scr.autoWrap,
		savedR:      scr.savedCurR,
		savedC:      scr.savedCurC,
		kitty:       append([]uint8(nil), vt.primary.kittyStack[:vt.primary.kittyLen]...),
		altKitty:    append([]uint8(nil), vt.alt.kittyStack[:vt.alt.kittyLen]...),
		bracketed:   vt.bracketedPaste,
		focus:       vt.focusEvents,
		mouse:       vt.mouseMode,
		mouseSGR:    vt.mouseSGR,
		appCursor:   vt.appCursorKeys,
		keypad:      vt.appKeypad,
		cursorShape: scr.cursorShape,
		style:       vt.style,
		title:       vt.title,
		charsets:    fmt.Sprintf("%c%c%d", vt.g0Charset, vt.g1Charset, vt.activeCharset),
	}
	for _, row := range scr.grid {
		st.rows = append(st.rows, row.plainText())
		styles := make([]vtermStyle, len(row.cells))
		for i, c := range row.cells {
			styles[i] = c.style
		}
		st.styles = append(st.styles, styles)
	}
	return st
}

func scrollbackText(vt *vterm) []string {
	var out []string
	for _, line := range vt.scrollback {
		out = append(out, cellsPlainText(decodeANSICells(line.ansi, line.width)))
	}
	return out
}

func assertSnapshotReplays(t *testing.T, rows, cols int, program string) {
	t.Helper()
	app := newVTerm(rows, cols)
	app.Write([]byte(program))
	host := newVTerm(rows, cols)
	// A shell session left output above the cursor, which sits on a fresh line.
	host.Write([]byte("leftover shell text\r\n\x1b[31mred prompt\x1b[0m\r\n"))
	host.Write(app.Snapshot())

	want, got := captureState(app), captureState(host)
	for r := range want.rows {
		if want.rows[r] != got.rows[r] {
			t.Errorf("row %d = %q, want %q", r, got.rows[r], want.rows[r])
		}
		for c := range want.styles[r] {
			if want.styles[r][c] != got.styles[r][c] {
				t.Errorf("row %d col %d style = %+v, want %+v", r, c, got.styles[r][c], want.styles[r][c])
				break
			}
		}
	}
	want.rows, got.rows, want.styles, got.styles = nil, nil, nil, nil
	if fmt.Sprintf("%+v", want) != fmt.Sprintf("%+v", got) {
		t.Errorf("terminal state differs\n got: %+v\nwant: %+v", got, want)
	}
	switch {
	case len(app.scrollback) > 0:
		if w, g := strings.Join(scrollbackText(app), "\n"), strings.Join(scrollbackText(host), "\n"); w != g {
			t.Errorf("scrollback differs\n got: %q\nwant: %q", g, w)
		}
	default:
		// Without session history to restore, the terminal keeps its own.
		if sb := scrollbackText(host); strings.Join(sb, "|") != "leftover shell text|red prompt" {
			t.Errorf("terminal history was not preserved exactly: %q", sb)
		}
	}
	// The same program continuing after the snapshot must keep both in sync.
	tail := "\x1b[2;3Hafter\x1b[K\r\nmore output ✅ 中文\r\n"
	app.Write([]byte(tail))
	host.Write([]byte(tail))
	if w, g := captureState(app).rows, captureState(host).rows; strings.Join(w, "\n") != strings.Join(g, "\n") {
		t.Errorf("screens diverge after continuing output\n got: %q\nwant: %q", g, w)
	}
}

func TestSnapshotReplaysShellOutputWithScrollbackAndWrapping(t *testing.T) {
	var p strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&p, "\x1b[3%dmline %02d\x1b[0m plain ✅ 中文 \x1b]8;;https://x.test\x1b\\link\x1b]8;;\x1b\\\r\n", i%8, i)
	}
	p.WriteString(strings.Repeat("long-wrapped-", 9) + "\r\n")
	p.WriteString("\x1b[44m blue background \x1b[K\x1b[0m\r\n")
	p.WriteString("\x1b[4:3;58:2::200:10:10mcurly\x1b[0m \x1b[4;58;5;33mplain\x1b[0m\r\n$ ")
	assertSnapshotReplays(t, 8, 40, p.String())
}

func TestSnapshotReplaysInlineAgentRedraws(t *testing.T) {
	var p strings.Builder
	p.WriteString("\x1b[?2004h\x1b[>1u\x1b[?1004h")
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&p, "⏺ assistant message %d with some text\r\n\r\n", i)
	}
	box := "╭──────────────────────────────╮\r\n│ > type here                  │\r\n╰──────────────────────────────╯\r\n  ? for shortcuts"
	p.WriteString(box)
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&p, "\x1b[?2026h\r\x1b[4A\x1b[J⠋ Working (%ds)\r\n%s\x1b[?2026l", i, box)
	}
	p.WriteString("\x1b[2A\x1b[5G")
	assertSnapshotReplays(t, 10, 34, p.String())
}

func TestSnapshotReplaysAltScreenModesAndSavedCursors(t *testing.T) {
	var p strings.Builder
	p.WriteString("primary before\r\nprompt$ \x1b[1;32m")
	p.WriteString("\x1b[>5u\x1b[?1049h\x1b[>3u\x1b[?1h\x1b=\x1b[?1002h\x1b[?1006h\x1b[5 q\x1b]2;my title\x07")
	p.WriteString("\x1b[H\x1b[2Jheader\x1b[3;9r\x1b[?6h\x1b[2;4Hin region\x1b7\x1b[5;1Hcursor here\x1b(0lqk\x1b(B")
	p.WriteString("\x1b[0;4;38;2;10;20;30mstyled")
	assertSnapshotReplays(t, 12, 30, p.String())
}

func TestSnapshotReplaysPendingWrapAndWideCharacters(t *testing.T) {
	p := "中文字符测试中文字符测试中文字符测试" + "\r\n" + strings.Repeat("x", 19) + "中" + "abcdefghijklmnopqrst"
	assertSnapshotReplays(t, 6, 20, p)
}

func TestSnapshotLeavesPrimaryCursorForAltScreenExit(t *testing.T) {
	app := newVTerm(6, 20)
	app.Write([]byte("one\r\ntwo\x1b[?1049h\x1b[Halt"))
	host := newVTerm(6, 20)
	host.Write(app.Snapshot())
	exit := []byte("\x1b[?1049lX")
	app.Write(exit)
	host.Write(exit)
	if w, g := captureState(app), captureState(host); w.curR != g.curR || w.curC != g.curC || w.rows[1] != g.rows[1] {
		t.Fatalf("after leaving alt screen host=(%d,%d %q) app=(%d,%d %q)", g.curR, g.curC, g.rows[1], w.curR, w.curC, w.rows[1])
	}
}

func TestEraseScrollbackMatchesRealTerminals(t *testing.T) {
	vt := newVTerm(3, 20)
	vt.Write([]byte("a\r\nb\r\nc\r\nd\r\ne"))
	if got := scrollbackText(vt); strings.Join(got, ",") != "a,b" {
		t.Fatalf("scrollback = %q", got)
	}
	// Agents clear and reprint their transcript on resize; nothing may be duplicated.
	vt.Write([]byte("\x1b[H\x1b[2J\x1b[3Ja\r\nb\r\nc\r\nd\r\ne"))
	if got := scrollbackText(vt); strings.Join(got, ",") != "a,b" {
		t.Fatalf("scrollback after reprint = %q", got)
	}
}

func TestResizeReflowsSoftWrappedLines(t *testing.T) {
	vt := newVTerm(4, 10)
	vt.Write([]byte("0123456789abcdef\r\nnext"))
	vt.Resize(4, 20)
	if got := vt.primary.grid[0].plainText(); got != "0123456789abcdef" {
		t.Fatalf("row 0 after widening = %q", got)
	}
	if vt.primary.curR != 1 || vt.primary.curC != 4 {
		t.Fatalf("cursor after widening = %d,%d", vt.primary.curR, vt.primary.curC)
	}
}

func TestRuneWidths(t *testing.T) {
	for r, want := range map[rune]int{'a': 1, '中': 2, '✅': 2, '❌': 2, '⚡': 2, '⭐': 2, '🔧': 2, '─': 1, '⏺': 1, '✻': 1, '\u0301': 0, '\u200d': 0, 'ｱ': 1, 'Ａ': 2} {
		if got := singleRuneWidth(r); got != want {
			t.Errorf("width(%q) = %d, want %d", r, got, want)
		}
	}
}

func TestEmulatorAnswersQueriesAndTracksNotifications(t *testing.T) {
	vt := newVTerm(5, 20)
	vt.Write([]byte("ab\x1b[6n\x1b[c\x1b[?2026$p\x1b[?u\x07\x1b]9;Agent needs you\x07\x1b]9;4;1;50\x07\x1b]7501;?\x07"))
	if got := string(vt.DrainReplies()); got != "\x1b[1;3R\x1b[?62;22c\x1b[?2026;2$y\x1b[?0u" {
		t.Fatalf("replies = %q", got)
	}
	if got := string(vt.DrainStatusReplies()); got != "\x1b]7501;?\x1b\\" {
		t.Fatalf("status replies = %q", got)
	}
	if vt.bells != 1 || vt.notices != 1 || vt.lastNotice != "Agent needs you" || vt.progressSeq != "4;1;50" {
		t.Fatalf("bells=%d notices=%d notice=%q progress=%q", vt.bells, vt.notices, vt.lastNotice, vt.progressSeq)
	}
}

func TestHostRestoreUndoesSessionModes(t *testing.T) {
	tracker := newVTerm(10, 40)
	tracker.Write([]byte("\x1b[>1u\x1b[>5u\x1b[?1049h\x1b[>3u\x1b[?2004h\x1b[?1003h"))
	got := hostRestoreSequence(tracker)
	for _, want := range []string{"\x1b[<1u\x1b[?1049l\x1b[<2u", "\x1b[?2004l", "\x1b[?1003l", "\x1b[?25h"} {
		if !strings.Contains(got, want) {
			t.Errorf("restore sequence %q missing %q", got, want)
		}
	}
	host := newVTerm(10, 40)
	host.Write([]byte("\x1b[>1u\x1b[>5u\x1b[?1049h\x1b[>3u\x1b[?2004h\x1b[?1003h"))
	host.Write([]byte(got))
	if host.inAltScreen || host.primary.kittyLen != 0 || host.alt.kittyLen != 0 || host.bracketedPaste || host.mouseMode != 0 {
		t.Fatalf("host still has session modes: alt=%v kitty=%d/%d paste=%v mouse=%d",
			host.inAltScreen, host.primary.kittyLen, host.alt.kittyLen, host.bracketedPaste, host.mouseMode)
	}
}

// ---- previews and summaries ----

func TestPreviewFitsNarrowWindow(t *testing.T) {
	vt := newVTerm(8, 60)
	vt.Write([]byte("older line\r\n"))
	vt.Write([]byte(strings.Repeat("─", 60) + "\r\n"))
	vt.Write([]byte("│ > prompt" + strings.Repeat(" ", 48) + "│\r\n"))
	vt.Write([]byte("⏺ " + strings.Repeat("word ", 14) + "\r\n"))
	lines := strings.Split(stripANSI(vt.Preview(30, 10)), "\n")
	for _, line := range lines {
		if w := runeDisplayWidth(line); w > 30 {
			t.Errorf("line %q is %d columns wide", line, w)
		}
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"older line", "│ > prompt", "│"} {
		if !strings.Contains(joined, want) {
			t.Errorf("preview missing %q:\n%s", want, joined)
		}
	}
	if got := strings.Count(strings.ReplaceAll(joined, "\n", ""), "word"); got != 14 {
		t.Errorf("wrapped text lost words: %d\n%s", got, joined)
	}
}

func stripANSI(s string) string {
	vt := newVTerm(1, maxVTermCols)
	var out []string
	for _, line := range strings.Split(s, "\n") {
		vt = newVTerm(1, maxVTermCols)
		vt.primary.autoWrap = false
		vt.Write([]byte(line))
		out = append(out, vt.primary.grid[0].plainText())
	}
	return strings.Join(out, "\n")
}

func TestSummarySkipsAgentChrome(t *testing.T) {
	vt := newVTerm(12, 60)
	vt.Write([]byte("⏺ Refactored the attach path.\r\n\r\n⠙ Running tests…\r\n╭────────╮\r\n│ >      │\r\n╰────────╯\r\n  ? for shortcuts\r\n"))
	activity, text := vt.Summary()
	if activity != "Running tests…" || text != "Refactored the attach path." {
		t.Fatalf("activity=%q text=%q", activity, text)
	}
	vt.Write([]byte("\x1b]0;✳ Fix login bug\x07"))
	if got := vt.CleanTitle(); got != "Fix login bug" {
		t.Fatalf("title = %q", got)
	}
}

// ---- state classification and picker ----

func TestActivityState(t *testing.T) {
	now := time.Now()
	seen := now.Add(-time.Minute)
	for _, tc := range []struct {
		name       string
		agent      string
		attention  bool
		attached   bool
		lastOutput time.Time
		want       string
	}{
		{"recent output", "", false, false, now.Add(-time.Second), "busy"},
		{"quiet with unseen output", "", false, false, now.Add(-10 * time.Second), "new"},
		{"quiet and seen", "", false, false, seen.Add(-time.Second), "idle"},
		{"bell while detached", "", true, false, now.Add(-10 * time.Second), "waiting"},
		{"attached is never new", "", false, true, now.Add(-10 * time.Second), "idle"},
		{"agent reports blocked", "state=blocked", false, false, now.Add(-time.Second), "waiting"},
		{"agent reports working", "state=working", false, false, seen.Add(-time.Hour), "busy"},
		{"agent reports idle", "state=idle", false, false, now.Add(-time.Second), "new"},
	} {
		if got := activityState(tc.agent, tc.attention, tc.attached, tc.lastOutput, seen, now); got != tc.want {
			t.Errorf("%s: state = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestSortSessionsPutsAttentionFirstAndCurrentLast(t *testing.T) {
	now := time.Now()
	list := []sessionEntry{
		{Sock: "current", Current: true, Status: sessionStatus{State: "waiting"}},
		{Sock: "idle-old", Status: sessionStatus{State: "idle", LastSeen: now.Add(-time.Hour)}},
		{Sock: "busy", Status: sessionStatus{State: "busy", LastSeen: now.Add(-2 * time.Hour)}},
		{Sock: "idle-recent", Status: sessionStatus{State: "idle", LastSeen: now.Add(-time.Minute)}},
		{Sock: "new", Status: sessionStatus{State: "new", LastOutput: now.Add(-time.Minute)}},
		{Sock: "waiting", Status: sessionStatus{State: "waiting", LastOutput: now.Add(-time.Hour)}},
	}
	sortSessions(list)
	var got []string
	for _, e := range list {
		got = append(got, e.Sock)
	}
	if strings.Join(got, ",") != "waiting,new,busy,idle-recent,idle-old,current" {
		t.Fatalf("order = %v", got)
	}
}

func TestPickerColumnsAlign(t *testing.T) {
	home, _ := os.UserHomeDir()
	now := time.Now()
	entries := []sessionEntry{
		{Sock: "/tmp/a.sock", Meta: sessionMeta{Command: []string{"codex", "--yolo"}, PWD: home + "/src/very/long/project/path/name"},
			Status: sessionStatus{State: "busy", Activity: "Running tests", LastOutput: now}},
		{Sock: "/tmp/b.sock", Meta: sessionMeta{Command: []string{"claude", "--dangerously-skip-permissions"}, PWD: home},
			Status: sessionStatus{State: "waiting", Notice: "Claude needs your permission", Title: "修复登录问题"}},
		{Sock: "/tmp/c.sock", Meta: sessionMeta{Command: []string{"make"}, PWD: "/srv"},
			Status: sessionStatus{State: "exited", ExitCode: 2, ExitedAt: now.Add(-3 * time.Hour)}},
	}
	width := -1
	for _, e := range entries {
		line := e.columns(100, false, now)
		if width >= 0 && runeDisplayWidth(line) != width {
			t.Errorf("line widths differ: %q", line)
		}
		width = runeDisplayWidth(line)
	}
	b := entries[1].columns(100, false, now)
	if !strings.Contains(b, "待处理") || !strings.Contains(b, "claude ") || strings.Contains(b, "dangerously") {
		t.Errorf("waiting line = %q", b)
	}
	if a := entries[0].columns(100, false, now); !strings.Contains(a, "…/") || !strings.Contains(a, "Running tests") {
		t.Errorf("busy line = %q", a)
	}
	if c := entries[2].columns(100, false, now); !strings.Contains(c, "[exit 2]") || !strings.Contains(c, "3h") {
		t.Errorf("exited line = %q", c)
	}
}

func TestFZFArgs(t *testing.T) {
	args := strings.Join(fzfArgs("/bin/d", "/run/di/x.sock", "", 40, false), "\n")
	for _, want := range []string{"--ansi", "--with-nth=2..", "--preview='/bin/d' --preview {1}", "--bind=ctrl-\\:down", "--current '/run/di/x.sock'", "--kill-confirm {1}", "--expect=ctrl-o"} {
		if !strings.Contains(args, want) {
			t.Errorf("fzf args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "--select-1") {
		t.Error("switching must always show the picker")
	}
	top := strings.Join(fzfArgs("/bin/d", "", "cod", 40, false), "\n")
	if !strings.Contains(top, "--query=cod") || !strings.Contains(top, "--select-1") {
		t.Errorf("top-level picker args:\n%s", top)
	}
	withInterrupted := strings.Join(fzfArgs("/bin/d", "", "cod", 40, true), "\n")
	if strings.Contains(withInterrupted, "--select-1") || !strings.Contains(withInterrupted, "Ctrl-O 恢复到目录") {
		t.Errorf("an interrupted session must never be picked without asking:\n%s", withInterrupted)
	}
}

func TestParsePickerOutput(t *testing.T) {
	for _, tc := range []struct{ out, key, selected string }{
		{"\n/run/di/a.sock\t● 运行中 ~\n", "", "/run/di/a.sock"},
		{"ctrl-o\n/state/di/restore/b.json\t⟲ 已中断 ~\n", "ctrl-o", "/state/di/restore/b.json"},
		{"", "", ""},
	} {
		if key, selected := parsePickerOutput(tc.out); key != tc.key || selected != tc.selected {
			t.Errorf("parsePickerOutput(%q) = %q, %q", tc.out, key, selected)
		}
	}
}

// ---- input keys ----

func TestInputKeyScanning(t *testing.T) {
	keys := func() *inputKeys { return &inputKeys{detach: 0x1d, switchKey: 0x1c} }
	for _, tc := range []struct {
		in     string
		fwd    string
		action attachEnd
		hit    bool
	}{
		{"\x1d", "", endDetach, true},
		{"abc\x1dxyz", "abc", endDetach, true},
		{"\x1c", "", endSwitch, true},
		{"ls\r", "ls\r", 0, false},
		{"x\x1b[93;5u", "x", endDetach, true},
		{"\x1b[93;5:1u", "", endDetach, true},
		{"\x1b[93;5:3u", "\x1b[93;5:3u", 0, false},
		{"\x1b[93;69u", "", endDetach, true},
		{"\x1b[93;7u", "\x1b[93;7u", 0, false},
		{"\x1b[27;5;92~", "", endSwitch, true},
		{"\x1b[A\x1b[<64;10;5M", "\x1b[A\x1b[<64;10;5M", 0, false},
		{"\x1b[200~paste \x1d here\x1b[201~", "\x1b[200~paste \x1d here\x1b[201~", 0, false},
		{"\x1b[200~paste\x1b[201~\x1d", "\x1b[200~paste\x1b[201~", endDetach, true},
	} {
		fwd, action, hit := keys().scan([]byte(tc.in))
		if string(fwd) != tc.fwd || action != tc.action || hit != tc.hit {
			t.Errorf("scan(%q) = %q,%v,%v want %q,%v,%v", tc.in, fwd, action, hit, tc.fwd, tc.action, tc.hit)
		}
	}
	// A paste split across reads stays a paste.
	k := keys()
	if _, _, hit := k.scan([]byte("\x1b[200~first \x1d")); hit {
		t.Fatal("detach key inside paste")
	}
	if _, _, hit := k.scan([]byte("second \x1d\x1b[201~")); hit {
		t.Fatal("detach key inside continued paste")
	}
	if _, action, hit := k.scan([]byte("\x1d")); !hit || action != endDetach {
		t.Fatal("detach key after paste ended")
	}
}

func TestParseCtrlKey(t *testing.T) {
	if parseCtrlKey("", defaultDetachKey) != 0x1d || parseCtrlKey("", defaultSwitchKey) != 0x1c || parseCtrlKey("^B", "") != 0x02 {
		t.Fatal("unexpected control key")
	}
}

// ---- server end to end ----

type testServer struct {
	sock string
	done chan error
}

func startTestServer(t *testing.T, args ...string) *testServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "di-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s := &testServer{sock: filepath.Join(dir, "s.sock"), done: make(chan error, 1)}
	t.Setenv("DI_INITIAL_ROWS", "12")
	t.Setenv("DI_INITIAL_COLS", "60")
	go func() { s.done <- runServer(append([]string{s.sock}, args...)) }()
	if err := waitSocket(s.sock, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *testServer) waitExit(t *testing.T) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not exit")
	}
	if isSocket(s.sock) {
		t.Fatal("socket left behind")
	}
}

type testClient struct {
	conn  net.Conn
	out   bytes.Buffer
	exit  *exitInfo
	hello helloInfo
	gone  bool
}

func attachTestClient(t *testing.T, sock string, rows, cols int) *testClient {
	t.Helper()
	conn, err := dialSession(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := writeFrame(conn, frameAttach, sizePayload(rows, cols)); err != nil {
		t.Fatal(err)
	}
	c := &testClient{conn: conn}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	typ, payload, err := readFrame(conn)
	if err != nil || typ != frameHello {
		t.Fatalf("hello: %c %v", typ, err)
	}
	_ = json.Unmarshal(payload, &c.hello)
	return c
}

// readUntil collects output frames until want appears (or the session ends).
func (c *testClient) readUntil(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(c.out.String(), want) {
		if c.exit != nil || c.gone {
			t.Fatalf("session ended before %q; output %q", want, c.out.String())
		}
		_ = c.conn.SetReadDeadline(deadline)
		typ, payload, err := readFrame(c.conn)
		if err != nil {
			t.Fatalf("waiting for %q: %v; output %q", want, err, c.out.String())
		}
		c.handle(typ, payload)
	}
}

func (c *testClient) readToEnd(t *testing.T) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for c.exit == nil && !c.gone {
		typ, payload, err := readFrame(c.conn)
		if err != nil {
			c.gone = true
			return
		}
		c.handle(typ, payload)
	}
}

func (c *testClient) handle(typ byte, payload []byte) {
	switch typ {
	case frameOutput:
		c.out.Write(payload)
	case frameExit:
		c.exit = &exitInfo{}
		_ = json.Unmarshal(payload, c.exit)
	case frameDetach:
		c.gone = true
	}
}

func (c *testClient) send(t *testing.T, input string) {
	t.Helper()
	if err := writeFrame(c.conn, frameInput, []byte(input)); err != nil {
		t.Fatal(err)
	}
}

func runServerAndRead(t *testing.T, args []string) []byte {
	t.Helper()
	s := startTestServer(t, args...)
	c := attachTestClient(t, s.sock, 12, 60)
	c.readToEnd(t)
	s.waitExit(t)
	return c.out.Bytes()
}

func TestServerPassesOutputThroughAndRestoresOnReattach(t *testing.T) {
	s := startTestServer(t, "sh", "-c", `printf 'READY\n'; while read -r line; do printf '\033[32mgot:%s\033[0m\n' "$line"; done`)
	c := attachTestClient(t, s.sock, 12, 60)
	c.readUntil(t, "READY")
	c.send(t, "hello\r")
	c.readUntil(t, "\x1b[32mgot:hello\x1b[0m")
	c.conn.Close()

	st, err := querySession(s.sock, 0, 0)
	if err != nil || st.Attached != 0 || st.Rows != 12 || st.Cols != 60 {
		t.Fatalf("status after detach: %+v %v", st, err)
	}

	c2 := attachTestClient(t, s.sock, 10, 50)
	c2.readUntil(t, "\x1b[?2026l")
	host := newVTerm(10, 50)
	host.Write(c2.out.Bytes())
	if screen := strings.Join(captureState(host).rows, "\n"); !strings.Contains(screen, "got:hello") || !strings.Contains(screen, "READY") {
		t.Fatalf("reattach snapshot screen:\n%s", screen)
	}
	if st, _ := querySession(s.sock, 0, 0); st.Rows != 10 || st.Cols != 50 || st.Attached != 1 {
		t.Fatalf("reattach did not take the client size: %+v", st)
	}
	c2.send(t, "again\r")
	c2.readUntil(t, "got:again")
	c2.send(t, "\x04")
	c2.readToEnd(t)
	if c2.exit == nil || c2.exit.Code != 0 {
		t.Fatalf("exit frame = %+v", c2.exit)
	}
	s.waitExit(t)
}

func TestServerKeepsExitedSessionUntilViewed(t *testing.T) {
	s := startTestServer(t, "sh", "-c", `read -r x; printf 'final words\n'; exit 3`)
	c := attachTestClient(t, s.sock, 12, 60)
	c.readUntil(t, "\x1b[?2026l")
	c.send(t, "go\r")
	c.conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	var st sessionStatus
	for time.Now().Before(deadline) {
		st, _ = querySession(s.sock, 6, 40)
		if st.State == "exited" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.State != "exited" || st.ExitCode != 3 || !strings.Contains(st.Screen, "final words") {
		t.Fatalf("status = %+v", st)
	}
	viewer := attachTestClient(t, s.sock, 12, 60)
	if !viewer.hello.Exited {
		t.Fatal("hello does not report the exit")
	}
	viewer.readToEnd(t)
	if viewer.exit == nil || viewer.exit.Code != 3 || !strings.Contains(viewer.out.String(), "final words") {
		t.Fatalf("viewer got exit=%+v output=%q", viewer.exit, viewer.out.String())
	}
	s.waitExit(t)
}

func TestServerStartsProgramAfterAttachSoQueriesReachTerminal(t *testing.T) {
	s := startTestServer(t, "sh", "-c", `printf 'Q\033[6n'; IFS= read -r -d R reply; printf 'reply=%s\n' "$reply" | tr -d '\033'`)
	c := attachTestClient(t, s.sock, 12, 60)
	c.readUntil(t, "Q\x1b[6n")
	c.send(t, "\x1b[7;9R")
	c.readUntil(t, "reply=[7;9")
	c.readToEnd(t)
	s.waitExit(t)
}

func TestServerAnswersQueriesWhileDetached(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	s := startTestServer(t, "bash", "-c", `sleep 0.3; printf 'ab\033[6n'; IFS= read -r -d R reply; printf 'reply=%s\n' "${reply#?}"; sleep 5`)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := querySession(s.sock, 5, 40)
		if err == nil && strings.Contains(st.Screen, "reply=[1;3") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	st, _ := querySession(s.sock, 5, 40)
	t.Fatalf("query was not answered while detached: %q", st.Screen)
}

func TestServerMarksBellWhileDetachedAsWaiting(t *testing.T) {
	s := startTestServer(t, "sh", "-c", `read -r x; sleep 0.2; printf 'need input\a'; sleep 10`)
	c := attachTestClient(t, s.sock, 12, 60)
	c.readUntil(t, "\x1b[?2026l")
	c.send(t, "\r")
	c.conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := querySession(s.sock, 0, 0); st.State == "waiting" {
			c2 := attachTestClient(t, s.sock, 12, 60)
			c2.readUntil(t, "\x1b[?2026l")
			if st, _ := querySession(s.sock, 0, 0); st.State == "waiting" {
				t.Fatal("attaching did not clear the attention flag")
			}
			_ = killSession(s.sock)
			s.waitExit(t)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("bell while detached did not mark the session as waiting")
}

func TestServerKill(t *testing.T) {
	s := startTestServer(t, "sh", "-c", `trap '' TERM; printf 'stubborn\n'; while :; do sleep 1; done`)
	c := attachTestClient(t, s.sock, 12, 60)
	c.readUntil(t, "stubborn")
	if err := killSession(s.sock); err != nil {
		t.Fatal(err)
	}
	c.readToEnd(t)
	if c.exit == nil {
		t.Fatal("attached client was not told the session ended")
	}
	s.waitExit(t)
}

func TestServerDetachAll(t *testing.T) {
	s := startTestServer(t, "sh", "-c", `printf 'up\n'; sleep 10`)
	c := attachTestClient(t, s.sock, 12, 60)
	c.readUntil(t, "up")
	if err := detachSession(s.sock); err != nil {
		t.Fatal(err)
	}
	c.readToEnd(t)
	if !c.gone || c.exit != nil {
		t.Fatalf("client gone=%v exit=%v", c.gone, c.exit)
	}
	_ = killSession(s.sock)
	s.waitExit(t)
}

func TestServerSwitchRequestMovesAttachedClients(t *testing.T) {
	s := startTestServer(t, "sh", "-c", `printf 'shell\n'; sleep 10`)
	if requestSwitch(s.sock, "/elsewhere.sock") {
		t.Fatal("switch reported success with nobody attached")
	}
	c := attachTestClient(t, s.sock, 12, 60)
	c.readUntil(t, "shell")
	target := filepath.Join(filepath.Dir(s.sock), "other.sock")
	if !requestSwitch(s.sock, target) {
		t.Fatal("switch request was not delivered")
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		typ, payload, err := readFrame(c.conn)
		if err != nil {
			t.Fatal(err)
		}
		if typ == frameSwitch {
			if string(payload) != target {
				t.Fatalf("switch target = %q", payload)
			}
			break
		}
	}
	_ = killSession(s.sock)
	s.waitExit(t)
}

func TestServerSupportsClientsFromOlderBuilds(t *testing.T) {
	s := startTestServer(t, "sh", "-c", `printf 'legacy ok\n'; read -r x; printf 'echo:%s\n' "$x"`)
	conn, err := dialSession(s.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := writeFrame(conn, frameResize, sizePayload(12, 60)); err != nil {
		t.Fatal(err)
	}
	_ = writeFrame(conn, frameInput, []byte("old\r"))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	data, _ := io.ReadAll(conn)
	if !strings.Contains(string(data), "echo:old") || data[0] != 0x1b {
		t.Fatalf("legacy client stream = %q", data)
	}
	s.waitExit(t)
}

func TestServerReportsMissingCommand(t *testing.T) {
	out := runServerAndRead(t, []string{"/nonexistent/di-test-command"})
	if !strings.Contains(string(out), "di: ") {
		t.Fatalf("output = %q", out)
	}
}

func TestRunServerKeepsPTYForDescendant(t *testing.T) {
	// Like a real terminal, the PTY stays open while a descendant that survived
	// its parent's hangup still holds it.
	out := runServerAndRead(t, []string{"sh", "-c", `trap '' HUP; (sleep 0.3; printf 'descendant\n') & printf 'parent\n'`})
	if !strings.Contains(string(out), "descendant") {
		t.Fatalf("output = %q", out)
	}
}

func TestQuerySessionReadsOlderServers(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "old.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(conn, "\x1b]7502;vterm=1;snap=begin;rows=6;cols=30\x07\x1b[H\x1b[2Jold session text\r\n\x1b]7501;state=working\x07\x1b]7502;snap=end\x07")
			time.Sleep(50 * time.Millisecond)
			conn.Close()
		}
	}()
	st, err := querySession(sock, 4, 30)
	if err != nil || !st.Legacy || st.State != "busy" || !strings.Contains(st.Screen, "old session text") || st.Cols != 30 {
		t.Fatalf("legacy status = %+v, %v", st, err)
	}
}

// ---- attach client in a real pty ----

func TestAttachClientDetachesAndRestoresTerminal(t *testing.T) {
	s := startTestServer(t, "sh", "-c", `printf '\033[?2004h\033[>1u\033[?1049hFULLSCREEN\n'; sleep 30`)
	cmd := exec.Command(os.Args[0], "-test.run=^TestAttachClientHelper$")
	cmd.Env = append(os.Environ(), "DI_ATTACH_TEST_SOCK="+s.sock, "D_DETACH=^]")
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 12, Cols: 60})
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	var output bytes.Buffer
	readDone := make(chan struct{})
	sentDetach := false
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			output.Write(buf[:n])
			if !sentDetach && bytes.Contains(output.Bytes(), []byte("FULLSCREEN")) {
				sentDetach = true
				_, _ = master.Write([]byte("\x1b[93;5u"))
			}
			if err != nil {
				return
			}
		}
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("attach client: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("attach did not exit; output %q", output.String())
	}
	<-readDone
	out := output.String()
	full := strings.LastIndex(out, "FULLSCREEN")
	restore := strings.LastIndex(out, "\x1b[<1u")
	if full < 0 || restore < full || !strings.Contains(out[restore-20:], "\x1b[?1049l") || !strings.Contains(out[restore:], "\x1b[?2004l") {
		t.Fatalf("missing ordered mode restore: %q", out)
	}
	if !strings.Contains(out, "[di] detached") {
		t.Fatalf("missing detach message: %q", out)
	}
	if st, err := querySession(s.sock, 0, 0); err != nil || st.Attached != 0 {
		t.Fatalf("session after detach: %+v %v", st, err)
	}
	_ = killSession(s.sock)
	s.waitExit(t)
}

func TestAttachClientHelper(t *testing.T) {
	sock := os.Getenv("DI_ATTACH_TEST_SOCK")
	if sock == "" {
		return
	}
	if err := attachSession(sock); err != nil {
		t.Fatal(err)
	}
}

// ---- session files ----

func TestUniqueSocketPathFitsPortableLimit(t *testing.T) {
	base, err := os.MkdirTemp("", "u")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	dir := filepath.Join(base, strings.Repeat("d", 20))
	sock, err := uniqueSocketPath(dir, strings.Repeat("label-", 30))
	if err != nil {
		t.Fatal(err)
	}
	if len(sock) > maxSocketPathLen {
		t.Fatalf("socket path is %d bytes", len(sock))
	}
	if got := cleanSessionName("", sock); !strings.HasPrefix(got, "label-") || strings.ContainsAny(got, "0123456789") {
		t.Fatalf("clean name = %q", got)
	}
}

func TestUniqueSocketPathRejectsLongDirectory(t *testing.T) {
	if _, err := uniqueSocketPath("/"+strings.Repeat("x", 120), "label"); err == nil {
		t.Fatal("expected error for long directory")
	}
}

func TestCleanSessionName(t *testing.T) {
	for in, want := range map[string]string{
		"agy---dangerously-skip-permissions-dmii0el5j50w-3531216": "agy",
		"codex---yolo-dmkr3kzx61zd-3313217":                       "codex---yolo",
		"make-test-dmkr3kzx61zd-12-1":                             "make-test",
	} {
		if got := cleanSessionName(in, ""); got != want {
			t.Errorf("cleanSessionName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	sessions := filepath.Join(dir, "di")
	_ = os.MkdirAll(sessions, 0o700)
	for _, name := range []string{"codex---yolo-aaaaaaaaaa-1", "claude-bbbbbbbbbb-2", "claude-cccccccccc-3"} {
		ln, err := net.Listen("unix", filepath.Join(sessions, name+".sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
	}
	if got, err := resolveSession("codex"); err != nil || sessionName(got) != "codex---yolo-aaaaaaaaaa-1" {
		t.Fatalf("prefix: %q %v", got, err)
	}
	if got, err := resolveSession("cccc"); err != nil || sessionName(got) != "claude-cccccccccc-3" {
		t.Fatalf("substring: %q %v", got, err)
	}
	if _, err := resolveSession("claude"); err == nil || !strings.Contains(err.Error(), "several") {
		t.Fatalf("ambiguous: %v", err)
	}
	if _, err := resolveSession("nope"); err == nil {
		t.Fatal("expected no match")
	}
}

func TestParseAliasOutputBashAndZshFormats(t *testing.T) {
	got := parseAliasOutput([]byte("alias ll='ls -l'\nalias say='echo '\\''hi'\\'''\nzz='echo z'\n"))
	if got["ll"] != "ls -l" || got["say"] != "echo 'hi'" || got["zz"] != "echo z" {
		t.Fatalf("aliases = %#v", got)
	}
}

func TestParseAliasOutputSkipsNoise(t *testing.T) {
	got := parseAliasOutput([]byte("\nnot an alias\n=bad\nalias ok=yes\n"))
	if len(got) != 1 || got["ok"] != "yes" {
		t.Fatalf("aliases = %#v", got)
	}
}

func TestShellAliasCmdDetachesFromTerminal(t *testing.T) {
	cmd := shellAliasCmd(t.Context(), "/bin/sh")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("alias query must not own the terminal")
	}
}

func TestSnapshotAtEverySplitPointConverges(t *testing.T) {
	program := "hello \x1b[38;2;10;20;30mcolor\x1b[0m 中文✅ \x1b]2;the title\x07after \x1b_Gf=100;AAAABBBB\x1b\\done\r\n\x1b[1;31mbold\x1b[0m"
	for split := 0; split <= len(program); split++ {
		app := newVTerm(6, 40)
		app.Write([]byte(program[:split]))
		host := newVTerm(6, 40)
		host.Write(app.Snapshot())
		app.Write([]byte(program[split:]))
		host.Write([]byte(program[split:]))
		w, g := captureState(app), captureState(host)
		if strings.Join(w.rows, "\n") != strings.Join(g.rows, "\n") || w.title != g.title || w.style != g.style {
			t.Fatalf("split at %d (%q | %q):\n got %q title %q\nwant %q title %q", split, program[:split], program[split:], g.rows[:2], g.title, w.rows[:2], w.title)
		}
	}
}

func TestGlobalModesSurviveScreenSwitches(t *testing.T) {
	assertSnapshotReplays(t, 4, 10, "\x1b[?7l\x1b[?25l\x1b[4 q\x1b[?1049hABCDEFGHIJKLM")
	vt := newVTerm(4, 10)
	vt.Write([]byte("\x1b[?7l\x1b[?25l\x1b[?1049hABCDEFGHIJKLM"))
	if got := vt.alt.grid[0].plainText(); got != "ABCDEFGHIM" || vt.alt.cursorVis {
		t.Fatalf("alt screen ignored global modes: %q visible=%v", got, vt.alt.cursorVis)
	}
}

func TestAltScreen1047SharesCursor(t *testing.T) {
	app := newVTerm(6, 20)
	app.Write([]byte("abc\x1b[?1047h\x1b[4;5Hin alt"))
	host := newVTerm(6, 20)
	host.Write(app.Snapshot())
	for _, vt := range []*vterm{app, host} {
		vt.Write([]byte("\x1b[?1047lX"))
	}
	if a, h := captureState(app), captureState(host); a.curR != h.curR || a.curC != h.curC || a.curR != 3 {
		t.Fatalf("cursor after 1047l: app %d,%d host %d,%d", a.curR, a.curC, h.curR, h.curC)
	}
}

func TestScrollbackIsBoundedByBytes(t *testing.T) {
	vt := newVTerm(4, 1000)
	line := strings.Repeat("\x1b[31mx\x1b[32my", 400) + "\r\n"
	for i := 0; i < 3000; i++ {
		vt.Write([]byte(line))
	}
	total := 0
	for _, l := range vt.scrollback {
		total += len(l.ansi)
	}
	if total != vt.scrollbackBytes || total > maxScrollbackBytes+maxScrollbackBytes/8 {
		t.Fatalf("scrollback holds %d bytes (tracked %d)", total, vt.scrollbackBytes)
	}
	if snap := vt.Snapshot(); len(snap) > 2*maxScrollbackBytes {
		t.Fatalf("snapshot is %d bytes", len(snap))
	}
}

func TestStripTerminalReplies(t *testing.T) {
	in := "a\x1b[12;40Rb\x1b[?62;22c\x1b[>1;10;0c\x1b[?2026;2$y\x1b[?1u\x1b[0n\x1b]11;rgb:0000/0000/0000\x07\x1bP1$r0m\x1b\\\x1b[8;24;80tc"
	if got := string(stripTerminalReplies([]byte(in))); got != "abc" {
		t.Fatalf("got %q", got)
	}
	keys := "\x1b[A\x1b[93;5u\x1b[200~text\x1b[201~\x1b[<0;3;4M\x1b[13~\x1bOR"
	if got := string(stripTerminalReplies([]byte(keys))); got != keys {
		t.Fatalf("keyboard input altered: %q", got)
	}
}

func TestOnlyPrimaryClientAnswersQueries(t *testing.T) {
	s := startTestServer(t, "sh", "-c", `stty -echo; while IFS= read -r line; do printf 'in:%s\n' "$line" | tr -d '\033'; done`)
	first := attachTestClient(t, s.sock, 12, 60)
	first.readUntil(t, "\x1b[?2026l")
	second := attachTestClient(t, s.sock, 12, 60)
	second.readUntil(t, "\x1b[?2026l")
	first.send(t, "\x1b[3;4Rfirst\r")
	second.send(t, "\x1b[3;4Rsecond\r")
	// The first terminal is no longer primary: its reply is dropped, its typing is not.
	second.readUntil(t, "in:first\r")
	second.readUntil(t, "in:[3;4Rsecond\r")
	_ = killSession(s.sock)
	s.waitExit(t)
}

func TestInputKeyScanningAcrossReads(t *testing.T) {
	k := &inputKeys{detach: 0x1d, switchKey: 0x1c}
	steps := []struct {
		in     string
		fwd    string
		action attachEnd
		hit    bool
	}{
		{"\x1b[200~abc\x1b[20", "\x1b[200~abc\x1b[20", 0, false},
		{"1~", "1~", 0, false},
		{"\x1d", "", endDetach, true},
	}
	for i, st := range steps {
		fwd, action, hit := k.scan([]byte(st.in))
		if string(fwd) != st.fwd || action != st.action || hit != st.hit {
			t.Fatalf("step %d: %q,%v,%v", i, fwd, action, hit)
		}
	}
	k = &inputKeys{detach: 0x1d, switchKey: 0x1c}
	if _, _, hit := k.scan([]byte("x\x1b[20")); hit {
		t.Fatal("unexpected hit")
	}
	if _, _, hit := k.scan([]byte("0~paste \x1d")); hit {
		t.Fatal("detach key inside a paste whose start marker was split")
	}
	k = &inputKeys{detach: 0x1d, switchKey: 0x1c}
	if fwd, _, _ := k.scan([]byte("ab\x1b[93;")); string(fwd) != "ab\x1b[93;" {
		t.Fatalf("forwarded %q", fwd)
	}
	if fwd, action, hit := k.scan([]byte("5u")); !hit || action != endDetach || len(fwd) != 0 {
		t.Fatalf("split kitty detach: %q %v %v", fwd, action, hit)
	}
}

func TestServerKillEscalatesForStubbornPrograms(t *testing.T) {
	s := startTestServer(t, "sh", "-c", `trap '' HUP TERM; printf 'ready\n'; while :; do sleep 1; done`)
	c := attachTestClient(t, s.sock, 12, 60)
	c.readUntil(t, "ready")
	st, _ := querySession(s.sock, 0, 0)
	start := time.Now()
	if err := killSession(s.sock); err != nil {
		t.Fatal(err)
	}
	s.waitExit(t)
	if err := syscall.Kill(st.PID, 0); err == nil {
		t.Fatalf("program %d survived kill", st.PID)
	}
	t.Logf("killed in %v", time.Since(start))
}

func TestHostRestoreKeepsCursorWhereProgramLeftIt(t *testing.T) {
	for _, tc := range []struct {
		name, program string
	}{
		{"plain", "line 1\r\nline 2\r\n  Goodbye!\r\n"},
		{"margins and origin mode", "a\r\nb\r\nc\x1b[2;8r\x1b[?6h\x1b[3;4Hmid"},
		{"exit while in alt screen", "shell$ cmd\r\n\x1b[?1049h\x1b[Hfull screen app\x1b[5;5H"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker, host := newVTerm(10, 30), newVTerm(10, 30)
			tracker.Write([]byte(tc.program))
			host.Write([]byte(tc.program))
			want := newVTerm(10, 30)
			want.Write([]byte(tc.program))
			if want.inAltScreen {
				want.Write([]byte("\x1b[?1049l"))
			}
			host.Write([]byte(hostRestoreSequence(tracker)))
			scr := host.activeScreen()
			if host.inAltScreen || scr.curR != want.primary.curR || scr.curC != want.primary.curC {
				t.Fatalf("cursor at %d,%d (alt=%v), want %d,%d", scr.curR, scr.curC, host.inAltScreen, want.primary.curR, want.primary.curC)
			}
			if scr.scrollTop != 0 || scr.scrollBottom != scr.rows-1 || scr.originMode {
				t.Fatal("margins or origin mode not reset")
			}
		})
	}
}

// slowWriter stands in for a terminal that renders slowly.
type slowWriter struct {
	mu sync.Mutex
	n  int
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(time.Duration(len(p)/4096+1) * 5 * time.Millisecond)
	w.mu.Lock()
	w.n += len(p)
	w.mu.Unlock()
	return len(p), nil
}

func TestLegacyServerDoesNotDropClientBehindSlowTerminal(t *testing.T) {
	// Older servers write each chunk with a 100ms deadline and drop the client
	// when it is exceeded.
	sock := filepath.Join(t.TempDir(), "old.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	const total = 4 << 20
	dropped := make(chan bool, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		chunk := bytes.Repeat([]byte("transcript line ✅\r\n"), 64<<10/22)
		for sent := 0; sent < total; sent += len(chunk) {
			_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
			if _, err := conn.Write(chunk); err != nil {
				dropped <- true
				return
			}
		}
		dropped <- false
		time.Sleep(200 * time.Millisecond)
	}()
	conn, err := dialSession(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream := &attachStream{conn: conn, legacy: true}
	w := &slowWriter{}
	out := &hostWriter{tracker: newVTerm(24, 80), w: w}
	go stream.copyTo(out)
	if <-dropped {
		t.Fatal("the old server dropped the client")
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		w.mu.Lock()
		n := w.n
		w.mu.Unlock()
		if n >= total-64<<10 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal received %d of %d bytes", n, total)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLegacySessionEndedDetectsServerExit(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "old.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if legacySessionEnded(sock) {
		t.Fatal("a live session was reported as ended")
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		ln.Close() // removes the socket, as older servers do when the program exits
	}()
	if !legacySessionEnded(sock) {
		t.Fatal("ended session not detected")
	}
}
