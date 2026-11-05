package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/term"
)

type sessionEntry struct {
	Sock    string
	Meta    sessionMeta
	Status  sessionStatus
	Current bool
}

// querySession asks a session for its status, plus a rendered preview when
// previewRows > 0. Sessions run by older di builds answer every connection
// with a full snapshot instead; that is replayed locally to the same result.
func querySession(sock string, previewRows, previewCols int) (sessionStatus, error) {
	conn, err := dialSession(sock)
	if err != nil {
		return sessionStatus{}, err
	}
	defer conn.Close()
	request, reply, payload := byte(frameStatus), byte(frameReply), []byte(nil)
	if previewRows > 0 {
		request, reply, payload = framePreview, framePreviewReply, sizePayload(previewRows, previewCols)
	}
	if err := writeFrame(conn, request, payload); err != nil {
		return sessionStatus{}, err
	}
	head, _ := readHead(conn, 5, 1500*time.Millisecond)
	if len(head) == 5 && head[0] == reply {
		n := int(uint32(head[1])<<24 | uint32(head[2])<<16 | uint32(head[3])<<8 | uint32(head[4]))
		if n <= maxFrameLen {
			body := make([]byte, n)
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := readFull(conn, body); err == nil {
				var st sessionStatus
				if json.Unmarshal(body, &st) == nil {
					return st, nil
				}
			}
		}
	}
	snapshot := append(head, readLegacySnapshot(conn)...)
	return legacyStatus(snapshot, previewRows, previewCols), nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	got := 0
	for got < len(buf) {
		n, err := conn.Read(buf[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}

func readLegacySnapshot(conn net.Conn) []byte {
	end := []byte("\x1b]7502;snap=end\x07")
	deadline := time.Now().Add(1500 * time.Millisecond)
	_ = conn.SetReadDeadline(deadline)
	var out []byte
	buf := make([]byte, 64<<10)
	for len(out) < 32<<20 {
		n, err := conn.Read(buf)
		out = append(out, buf[:n]...)
		if bytes.Contains(out[max(len(out)-n-len(end), 0):], end) {
			break
		}
		if err != nil {
			break
		}
		// Snapshots from servers without an end marker arrive in one burst.
		_ = conn.SetReadDeadline(minTime(deadline, time.Now().Add(100*time.Millisecond)))
	}
	return out
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func legacyStatus(snapshot []byte, previewRows, previewCols int) sessionStatus {
	vt := newVTerm(defaultVTermRows, defaultVTermCols)
	vt.Write(snapshot)
	st := sessionStatus{Legacy: true, State: "idle", Title: vt.CleanTitle()}
	st.Activity, st.Summary = vt.Summary()
	switch {
	case agentStateIs(vt.agentState, "blocked"):
		st.State = "waiting"
	case agentStateIs(vt.agentState, "working") || st.Activity != "":
		st.State = "busy"
	}
	st.Rows, st.Cols = vt.Size()
	if previewRows > 0 {
		st.Screen = vt.Preview(previewCols, previewRows)
	}
	return st
}

func collectSessions(currentSock string) ([]sessionEntry, error) {
	dir, err := sessionDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var socks []string
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if strings.HasSuffix(e.Name(), ".sock") && isSocket(path) {
			socks = append(socks, path)
		}
	}
	results := make([]*sessionEntry, len(socks))
	var wg sync.WaitGroup
	for i, sock := range socks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := querySession(sock, 0, 0)
			if err != nil {
				if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
					removeSessionFiles(sock)
				}
				return
			}
			results[i] = &sessionEntry{
				Sock:    sock,
				Meta:    readSessionMeta(sock),
				Status:  st,
				Current: currentSock != "" && sameSession(sock, currentSock),
			}
		}()
	}
	wg.Wait()
	var list []sessionEntry
	for _, r := range results {
		if r != nil {
			list = append(list, *r)
		}
	}
	sortSessions(list)
	return list, nil
}

func stateRank(state string) int {
	switch state {
	case "waiting":
		return 0
	case "new", "exited":
		return 1
	case "busy":
		return 2
	default:
		return 3
	}
}

// sortSessions puts sessions that need you first (most recent first), then
// the rest by when you last looked at them, and the current session last.
func sortSessions(list []sessionEntry) {
	recency := func(e sessionEntry) time.Time {
		st := e.Status
		var t time.Time
		if stateRank(st.State) <= 1 {
			t = st.LastOutput
			if st.ExitedAt.After(t) {
				t = st.ExitedAt
			}
		} else {
			t = st.LastSeen
		}
		if t.IsZero() {
			t, _ = time.Parse(time.RFC3339, e.Meta.StartedAt)
		}
		return t
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Current != b.Current {
			return !a.Current
		}
		if ra, rb := stateRank(a.Status.State), stateRank(b.Status.State); ra != rb {
			return ra < rb
		}
		return recency(a).After(recency(b))
	})
}

type stateStyle struct {
	icon, word, color string
}

func styleForState(st sessionStatus, current bool) stateStyle {
	if current {
		return stateStyle{"▶", "当前", "1"}
	}
	switch st.State {
	case "busy":
		return stateStyle{"●", "运行中", "33"}
	case "waiting":
		return stateStyle{"◆", "待处理", "1;35"}
	case "new":
		return stateStyle{"◆", "有更新", "1;36"}
	case "exited":
		return stateStyle{"✕", "已退出", "31"}
	default:
		return stateStyle{"○", "空闲", "2"}
	}
}

// displayText makes program-controlled text safe for one fixed-width line.
func displayText(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return r < 0x20 || (r >= 0x7f && r < 0xa0) || unicode.IsSpace(r)
	}), " ")
}

func (e sessionEntry) summaryText() string {
	st := e.Status
	var parts []string
	if st.State == "exited" && st.ExitCode != 0 {
		parts = append(parts, fmt.Sprintf("[exit %d]", st.ExitCode))
	}
	if st.Notice != "" {
		parts = append(parts, st.Notice)
	}
	if st.Title != "" && !strings.EqualFold(st.Title, filepath.Base(firstArg(e.Meta.Command))) {
		parts = append(parts, st.Title)
	}
	switch {
	case st.Activity != "" && st.State == "busy":
		parts = append(parts, st.Activity)
	case st.Summary != "":
		parts = append(parts, st.Summary)
	}
	return displayText(strings.Join(parts, " · "))
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func (e sessionEntry) age(now time.Time) string {
	st := e.Status
	t := st.LastOutput
	if st.State == "exited" && !st.ExitedAt.IsZero() {
		t = st.ExitedAt
	}
	if t.IsZero() {
		return "-"
	}
	return compactDuration(now.Sub(t))
}

func compactDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func colorize(s, code string, color bool) string {
	if !color || code == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// columns renders the visible part of a session line in fixed-width columns.
func (e sessionEntry) columns(width int, color bool, now time.Time) string {
	if width <= 0 {
		width = 120
	}
	avail := max(width-4, 60)
	dirW := clampInt(avail*22/100, 14, 30)
	cmdW := clampInt(avail*16/100, 10, 24)
	const stateW, ageW = 8, 4
	sumW := max(avail-stateW-dirW-cmdW-ageW-4, 10)

	style := styleForState(e.Status, e.Current)
	state := padDisplayWidth(style.icon+" "+style.word, stateW)
	name := cleanSessionName(e.Meta.Name, e.Sock)
	dir := padDisplayWidth(fitPathLeft(displayText(formatDisplayPWD(e.Meta.PWD)), dirW), dirW)
	cmd := padDisplayWidth(fitField(displayText(formatDisplayCommand(e.Meta.Command, name)), cmdW), cmdW)
	summary := padDisplayWidth(fitField(e.summaryText(), sumW), sumW)
	age := fmt.Sprintf("%*s", ageW, e.age(now))
	return strings.Join([]string{
		colorize(state, style.color, color),
		colorize(dir, "34", color),
		colorize(cmd, "1", color),
		summary,
		colorize(age, "2", color),
	}, " ")
}

func pickerLines(list []sessionEntry, width int) string {
	now := time.Now()
	var b strings.Builder
	for _, e := range list {
		b.WriteString(e.Sock)
		b.WriteByte('\t')
		b.WriteString(e.columns(width, true, now))
		b.WriteByte('\n')
	}
	return b.String()
}

func printPickerList(args []string) error {
	current := ""
	if len(args) >= 2 && args[0] == "--current" {
		current = args[1]
	}
	list, err := collectSessions(current)
	if err != nil {
		return err
	}
	width, _ := pickerSize()
	fmt.Print(pickerLines(list, width))
	return nil
}

// pickerSize is the terminal size fzf runs in; reloads run without a tty on
// stdin, so fall back to the controlling terminal and fzf's own variables.
func pickerSize() (int, int) {
	if w, h := terminalSize(); w > 0 {
		return w, h
	}
	if tty, err := os.Open("/dev/tty"); err == nil {
		defer tty.Close()
		if w, h, err := term.GetSize(int(tty.Fd())); err == nil {
			return w, h
		}
	}
	return positiveEnvInt("FZF_COLUMNS", 120), positiveEnvInt("FZF_LINES", 40)
}

func listSessions() error {
	list, err := collectSessions("")
	if err != nil {
		return err
	}
	tty := term.IsTerminal(1)
	if len(list) == 0 {
		if tty {
			fmt.Println("no sessions")
		}
		return nil
	}
	width := 120
	if tty {
		width, _ = terminalSize()
	}
	nameW := 0
	for _, e := range list {
		nameW = max(nameW, runeDisplayWidth(sessionName(e.Sock)))
	}
	// Names go last and are what --attach/--detach/--kill accept.
	cols := max(width-nameW-2, 80)
	now := time.Now()
	for _, e := range list {
		fmt.Printf("%s  %s\n", e.columns(cols, tty, now), colorize(sessionName(e.Sock), "2", tty))
	}
	return nil
}

func pickAndAttach(query string) error {
	outer := insideSession()
	selected, err := pickSession(outer, query)
	if err != nil || selected == "" {
		return err
	}
	if outer != "" && requestSwitch(outer, selected) {
		return nil
	}
	return attachSession(selected)
}

// pickSession runs fzf over the sessions. currentSock is set when switching
// from inside an attached session.
func pickSession(currentSock, query string) (string, error) {
	if _, err := exec.LookPath("fzf"); err != nil {
		return "", errors.New("di: fzf is not installed (d --list works without it)")
	}
	list, err := collectSessions(currentSock)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", errors.New("di: no sessions; start one with: d <command>")
	}
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	width, height := terminalSize()
	cmd := exec.Command("fzf", fzfArgs(exe, currentSock, query, height)...)
	cmd.Stdin = strings.NewReader(pickerLines(list, width))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && (exitErr.ExitCode() == 130 || exitErr.ExitCode() == 1) {
			return "", nil
		}
		return "", err
	}
	selected, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	return selected, nil
}

func fzfArgs(exe, currentSock, query string, height int) []string {
	self := shellQuote(exe)
	reload := self + " --picker-list"
	header := "Enter 进入 · Ctrl-X 结束会话 · Ctrl-R 刷新 · Esc 退出"
	if currentSock != "" {
		reload += " --current " + shellQuote(currentSock)
		header = "Enter 切换 · Ctrl-\\ 下一个 · Ctrl-X 结束会话 · Ctrl-R 刷新 · Esc 返回"
	}
	if height <= 0 {
		height = 40
	}
	listRows := clampInt(height*2/5, 6, 14)
	previewRows := max(height-listRows-3, 5)
	args := []string{
		"--ansi",
		"--no-sort",
		"--layout=reverse",
		"--height=100%",
		"--delimiter=\t",
		"--with-nth=2..",
		"--no-hscroll",
		"--info=inline-right",
		"--prompt=di> ",
		"--header=" + header,
		"--preview=" + self + " --preview {1}",
		fmt.Sprintf("--preview-window=down,%d,border-top", previewRows),
		"--bind=ctrl-\\:down",
		"--bind=ctrl-r:reload(" + reload + ")",
		"--bind=ctrl-x:execute(" + self + " --kill-confirm {1})+reload(" + reload + ")",
	}
	if query != "" {
		args = append(args, "--query="+query)
	}
	if currentSock == "" {
		args = append(args, "--select-1")
	}
	return args
}

func previewSession(sock string) error {
	cols := positiveEnvInt("FZF_PREVIEW_COLUMNS", 120)
	rows := positiveEnvInt("FZF_PREVIEW_LINES", 40)
	// One line for the header, and one spare: fzf shows a scroll indicator over
	// the header as soon as the content fills the window.
	st, err := querySession(sock, max(rows-2, 1), cols)
	if err != nil {
		fmt.Println("\x1b[2m(session has ended)\x1b[0m")
		return nil
	}
	e := sessionEntry{Sock: sock, Meta: readSessionMeta(sock), Status: st}
	style := styleForState(st, false)
	info := []string{
		formatDisplayCommand(e.Meta.Command, cleanSessionName(e.Meta.Name, sock)),
		formatDisplayPWD(e.Meta.PWD),
	}
	if age := e.age(time.Now()); age != "-" {
		info = append(info, age+" ago")
	}
	if st.Attached > 0 {
		info = append(info, fmt.Sprintf("%d attached", st.Attached))
	}
	if st.Title != "" {
		info = append(info, st.Title)
	}
	header := fitField(displayText(strings.Join(info, " · ")), max(cols-runeDisplayWidth(style.word)-3, 10))
	fmt.Printf("%s \x1b[2m%s\x1b[0m\n", colorize(style.icon+" "+style.word, style.color, true), header)
	fmt.Print(st.Screen)
	return nil
}

// killSession ends a running session or dismisses an exited one.
func killSession(sock string) error {
	st, err := querySession(sock, 0, 0)
	if err != nil {
		removeSessionFiles(sock)
		return nil
	}
	if st.Legacy {
		return killLegacySession(sock)
	}
	conn, err := dialSession(sock)
	if err != nil {
		return nil
	}
	_ = writeFrame(conn, frameKill, nil)
	_ = conn.Close()
	// The server hangs up the program, then escalates to SIGTERM and SIGKILL.
	if waitGone(sock, 10*time.Second) {
		return nil
	}
	return fmt.Errorf("d: %s is still running", sessionName(sock))
}

// killLegacySession stops a session run by an older di build, which has no
// kill request: hang up the program's process group, then stop the server.
func killLegacySession(sock string) error {
	server, children := legacyServerProcesses(sock)
	if server <= 0 {
		return fmt.Errorf("d: cannot find the server of %s", sessionName(sock))
	}
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM, syscall.SIGKILL} {
		for _, child := range children {
			_ = syscall.Kill(-child, sig)
		}
		if waitProcessesGone(children, 3*time.Second) {
			break
		}
	}
	// With its program gone the server exits by itself; make sure of it.
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if waitGone(sock, time.Second) {
			break
		}
		_ = syscall.Kill(server, sig)
	}
	if !waitGone(sock, 3*time.Second) || !waitProcessesGone(children, 0) {
		return fmt.Errorf("d: %s is still running", sessionName(sock))
	}
	removeSessionFiles(sock)
	return nil
}

func waitProcessesGone(pids []int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		alive := false
		for _, pid := range pids {
			if syscall.Kill(pid, 0) == nil {
				alive = true
			}
		}
		if !alive {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitGone(sock string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !sessionReachable(sock) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// legacyServerProcesses finds the server process for sock and its direct
// children (the session programs, each leading its own process group).
func legacyServerProcesses(sock string) (server int, children []int) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return 0, nil
	}
	type proc struct{ pid, ppid int }
	var procs []proc
	for line := range strings.SplitSeq(string(out), "\n") {
		var p proc
		if _, err := fmt.Sscanf(line, "%d %d", &p.pid, &p.ppid); err != nil {
			continue
		}
		procs = append(procs, p)
		if strings.Contains(line+" ", " --server "+sock+" ") {
			server = p.pid
		}
	}
	for _, p := range procs {
		if server > 0 && p.ppid == server {
			children = append(children, p.pid)
		}
	}
	return server, children
}

func confirmKillSession(sock string) error {
	st, err := querySession(sock, 0, 0)
	if err != nil {
		return nil
	}
	if st.State != "exited" {
		meta := readSessionMeta(sock)
		width, _ := pickerSize()
		label := fitField(formatDisplayCommand(meta.Command, cleanSessionName(meta.Name, sock)), max(width/2, 20))
		fmt.Printf("\n  结束会话 \x1b[1m%s\x1b[0m (%s)？ [y/N] ", label, fitPathLeft(formatDisplayPWD(meta.PWD), max(width/4, 12)))
		if !readYes() {
			return nil
		}
	}
	return killSession(sock)
}

func readYes() bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	defer tty.Close()
	if state, err := term.MakeRaw(int(tty.Fd())); err == nil {
		defer term.Restore(int(tty.Fd()), state)
	}
	var b [1]byte
	if n, _ := tty.Read(b[:]); n == 1 {
		return b[0] == 'y' || b[0] == 'Y'
	}
	return false
}

func formatDisplayPWD(pwd string) string {
	if pwd == "" {
		return "-"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if pwd == home {
			return "~"
		}
		if rest, ok := strings.CutPrefix(pwd, home+"/"); ok {
			return "~/" + rest
		}
	}
	return pwd
}

func formatDisplayCommand(command []string, fallback string) string {
	if len(command) == 0 {
		return fallback
	}
	args := []string{filepath.Base(command[0])}
	for _, arg := range command[1:] {
		if arg != "--dangerously-skip-permissions" {
			args = append(args, arg)
		}
	}
	return strings.Join(args, " ")
}

func cleanSessionName(name, sock string) string {
	if name == "" {
		name = sessionName(sock)
	}
	name = sessionSuffixRe.ReplaceAllString(name, "")
	return strings.ReplaceAll(name, "---dangerously-skip-permissions", "")
}

// Session names end with -<base36 start time>-<pid>[-n].
var sessionSuffixRe = regexp.MustCompile(`-[0-9a-z]{10,13}-[0-9]+(-[0-9]+)?$`)

// fitField truncates on the right with an ellipsis.
func fitField(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if runeDisplayWidth(value) <= width {
		return value
	}
	var out strings.Builder
	w := 0
	for _, r := range value {
		rw := singleRuneWidth(r)
		if w+rw > width-1 {
			break
		}
		out.WriteRune(r)
		w += rw
	}
	return out.String() + "…"
}

// fitFieldLeft truncates on the left, keeping the end of a path visible.
func fitFieldLeft(value string, width int) string {
	if width <= 0 || runeDisplayWidth(value) <= width {
		return value
	}
	runes := []rune(value)
	w := 0
	i := len(runes)
	for i > 0 {
		rw := singleRuneWidth(runes[i-1])
		if w+rw > width-1 {
			break
		}
		w += rw
		i--
	}
	return "…" + string(runes[i:])
}

// fitPathLeft keeps the last path components, cutting at a separator when that
// still leaves a useful amount of the path.
func fitPathLeft(path string, width int) string {
	cut := fitFieldLeft(path, width)
	if cut == path {
		return cut
	}
	rest := strings.TrimPrefix(cut, "…")
	if i := strings.IndexByte(rest, '/'); i > 0 && runeDisplayWidth(rest[i:]) >= width/2 {
		return "…" + rest[i:]
	}
	return cut
}

func padDisplayWidth(value string, width int) string {
	if w := runeDisplayWidth(value); w < width {
		return value + strings.Repeat(" ", width-w)
	}
	return value
}
