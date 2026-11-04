package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"golang.org/x/term"
)

const (
	defaultDetachKey = "^]"
	defaultSwitchKey = `^\`
	frameInput       = 'i'
	frameResize      = 'w'
	frameDetachAll   = 'D'
	dialTimeout      = 200 * time.Millisecond
	serverStartWait  = 2 * time.Second
	// Keep fast command output available until the launching client can attach.
	initialAttachWait = serverStartWait + time.Second
	// Linux allows 107 pathname bytes; 103 also fits macOS sockaddr_un.
	maxSocketPathLen = 103
)

type sessionMeta struct {
	Name      string   `json:"name"`
	Command   []string `json:"command"`
	PWD       string   `json:"pwd"`
	StartedAt string   `json:"started_at"`
}

func main() {
	var err error
	if len(os.Args) > 1 && isHelpArg(os.Args[1]) {
		fmt.Print(helpText())
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "--preview" {
		err = previewSession(os.Args[2:])
	} else if len(os.Args) > 1 && os.Args[1] == "--server" {
		err = runServer(os.Args[2:])
	} else if len(os.Args) > 2 && os.Args[1] == "--attach" {
		err = attachSession(os.Args[2], true)
	} else if len(os.Args) > 1 && os.Args[1] == "install" {
		err = installSelf()
	} else if filepath.Base(os.Args[0]) == "di" {
		err = pickAndAttach()
	} else {
		err = runD(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runD(args []string) error {
	if len(args) == 0 {
		return errors.New(usage())
	}
	if args[0] == "install" {
		return installSelf()
	}
	if isHelpArg(args[0]) {
		fmt.Print(helpText())
		return nil
	}

	dir, err := sessionDir()
	if err != nil {
		return err
	}
	switch args[0] {
	case "--list":
		return listSessions()
	case "--detach":
		if len(args) < 2 {
			return errors.New("usage: d --detach <name>")
		}
		return detachSession(filepath.Join(dir, args[1]+".sock"))
	}
	commandArgs := expandCommandAlias(args)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sock, err := uniqueSocketPath(dir, labelFor(args))
	if err != nil {
		return err
	}
	if err := writeSessionMeta(sock, args); err != nil {
		return err
	}
	if err := startServer(sock, commandArgs); err != nil {
		_ = os.Remove(metaPath(sock))
		return err
	}
	if err := waitSocket(sock, serverStartWait); err != nil {
		_ = os.Remove(metaPath(sock))
		return err
	}
	return attach(sock)
}

func expandCommandAlias(args []string) []string {
	shell := os.Getenv("SHELL")
	return expandAliasArgs(args, queryShellAliases(shell), shell)
}

func expandAliasArgs(args []string, aliases map[string]string, shell string) []string {
	if len(args) == 0 || shell == "" {
		return args
	}
	value, ok := aliases[args[0]]
	if !ok || value == "" {
		return args
	}
	// Let the shell interpret assignments, quoting, and other aliases. Remove
	// the requested alias first so its expansion is not applied a second time.
	// Positional parameters keep user arguments literal, even with shell syntax.
	script := "unalias -- " + shellQuote(args[0]) + "\n" + value + ` "$@"`
	return append([]string{shell, "-ic", script, "d"}, args[1:]...)
}

func queryShellAliases(shell string) map[string]string {
	switch filepath.Base(shell) {
	case "bash", "zsh":
	default:
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := shellAliasCmd(ctx, shell).Output()
	if err != nil {
		return nil
	}
	return parseAliasOutput(out)
}

func shellAliasCmd(ctx context.Context, shell string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, shell, "-ic", "alias")
	cmd.Stdin = strings.NewReader("")
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

func parseAliasOutput(out []byte) map[string]string {
	aliases := map[string]string{}
	for rawLine := range strings.SplitSeq(string(out), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		line = strings.TrimPrefix(line, "alias ") // bash
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:eq])
		if name == "" || strings.ContainsAny(name, " \t") {
			continue
		}
		aliases[name] = unquoteAliasValue(strings.TrimSpace(line[eq+1:]))
	}
	return aliases
}

func unquoteAliasValue(value string) string {
	if value == "" {
		return ""
	}
	switch value[0] {
	case '\'':
		if len(value) < 2 || value[len(value)-1] != '\'' {
			return value
		}
		return strings.ReplaceAll(value[1:len(value)-1], `'\''`, `'`)
	case '"':
		if len(value) < 2 || value[len(value)-1] != '"' {
			return value
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
		return value[1 : len(value)-1]
	default:
		return value
	}
}

func usage() string {
	return "usage: d <command> [args...]\n       d install\n       d --list\n       d --detach <name>"
}

func isHelpArg(arg string) bool {
	return arg == "--help" || arg == "-h"
}

func helpText() string {
	return `di - detachable terminal sessions

Usage:
  d <command> [args...]       start a new detachable session and attach to it
  di                          pick an existing session with fzf and attach to it
  d --list                    list active sessions
  d --detach <name>           detach all clients from a session
  d install                   install the current binary to ~/.local/bin/d and link di
  d --help, di --help         show this help

Keys:
  Ctrl-]                      detach from the current session
  Ctrl-\                      switch to another session with fzf

Environment:
  D_DETACH=^B                 override the detach key
  D_SWITCH=^\                 override the session switch key

Notes:
  d reads bash/zsh aliases from $SHELL startup files for each new session.
  Alias commands run in that shell; additional arguments stay literal.
  Aliases defined only in the current terminal must be saved in startup files.
  di requires fzf to pick sessions. The picker renders the latest terminal screen
  for previews, including full-screen agent applications.
  Starting and listing sessions do not require fzf.
`
}

func installSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}

	src, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer src.Close()

	srcInfo, err := src.Stat()
	if err != nil {
		return err
	}
	if srcInfo.IsDir() {
		return fmt.Errorf("d install: executable is a directory: %s", exe)
	}

	tmp, err := os.CreateTemp(binDir, ".d-install-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, srcInfo.Mode().Perm()|0o755); err != nil {
		return err
	}

	dPath := filepath.Join(binDir, "d")
	diPath := filepath.Join(binDir, "di")
	if err := os.Rename(tmpPath, dPath); err != nil {
		return err
	}
	_ = os.Remove(diPath)
	if err := os.Symlink(dPath, diPath); err != nil {
		return err
	}
	fmt.Printf("installed %s\n", dPath)
	fmt.Printf("linked %s -> %s\n", diPath, dPath)
	return nil
}

func pickAndAttach() error {
	selected, err := pickSession("")
	if err != nil || selected == "" {
		return err
	}
	return attachSession(selected, true)
}

func pickSession(currentSock string) (string, error) {
	if _, err := exec.LookPath("fzf"); err != nil {
		return "", errors.New("di: fzf is not installed")
	}
	sessions, err := allSessions()
	if err != nil {
		return "", err
	}
	if len(sessions) == 0 {
		return "", errors.New("di: no sessions found")
	}
	sessions = orderAndEnrichSessions(sessions, currentSock)
	width, height := terminalSize()
	lines := make([]string, 0, len(sessions))
	for _, session := range sessions {
		lines = append(lines, session.displayLine(width))
	}
	cmdArgs := fzfArgs(os.Args[0], height)
	cmd := exec.Command("fzf", cmdArgs...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	go copyLines(stdin, lines)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 130 {
			return "", nil
		}
		return "", err
	}
	selected := strings.TrimSpace(string(out))
	if selected == "" {
		return "", nil
	}
	fields := strings.Split(selected, "\t")
	if len(fields) == 0 || fields[0] == "" {
		return "", nil
	}
	return fields[0], nil
}

func fzfArgs(executable string, height int) []string {
	preview := fmt.Sprintf("%s --preview {1}", shellQuote(executable))
	if height <= 0 {
		height = 24
	}
	listRows := clampInt(height*2/5, 8, 16)
	previewHeight := max(height-listRows-2, 6)
	return []string{
		"--prompt=di> ",
		"--header=Enter: 切换  Ctrl-\\: 下一个  Esc: 返回",
		"--bind=ctrl-\\:down",
		"--height=100%",
		"--reverse",
		"--no-hscroll",
		"--delimiter=\t",
		"--with-nth=2..",
		"--preview=" + preview,
		fmt.Sprintf("--preview-window=down,%d,wrap,border-top", previewHeight),
	}
}

func terminalSize() (int, int) {
	width, height, err := term.GetSize(0)
	if err != nil {
		return 0, 0
	}
	return width, height
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func previewSession(args []string) error {
	if len(args) != 1 || args[0] == "" {
		return nil
	}

	output, err := sessionPreviewOutput(args[0])
	if err != nil {
		return nil
	}
	cols, rows := previewSize()
	_, err = os.Stdout.Write(renderPreview(output, cols, rows))
	return err
}

func previewSize() (int, int) {
	return positiveEnvInt("FZF_PREVIEW_COLUMNS", 120), positiveEnvInt("FZF_PREVIEW_LINES", 40)
}

func positiveEnvInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func renderPreview(output []byte, cols, rows int) []byte {
	if len(output) == 0 {
		return nil
	}
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	const maxPreviewBytes = 64 << 10
	if len(output) > maxPreviewBytes {
		output = trimHistorySlice(output[len(output)-maxPreviewBytes:])
	}
	output = normalizeInkClear(output)
	// Replay into a virtual terminal using the session's native rendered width
	// and a tall row count so relative cursor-up redraws (\r\x1b[<N>A) do not
	// wrap out of sync or scroll conversation text off the top of the screen.
	vtCols := detectNativeCols(output, cols)
	vtRows := max(rows*4, 120)
	terminal := newVTerm(vtRows, vtCols)
	terminal.Write(output)

	rawLines := terminal.ScreenPlainLines()
	cleaned := cleanRenderedLines(rawLines, cols)
	if len(cleaned) == 0 {
		return nil
	}
	if len(cleaned) > rows {
		cleaned = cleaned[len(cleaned)-rows:]
	}
	return []byte(strings.Join(cleaned, "\n") + "\n")
}

// normalizeInkClear collapses Ink full-screen clear sequences (\x1b[H\x1b[2K\x1b[1B\x1b[2K)
// into a standard \x1b[2J\x1b[H clear.
func normalizeInkClear(output []byte) []byte {
	inkClear := []byte("\x1b[H\x1b[2K\x1b[1B\x1b[2K")
	if idx := bytes.LastIndex(output, inkClear); idx >= 0 {
		rest := output[idx:]
		j := 0
		for j < len(rest) {
			if bytes.HasPrefix(rest[j:], []byte("\x1b[H")) {
				j += 3
			} else if bytes.HasPrefix(rest[j:], []byte("\x1b[2K")) || bytes.HasPrefix(rest[j:], []byte("\x1b[1B")) {
				j += 4
			} else {
				break
			}
		}
		output = append([]byte(clearScreenHome), rest[j:]...)
	}
	return output
}

// detectNativeCols infers the PTY column width at which the latest screen in
// output was rendered by checking the most recent horizontal rule (──────).
func detectNativeCols(output []byte, fallback int) int {
	lines := bytes.Split(output, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimRight(lines[i], "\r ")
		w := countHorizontalRuleWidth(line)
		if w >= 30 && w <= 300 {
			return w
		}
	}
	return fallback
}

func countHorizontalRuleWidth(line []byte) int {
	// Strip simple CSI sequences before checking if the line is a horizontal rule.
	var plain []rune
	for i := 0; i < len(line); {
		if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '[' {
			i += 2
			for i < len(line) && (line[i] < 0x40 || line[i] > 0x7e) {
				i++
			}
			if i < len(line) {
				i++
			}
			continue
		}
		r, size := utf8.DecodeRune(line[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		plain = append(plain, r)
		i += size
	}
	if len(plain) < 30 {
		return 0
	}
	ruleChars := 0
	for _, r := range plain {
		if r == '─' || r == '━' || r == '═' {
			ruleChars++
		}
	}
	if ruleChars*10 >= len(plain)*9 {
		return len(plain)
	}
	return 0
}

func cleanRenderedLines(rawLines []string, targetCols int) []string {
	var collapsed []string
	blankRun := 0
	for _, line := range rawLines {
		trimmed := strings.TrimRight(line, " ")
		if trimmed == "" {
			blankRun++
			if len(collapsed) > 0 && blankRun <= 1 {
				collapsed = append(collapsed, "")
			}
			continue
		}
		blankRun = 0
		if isPureHorizontalRule(trimmed) {
			ruleLen := min(targetCols, runeDisplayWidth(trimmed))
			if ruleLen < 8 {
				ruleLen = 8
			}
			collapsed = append(collapsed, strings.Repeat("─", ruleLen))
			continue
		}
		collapsed = append(collapsed, wrapDisplayLine(trimmed, targetCols)...)
	}
	for len(collapsed) > 0 && collapsed[len(collapsed)-1] == "" {
		collapsed = collapsed[:len(collapsed)-1]
	}
	return collapsed
}

func isPureHorizontalRule(s string) bool {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) < 20 {
		return false
	}
	for _, r := range runes {
		if r != '─' && r != '━' && r != '═' && r != '-' {
			return false
		}
	}
	return true
}

func wrapDisplayLine(s string, maxCols int) []string {
	if maxCols <= 0 || runeDisplayWidth(s) <= maxCols {
		return []string{s}
	}
	var out []string
	var cur strings.Builder
	curW := 0
	for _, r := range s {
		rw := singleRuneWidth(r)
		if curW+rw > maxCols && curW > 0 {
			out = append(out, strings.TrimRight(cur.String(), " "))
			cur.Reset()
			curW = 0
		}
		cur.WriteRune(r)
		curW += rw
	}
	if cur.Len() > 0 {
		out = append(out, strings.TrimRight(cur.String(), " "))
	}
	return out
}

func sessionPreviewOutput(sock string) ([]byte, error) {
	conn, err := dialSession(sock)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// The server replays history immediately upon accept. Once the first chunk
	// arrives, use a short 5ms drain deadline so fzf previews and session list
	// summaries return almost instantaneously.
	if err := conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		return nil, err
	}
	var output []byte
	buf := make([]byte, 32<<10)
	shortenedDeadline := false
	for {
		n, readErr := conn.Read(buf)
		if n > 0 {
			output = append(output, buf[:n]...)
			if !shortenedDeadline {
				shortenedDeadline = true
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
			}
		}
		if readErr != nil {
			if errors.Is(readErr, os.ErrDeadlineExceeded) || errors.Is(readErr, syscall.EAGAIN) || errors.Is(readErr, io.EOF) {
				return output, nil
			}
			return output, readErr
		}
	}
}

func sessionDir() (string, error) {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "di"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "di"), nil
}

type sessionInfo struct {
	Sock    string
	Meta    sessionMeta
	Status  string
	Summary string
	Current bool
}

func orderAndEnrichSessions(sessions []sessionInfo, currentSock string) []sessionInfo {
	var wg sync.WaitGroup
	for i := range sessions {
		if currentSock != "" && sessions[i].Sock == currentSock {
			sessions[i].Current = true
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			output, err := sessionPreviewOutput(sessions[idx].Sock)
			if err != nil || len(output) == 0 {
				sessions[idx].Status = "○"
				return
			}
			status, summary := extractSessionSummary(output)
			sessions[idx].Status = status
			sessions[idx].Summary = summary
		}(i)
	}
	wg.Wait()

	// When invoked from inside an active session via Ctrl-\, place other sessions
	// first so pressing Enter or Ctrl-\ immediately switches to another session.
	if currentSock != "" && len(sessions) > 1 {
		others := make([]sessionInfo, 0, len(sessions))
		var current []sessionInfo
		for _, s := range sessions {
			if s.Current {
				current = append(current, s)
			} else {
				others = append(others, s)
			}
		}
		sessions = append(others, current...)
	}
	return sessions
}

func extractSessionSummary(output []byte) (status string, summary string) {
	const maxInspectBytes = 64 << 10
	if len(output) > maxInspectBytes {
		output = trimHistorySlice(output[len(output)-maxInspectBytes:])
	}
	status = "○"
	if _, state := extractLastOSC7501(output); state == "working" {
		status = "●"
	}

	output = normalizeInkClear(output)
	vtCols := detectNativeCols(output, 100)
	terminal := newVTerm(120, vtCols)
	terminal.Write(output)
	rawLines := terminal.DetectionLines(120)
	var activeTool string
	var substantive []string
	for i := len(rawLines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(rawLines[i])
		if line == "" || isPureHorizontalRule(line) {
			continue
		}
		if spinnerText, ok := parseSpinnerLine(line); ok {
			status = "●"
			if activeTool == "" && spinnerText != "" {
				activeTool = spinnerText
			}
			continue
		}
		if isUIChromeLine(line) {
			continue
		}
		cleaned := cleanSummaryText(line)
		if cleaned == "" {
			continue
		}
		substantive = append([]string{cleaned}, substantive...)
		if len(substantive) >= 2 || runeDisplayWidth(strings.Join(substantive, " ")) >= 56 {
			break
		}
	}
	summary = strings.Join(substantive, " ")
	if activeTool != "" {
		if summary != "" {
			summary = "[" + activeTool + "] " + summary
		} else {
			summary = "[" + activeTool + "]"
		}
	}
	return status, summary
}

func parseSpinnerLine(line string) (string, bool) {
	r, size := utf8.DecodeRuneInString(line)
	if r >= 0x2800 && r <= 0x28ff {
		return strings.TrimSpace(line[size:]), true
	}
	return "", false
}

func isUIChromeLine(line string) bool {
	if line == ">" || line == "❯" || line == "$" || line == "#" || line == "↳" {
		return true
	}
	if strings.HasPrefix(line, "? for shortcuts") ||
		strings.HasPrefix(line, "esc to cancel") ||
		strings.HasPrefix(line, "Argon ·") ||
		strings.HasPrefix(line, "8 Flash ·") ||
		strings.HasPrefix(line, "Flash ·") ||
		strings.HasPrefix(line, "Pro ·") ||
		strings.HasPrefix(line, "Gemini ") ||
		strings.HasPrefix(line, "GPT-") ||
		strings.HasPrefix(line, "Claude ") ||
		strings.HasPrefix(line, "└ Tip:") ||
		strings.HasPrefix(line, "⚡ ") ||
		strings.HasPrefix(line, "✻ ") ||
		strings.HasPrefix(line, "⏵⏵") ||
		strings.HasPrefix(line, "cache in:") ||
		strings.HasPrefix(line, "Reloaded keybindings") ||
		strings.Contains(line, "for shortcuts") ||
		strings.Contains(line, "shift+tab to") ||
		strings.Contains(line, "esc to interrupt") ||
		strings.Contains(line, "Esc to cancel") ||
		strings.Contains(line, "ctrl+o to expand") ||
		strings.Contains(line, "ctrl+c to") ||
		strings.Contains(line, "MCP:") ||
		strings.Contains(line, "think:high") ||
		strings.Contains(line, "think:medium") {
		return true
	}
	// Box drawing borders without text content.
	onlyBorder := true
	for _, r := range line {
		if r != '─' && r != '━' && r != '│' && r != '╭' && r != '╮' && r != '╯' && r != '╰' && r != ' ' && r != '>' && r != '❯' {
			onlyBorder = false
			break
		}
	}
	return onlyBorder
}

func cleanSummaryText(line string) string {
	line = strings.TrimLeft(line, "•*-│╭╰> ")
	line = strings.TrimRight(line, "│╮╯` ")
	return strings.Join(strings.Fields(line), " ")
}

func (s sessionInfo) displayLine(width int) string {
	name := cleanSessionName(s.Meta.Name, s.Sock)
	pwd := formatDisplayPWD(s.Meta.PWD, s.Status, s.Current)
	cmd := formatDisplayCommand(s.Meta.Command, name)
	summary := s.Summary
	if summary == "" {
		summary = name
	}

	if width <= 0 {
		width = 120
	}
	available := width - 8
	if available < 48 {
		available = 48
	}
	pwdWidth := clampInt(available*24/100, 16, 26)
	cmdWidth := clampInt(available*14/100, 10, 18)
	summaryWidth := available - pwdWidth - cmdWidth - 2
	if summaryWidth < 20 {
		summaryWidth = 20
	}
	return fmt.Sprintf(
		"%s\t%s\t%s\t%s",
		s.Sock,
		padDisplayWidth(fitField(pwd, pwdWidth), pwdWidth),
		padDisplayWidth(fitField(cmd, cmdWidth), cmdWidth),
		padDisplayWidth(fitField(summary, summaryWidth), summaryWidth),
	)
}

func formatDisplayPWD(pwd, status string, current bool) string {
	if pwd == "" {
		pwd = "-"
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		if pwd == home {
			pwd = "~"
		} else if strings.HasPrefix(pwd, home+"/") {
			pwd = "~/" + strings.TrimPrefix(pwd, home+"/")
		}
	}
	prefix := ""
	if current {
		prefix = "▶ "
	} else if status != "" {
		prefix = status + " "
	}
	return prefix + pwd
}

func formatDisplayCommand(command []string, fallback string) string {
	if len(command) == 0 {
		return fallback
	}
	prog := command[0]
	if strings.Contains(prog, "/") {
		prog = filepath.Base(prog)
	}
	args := make([]string, 0, len(command))
	args = append(args, prog)
	for _, arg := range command[1:] {
		if arg == "--dangerously-skip-permissions" {
			continue
		}
		args = append(args, arg)
	}
	res := strings.Join(args, " ")
	if res == "" {
		return fallback
	}
	return res
}

var sessionSuffixRe = regexp.MustCompile(`-dm[a-z0-9]{8,}-[0-9]+$`)

func cleanSessionName(name, sock string) string {
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(sock), ".sock")
	}
	name = sessionSuffixRe.ReplaceAllString(name, "")
	name = strings.ReplaceAll(name, "---dangerously-skip-permissions", "")
	return name
}

func clampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func fitField(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if runeDisplayWidth(value) <= width {
		return value
	}
	if width <= 3 {
		var out strings.Builder
		w := 0
		for _, r := range value {
			rw := singleRuneWidth(r)
			if w+rw > width {
				break
			}
			out.WriteRune(r)
			w += rw
		}
		return out.String()
	}
	target := width - 3
	var out strings.Builder
	w := 0
	for _, r := range value {
		rw := singleRuneWidth(r)
		if w+rw > target {
			break
		}
		out.WriteRune(r)
		w += rw
	}
	return out.String() + "..."
}

func padDisplayWidth(value string, width int) string {
	w := runeDisplayWidth(value)
	if w >= width {
		return value
	}
	return value + strings.Repeat(" ", width-w)
}

func runeDisplayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += singleRuneWidth(r)
	}
	return w
}

func singleRuneWidth(r rune) int {
	if r >= 0x1100 &&
		(r <= 0x115f || r == 0x2329 || r == 0x232a ||
			(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
			(r >= 0xac00 && r <= 0xd7a3) ||
			(r >= 0xf900 && r <= 0xfaff) ||
			(r >= 0xfe10 && r <= 0xfe19) ||
			(r >= 0xfe30 && r <= 0xfe6f) ||
			(r >= 0xff00 && r <= 0xff60) ||
			(r >= 0xffe0 && r <= 0xffe6) ||
			(r >= 0x1f300 && r <= 0x1faff) ||
			(r >= 0x20000 && r <= 0x3fffd)) {
		return 2
	}
	return 1
}

func allSessions() ([]sessionInfo, error) {
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
	var sessions []sessionInfo
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if strings.HasSuffix(e.Name(), ".sock") && isSocket(path) {
			if !sessionReachable(path) {
				removeSessionFiles(path)
				continue
			}
			sessions = append(sessions, sessionInfo{Sock: path, Meta: readSessionMeta(path)})
		}
	}
	return sessions, nil
}

func listSessions() error {
	sessions, err := allSessions()
	if err != nil {
		return err
	}
	sessions = orderAndEnrichSessions(sessions, "")
	width, _ := terminalSize()
	if width <= 0 {
		width = 120
	}
	for _, session := range sessions {
		fields := strings.Split(session.displayLine(width), "\t")
		if len(fields) >= 4 {
			fmt.Printf("%s\t%s\t%s\n", fields[1], fields[2], strings.TrimRight(fields[3], " "))
		}
	}
	return nil
}

func labelFor(args []string) string {
	re := regexp.MustCompile(`[^A-Za-z0-9._+-]+`)
	label := re.ReplaceAllString(strings.Join(args, " "), "-")
	label = strings.Trim(label, "-")
	if label == "" {
		return "session"
	}
	return label
}

func uniqueSocketPath(dir, base string) (string, error) {
	if base == "" {
		base = "session"
	}
	for i := 0; i < 100; i++ {
		suffix := fmt.Sprintf("-%s-%d", strconv.FormatInt(time.Now().UnixNano(), 36), os.Getpid())
		if i > 0 {
			suffix += fmt.Sprintf("-%d", i)
		}
		candidateBase := base
		path := filepath.Join(dir, candidateBase+suffix+".sock")
		if excess := len(path) - maxSocketPathLen; excess > 0 {
			keep := len(candidateBase) - excess
			if keep < 1 {
				return "", fmt.Errorf("d: session directory is too long for a Unix socket: %s", dir)
			}
			candidateBase = strings.TrimRight(candidateBase[:keep], "-")
			if candidateBase == "" {
				candidateBase = "s"
			}
			path = filepath.Join(dir, candidateBase+suffix+".sock")
		}
		if len(path) > maxSocketPathLen {
			return "", fmt.Errorf("d: session directory is too long for a Unix socket: %s", dir)
		}
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return path, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("d: could not allocate a unique session socket")
}

func metaPath(sock string) string {
	return strings.TrimSuffix(sock, ".sock") + ".json"
}

func removeSessionFiles(sock string) {
	_ = os.Remove(sock)
	_ = os.Remove(metaPath(sock))
}

func writeSessionMeta(sock string, args []string) error {
	pwd, _ := os.Getwd()
	meta := sessionMeta{
		Name:      strings.TrimSuffix(filepath.Base(sock), ".sock"),
		Command:   append([]string(nil), args...),
		PWD:       pwd,
		StartedAt: time.Now().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath(sock), data, 0o600)
}

func readSessionMeta(sock string) sessionMeta {
	var meta sessionMeta
	data, err := os.ReadFile(metaPath(sock))
	if err != nil {
		meta.Name = strings.TrimSuffix(filepath.Base(sock), ".sock")
		return meta
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		meta.Name = strings.TrimSuffix(filepath.Base(sock), ".sock")
	}
	return meta
}

func isSocket(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSocket != 0
}

func dialSession(sock string) (net.Conn, error) {
	var dialer net.Dialer
	dialer.Timeout = dialTimeout
	return dialer.Dial("unix", sock)
}

func sessionReachable(sock string) bool {
	conn, err := dialSession(sock)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func startServer(sock string, args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()
	cmd := exec.Command(exe, append([]string{"--server", sock}, args...)...)
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	env := os.Environ()
	if cols, rows := terminalSize(); cols > 0 && rows > 0 {
		env = append(env,
			fmt.Sprintf("DI_INITIAL_COLS=%d", cols),
			fmt.Sprintf("DI_INITIAL_ROWS=%d", rows),
		)
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func waitSocket(sock string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isSocket(sock) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("d: session socket was not created: %s", sock)
}

func attach(sock string) error {
	return attachSession(sock, false)
}

func attachSession(sock string, reattach bool) error {
	currentSock := sock
	for {
		switchSession, err := attachOnce(currentSock, reattach)
		if err != nil || !switchSession {
			return err
		}
		selected, pickErr := pickSession(currentSock)
		if pickErr != nil {
			return pickErr
		}
		if selected != "" {
			currentSock = selected
			reattach = true
			maybeExecUpdatedBinary(currentSock)
			continue
		}
		if !sessionReachable(currentSock) {
			return nil
		}
		reattach = true
		maybeExecUpdatedBinary(currentSock)
	}
}

func maybeExecUpdatedBinary(sock string) {
	link, err := os.Readlink("/proc/self/exe")
	if err != nil || !strings.HasSuffix(link, " (deleted)") {
		return
	}
	diskPath := strings.TrimSuffix(link, " (deleted)")
	if info, statErr := os.Stat(diskPath); statErr == nil && !info.IsDir() {
		_ = syscall.Exec(diskPath, []string{os.Args[0], "--attach", sock}, os.Environ())
	}
}

func attachOnce(sock string, reattach bool) (bool, error) {
	conn, err := dialSession(sock)
	if err != nil {
		removeSessionFiles(sock)
		return false, err
	}
	defer conn.Close()

	oldTerm, err := makeRaw(0)
	if err != nil {
		return false, err
	}
	defer restoreTerm(0, oldTerm)

	if reattach {
		fmt.Print(mouseTrackingReset)
	}

	initCols, initRows := terminalSize()
	tracker := newRedrawTracker()
	renderer := newClientSessionRenderer(os.Stdout, initRows, initCols, tracker)
	defer renderer.Close()

	done := make(chan struct{})
	go func() {
		forwardSessionOutputWithRenderer(os.Stdout, conn, tracker, renderer)
		close(done)
	}()
	// Stop and drain forwarded output before restoring terminal state. Otherwise
	// an already-read agent status can arrive after the cleanup on detach.
	defer func() {
		_ = conn.Close()
		<-done
	}()

	stopWinch := make(chan struct{})
	defer close(stopWinch)
	if reattach {
		_ = sendWindowSizeSmartReattach(conn, tracker, renderer, stopWinch)
	} else {
		_ = sendWindowSize(conn)
	}
	go watchWindowSizeWithRenderer(conn, tracker, renderer, stopWinch)

	type inputResult struct {
		switchSession bool
		err           error
	}
	inputCh := make(chan inputResult, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if isDetachInput(chunk) {
					inputCh <- inputResult{}
					return
				}
				if isSwitchInput(chunk) {
					inputCh <- inputResult{switchSession: true}
					return
				}
				if renderer.HandleInput(chunk) {
					continue
				}
				tracker.resetCompactor()
				if err := writeFrame(conn, frameInput, chunk); err != nil {
					inputCh <- inputResult{err: err}
					return
				}
			}
			if err != nil {
				inputCh <- inputResult{err: err}
				return
			}
		}
	}()

	select {
	case <-done:
		return false, nil
	case res := <-inputCh:
		_ = conn.Close()
		<-done
		if res.err == nil {
			clearLocalScreen()
		}
		return res.switchSession, res.err
	}
}

func runServer(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: d --server <socket> <command> [args...]")
	}
	sock := args[0]
	cmdArgs := args[1:]

	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return err
	}
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer os.Remove(sock)
	defer os.Remove(metaPath(sock))
	defer ln.Close()

	master, slave, err := openPTY()
	if err != nil {
		return err
	}
	defer master.Close()
	defer slave.Close()

	initialCols := positiveEnvInt("DI_INITIAL_COLS", 0)
	initialRows := positiveEnvInt("DI_INITIAL_ROWS", 0)
	if initialCols > 0 && initialRows > 0 {
		_ = pty.Setsize(master, &pty.Winsize{
			Rows: uint16(initialRows),
			Cols: uint16(initialCols),
		})
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.Env = filterInternalEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = slave.Close()

	server := &ptyServer{
		master:      master,
		clients:     map[net.Conn]struct{}{},
		clientReady: make(chan struct{}),
		lastRows:    uint16(initialRows),
		lastCols:    uint16(initialCols),
	}
	ptyDone := make(chan struct{})
	go func() {
		server.broadcastPTY()
		close(ptyDone)
	}()
	go func() {
		_ = cmd.Wait()
	}()
	go func() {
		<-ptyDone
		select {
		case <-server.clientReady:
		case <-time.After(initialAttachWait):
		}
		server.shutdownClients()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			server.shutdownClients()
			server.clientWG.Wait()
			return nil
		}
		if !server.add(conn) {
			continue
		}
		go func() {
			defer server.clientWG.Done()
			server.handle(conn)
		}()
	}
}

type ptyServer struct {
	mu                sync.Mutex
	master            *os.File
	clients           map[net.Conn]struct{}
	vt                *vterm
	history           []byte
	historyTruncated  bool
	lastStatusSeq     []byte
	hasRedrawTail     bool
	lastRedrawOffset  int
	lastRedrawDown    int
	lastRows          uint16
	lastCols          uint16
	initialResizeDone bool
	resizeSeq         uint64
	readyOnce         sync.Once
	clientReady       chan struct{}
	clientWG          sync.WaitGroup
	closing           bool
}

func (s *ptyServer) ensureVTermLocked() *vterm {
	if s.vt == nil {
		rows, cols := int(s.lastRows), int(s.lastCols)
		if rows <= 0 {
			rows = defaultVTermRows
		}
		if cols <= 0 {
			cols = defaultVTermCols
		}
		s.vt = newVTerm(rows, cols)
	}
	return s.vt
}

func (s *ptyServer) markClientReady() {
	s.readyOnce.Do(func() {
		close(s.clientReady)
	})
}

func (s *ptyServer) add(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		_ = conn.Close()
		return false
	}
	s.clientWG.Add(1)
	s.clients[conn] = struct{}{}
	hist := s.history
	if s.vt != nil && s.vt.ScrollbackLen() > 0 {
		if s.historyTruncated {
			_, _ = conn.Write(s.vt.ScrollbackHeaderANSI())
			hist = bytes.TrimPrefix(hist, []byte(clearAllHome))
		} else {
			_, _ = conn.Write([]byte("\x1b]7502;vterm=1\x07"))
		}
	}
	if len(hist) > 0 {
		_, _ = conn.Write(hist)
	}
	if len(s.lastStatusSeq) > 0 {
		if _, state := extractLastOSC7501(s.history); state == "" {
			_, _ = conn.Write(s.lastStatusSeq)
		}
	}
	return true
}

func (s *ptyServer) remove(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, conn)
	_ = conn.Close()
}

func (s *ptyServer) closeClients() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeClientsLocked()
}

func (s *ptyServer) shutdownClients() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing = true
	s.closeClientsLocked()
}

func (s *ptyServer) closeClientsLocked() {
	for conn := range s.clients {
		_ = conn.Close()
		delete(s.clients, conn)
	}
}

const (
	decrqmSyncOutput = "\x1b[?2026$p"
	decrpmSyncOutput = "\x1b[?2026;2$y"
	syncUpdateBegin  = "\x1b[?2026h"
	syncUpdateEnd    = "\x1b[?2026l"
	clearScreenHome  = "\x1b[2J\x1b[H"
	clearAllHome     = "\x1b[2J\x1b[3J\x1b[H"
)

func (s *ptyServer) broadcastPTY() {
	buf := make([]byte, 64<<10)
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if bytes.Contains(chunk, []byte(decrqmSyncOutput)) {
				_, _ = s.master.Write([]byte(decrpmSyncOutput))
				chunk = bytes.ReplaceAll(chunk, []byte(decrqmSyncOutput), nil)
			}
			chunk = normalizeScrollbackClear(chunk)
			if len(chunk) > 0 {
				s.mu.Lock()
				s.appendHistory(chunk)
				for conn := range s.clients {
					if _, werr := conn.Write(chunk); werr != nil {
						_ = conn.Close()
						delete(s.clients, conn)
					}
				}
				s.mu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *ptyServer) appendHistory(p []byte) {
	const maxHistory = 512 << 10
	s.ensureVTermLocked().Write(p)
	if seq, state := extractLastOSC7501(p); state != "" {
		switch state {
		case "working", "blocked", "idle":
			s.lastStatusSeq = append(s.lastStatusSeq[:0], seq...)
		case "done":
			s.lastStatusSeq = append(s.lastStatusSeq[:0], "\x1b]7501;state=idle\x07"...)
		case "clear", "error":
			s.lastStatusSeq = nil
		}
	}
	p = normalizeScrollbackClear(p)
	if idx := lastFullClearIndex(p); idx >= 0 {
		s.history = nil
		s.historyTruncated = true
		s.hasRedrawTail = false
		s.lastRedrawOffset = 0
		s.lastRedrawDown = 0
		p = p[idx:]
	}
	if up, down, ok := parseInPlaceRedrawFrame(p); ok {
		if s.hasRedrawTail && up >= s.lastRedrawDown && s.lastRedrawOffset <= len(s.history) {
			s.history = append(s.history[:s.lastRedrawOffset], p...)
			s.lastRedrawDown = down
		} else {
			s.lastRedrawOffset = len(s.history)
			s.lastRedrawDown = down
			s.hasRedrawTail = true
			s.history = append(s.history, p...)
		}
	} else {
		s.hasRedrawTail = false
		s.history = append(s.history, p...)
	}
	if len(s.history) > maxHistory {
		trimmed := trimHistorySlice(s.history[len(s.history)-maxHistory:])
		s.history = append([]byte(nil), trimmed...)
		s.historyTruncated = true
		s.hasRedrawTail = false
	}
}

func (s *ptyServer) handle(conn net.Conn) {
	defer s.remove(conn)
	firstResize := true
	for {
		typ, payload, err := readFrame(conn)
		if err != nil {
			return
		}
		s.markClientReady()
		switch typ {
		case frameInput:
			_, _ = s.master.Write(payload)
		case frameResize:
			if len(payload) == 8 {
				rows := uint16(binary.BigEndian.Uint32(payload[:4]))
				cols := uint16(binary.BigEndian.Uint32(payload[4:]))
				isFirst := firstResize
				firstResize = false
				s.applyResize(rows, cols, isFirst)
			}
		case frameDetachAll:
			s.closeClients()
			return
		}
	}
}

func (s *ptyServer) applyResize(rows, cols uint16, firstForConn bool) {
	if rows == 0 || cols == 0 {
		return
	}
	s.mu.Lock()
	s.ensureVTermLocked().Resize(int(rows), int(cols))
	shouldBounce := firstForConn && s.initialResizeDone && s.lastRows == rows && s.lastCols == cols
	s.initialResizeDone = true
	s.resizeSeq++
	seq := s.resizeSeq
	if !shouldBounce {
		s.lastRows = rows
		s.lastCols = cols
		_ = pty.Setsize(s.master, &pty.Winsize{Rows: rows, Cols: cols})
		s.mu.Unlock()
		return
	}
	bounceCols := bounceColumnCount(cols)
	s.lastRows = rows
	s.lastCols = bounceCols
	_ = pty.Setsize(s.master, &pty.Winsize{Rows: rows, Cols: bounceCols})
	s.mu.Unlock()

	go func() {
		time.Sleep(150 * time.Millisecond)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closing || s.resizeSeq != seq {
			return
		}
		s.lastRows = rows
		s.lastCols = cols
		s.ensureVTermLocked().Resize(int(rows), int(cols))
		_ = pty.Setsize(s.master, &pty.Winsize{Rows: rows, Cols: cols})
	}()
}

func bounceColumnCount(cols uint16) uint16 {
	if cols > 1 {
		return cols - 1
	}
	return cols + 1
}

func filterInternalEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "DI_INITIAL_COLS=") || strings.HasPrefix(kv, "DI_INITIAL_ROWS=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func normalizeScrollbackClear(chunk []byte) []byte {
	if !bytes.Contains(chunk, []byte(clearScreenHome)) {
		return chunk
	}
	return bytes.ReplaceAll(chunk, []byte(clearScreenHome), []byte(clearAllHome))
}

func lastFullClearIndex(p []byte) int {
	idxAll := bytes.LastIndex(p, []byte(clearAllHome))
	idxScreen := bytes.LastIndex(p, []byte(clearScreenHome))
	return max(idxAll, idxScreen)
}

func extractLastOSC7501(b []byte) (seq []byte, state string) {
	prefix := []byte("\x1b]7501;")
	search := b
	offset := 0
	for {
		idx := bytes.Index(search, prefix)
		if idx < 0 {
			break
		}
		start := offset + idx
		rest := b[start+len(prefix):]
		end := -1
		termLen := 0
		for i := 0; i < len(rest) && i < 256; i++ {
			if rest[i] == 0x07 {
				end = i
				termLen = 1
				break
			}
			if rest[i] == 0x1b && i+1 < len(rest) && rest[i+1] == '\\' {
				end = i
				termLen = 2
				break
			}
		}
		if end >= 0 {
			body := string(rest[:end])
			for _, field := range strings.Split(body, ":") {
				if st, ok := strings.CutPrefix(field, "state="); ok && st != "" {
					seq = b[start : start+len(prefix)+end+termLen]
					state = st
					break
				}
			}
			offset = start + len(prefix) + end + termLen
			search = b[offset:]
		} else {
			offset = start + len(prefix)
			search = b[offset:]
		}
	}
	return seq, state
}

// parseInPlaceRedrawFrame detects cursor-up viewport redraw frames (such as
// Antigravity CLI's cursedRenderer frames starting with \r\x1b[<N>A) that
// rewrite the active bottom viewport in place without pushing new lines into
// scrollback (linefeeds <= N).
func parseInPlaceRedrawFrame(p []byte) (up int, down int, ok bool) {
	if len(p) < 6 || bytes.Contains(p, []byte("\x1b[2J")) || bytes.Contains(p, []byte("\x1b[3J")) {
		return 0, 0, false
	}
	search := p
	if len(search) > 64 {
		search = search[:64]
	}
	up = parseLeadingCursorUp(search)
	if up <= 0 {
		return 0, 0, false
	}
	if !bytes.Contains(p, []byte("\x1b[K")) && !bytes.Contains(p, []byte("\x1b[J")) {
		return 0, 0, false
	}
	down = bytes.Count(p, []byte("\n"))
	if down > up {
		return 0, 0, false
	}
	return up, down, true
}

func parseLeadingCursorUp(prefix []byte) int {
	_, _, n, ok := parseLeadingCursorUpBounds(prefix)
	if !ok {
		return 0
	}
	return n
}

func parseLeadingCursorUpBounds(prefix []byte) (prefixEnd int, afterSeq int, up int, ok bool) {
	for i := 0; i+3 < len(prefix); i++ {
		if prefix[i] != '\r' || prefix[i+1] != 0x1b || prefix[i+2] != '[' {
			continue
		}
		j := i + 3
		n := 0
		for j < len(prefix) && prefix[j] >= '0' && prefix[j] <= '9' {
			n = n*10 + int(prefix[j]-'0')
			j++
		}
		if n > 0 && j < len(prefix) && prefix[j] == 'A' {
			return i, j + 1, n, true
		}
	}
	return 0, 0, 0, false
}

// inPlaceCompactor diffs consecutive equal-height cursor-up redraw frames
// (\r\x1b[<N>A with N linefeeds) and strips unchanged top lines so 10 Hz
// spinner updates only rewrite the bottom lines that actually changed.
type inPlaceCompactor struct {
	mu        sync.Mutex
	prevUp    int
	prevLines [][]byte
}

func (c *inPlaceCompactor) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.prevUp = 0
	c.prevLines = nil
	c.mu.Unlock()
}

func (c *inPlaceCompactor) saveLocked(up int, lines [][]byte) {
	c.prevUp = up
	if cap(c.prevLines) < len(lines) {
		c.prevLines = make([][]byte, len(lines))
	} else {
		c.prevLines = c.prevLines[:len(lines)]
	}
	for i, line := range lines {
		c.prevLines[i] = append(c.prevLines[i][:0], line...)
	}
}

func hasVerticalCursorMotion(b []byte) bool {
	for i := 0; i+2 < len(b); i++ {
		if b[i] != 0x1b || b[i+1] != '[' {
			continue
		}
		j := i + 2
		for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
			j++
		}
		if j >= len(b) {
			break
		}
		switch b[j] {
		case 'A', 'B', 'E', 'F', 'H', 'f', 'd', 'S', 'T', 'r':
			return true
		}
		i = j
	}
	return false
}

func (c *inPlaceCompactor) compact(b []byte) (out []byte, skip bool) {
	if c == nil {
		return b, false
	}
	up, down, ok := parseInPlaceRedrawFrame(b)
	if !ok || up <= 0 || down != up {
		c.reset()
		return b, false
	}
	search := b
	if len(search) > 64 {
		search = search[:64]
	}
	prefixEnd, afterSeq, parsedUp, ok := parseLeadingCursorUpBounds(search)
	if !ok || parsedUp != up || hasVerticalCursorMotion(b[afterSeq:]) {
		c.reset()
		return b, false
	}
	lines := bytes.Split(b[afterSeq:], []byte("\n"))
	if len(lines) != up+1 {
		c.reset()
		return b, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.prevUp != up || len(c.prevLines) != len(lines) {
		c.saveLocked(up, lines)
		return b, false
	}
	firstDiff := len(lines)
	for i := range lines {
		if !bytes.Equal(lines[i], c.prevLines[i]) {
			firstDiff = i
			break
		}
	}
	c.saveLocked(up, lines)
	if firstDiff == len(lines) {
		if !bytes.Contains(b[:prefixEnd], []byte("\x1b]")) {
			return nil, true
		}
		firstDiff = len(lines) - 1
	}
	if firstDiff == 0 {
		return b, false
	}
	remUp := up - firstDiff
	var buf bytes.Buffer
	buf.Grow(prefixEnd + 20 + len(b[afterSeq:])/2)
	buf.Write(b[:prefixEnd])
	buf.WriteString("\x1b[m\r")
	if remUp > 0 {
		fmt.Fprintf(&buf, "\x1b[%dA", remUp)
	}
	// Preserve \x1b[J (clear from first changed row to bottom of screen) so
	// shorter status/spinner lines never leave trailing ghost characters and
	// shouldWrapSyncOutput always wraps the frame in DEC 2026 sync markers.
	buf.WriteString("\x1b[J")
	for i := firstDiff; i < len(lines); i++ {
		if i > firstDiff {
			buf.WriteByte('\n')
		}
		buf.Write(lines[i])
	}
	return buf.Bytes(), false
}

func trimHistorySlice(b []byte) []byte {
	// Skip leading UTF-8 continuation bytes so we never start mid-rune.
	i := 0
	for i < len(b) && i < 4 && (b[i]&0xc0) == 0x80 {
		i++
	}
	b = b[i:]
	// Advance to the start of the first complete line so a truncated history
	// buffer never begins inside an unfinished OSC-8 hyperlink or CSI sequence.
	limit := min(len(b), 1024)
	if nl := bytes.IndexByte(b[:limit], '\n'); nl >= 0 && nl+1 < len(b) {
		return b[nl+1:]
	}
	return b
}

type redrawTracker struct {
	mu            sync.Mutex
	notify        chan struct{}
	clearCount    int
	lastReadTime  time.Time
	lastClearTime time.Time
	redrawSettled bool
	bounceActive  bool
	compactor     inPlaceCompactor
}

func newRedrawTracker() *redrawTracker {
	return &redrawTracker{
		notify: make(chan struct{}, 1),
	}
}

func (t *redrawTracker) resetCompactor() {
	if t == nil {
		return
	}
	t.compactor.reset()
}

func (t *redrawTracker) observe(chunk []byte) {
	if t == nil || len(chunk) == 0 {
		return
	}
	now := time.Now()
	hasClear := bytes.Contains(chunk, []byte("\x1b[2J"))
	hasCursorShow := bytes.Contains(chunk, []byte("\x1b[?25h"))

	t.mu.Lock()
	t.lastReadTime = now
	if hasClear {
		t.clearCount++
		t.lastClearTime = now
		t.redrawSettled = hasCursorShow
	} else if t.clearCount > 0 && hasCursorShow {
		t.redrawSettled = true
	}
	t.mu.Unlock()

	select {
	case t.notify <- struct{}{}:
	default:
	}
}

func (t *redrawTracker) snapshot() (clears int, lastRead time.Time, settled bool) {
	if t == nil {
		return 0, time.Time{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.clearCount, t.lastReadTime, t.redrawSettled
}

func (t *redrawTracker) tryStartBounce() bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.bounceActive {
		return false
	}
	t.bounceActive = true
	return true
}

func (t *redrawTracker) finishBounce() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.bounceActive = false
	t.mu.Unlock()
}

func forwardSessionOutput(dst io.Writer, conn net.Conn) {
	forwardSessionOutputWithTracker(dst, conn, nil)
}

func forwardSessionOutputWithTracker(dst io.Writer, conn net.Conn, tracker *redrawTracker) {
	forwardSessionOutputWithRenderer(dst, conn, tracker, nil)
}

func forwardSessionOutputWithRenderer(dst io.Writer, conn net.Conn, tracker *redrawTracker, renderer *clientSessionRenderer) {
	const maxBatch = 64 << 10
	buf := make([]byte, 32<<10)
	batch := make([]byte, 0, maxBatch)
	var localCompactor inPlaceCompactor
	compactor := &localCompactor
	if tracker != nil {
		compactor = &tracker.compactor
	}
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			batch = append(batch[:0], buf[:n]...)
			for len(batch) < maxBatch && needsMoreFrameBytes(batch) {
				if derr := conn.SetReadDeadline(time.Now().Add(2 * time.Millisecond)); derr != nil {
					break
				}
				m, rerr := conn.Read(buf[:min(len(buf), maxBatch-len(batch))])
				if m > 0 {
					batch = append(batch, buf[:m]...)
				}
				if rerr != nil {
					break
				}
			}
			_ = conn.SetReadDeadline(time.Time{})
			if renderer != nil {
				if werr := renderer.ProcessChunk(batch); werr != nil {
					return
				}
			} else {
				out := normalizeScrollbackClear(batch)
				var skip bool
				out, skip = compactor.compact(out)
				if skip {
					continue
				}
				if shouldWrapSyncOutput(out) {
					wrapped := make([]byte, 0, len(syncUpdateBegin)+len(out)+len(syncUpdateEnd))
					wrapped = append(wrapped, syncUpdateBegin...)
					wrapped = append(wrapped, out...)
					wrapped = append(wrapped, syncUpdateEnd...)
					out = wrapped
				}
				tracker.observe(out)
				if _, werr := dst.Write(out); werr != nil {
					return
				}
			}
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, syscall.EAGAIN) {
				_ = conn.SetReadDeadline(time.Time{})
				continue
			}
			return
		}
	}
}

func needsMoreFrameBytes(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	// Unclosed synchronized update block.
	if lastBegin := bytes.LastIndex(b, []byte(syncUpdateBegin)); lastBegin >= 0 {
		if bytes.LastIndex(b, []byte(syncUpdateEnd)) < lastBegin {
			return true
		}
	}
	// In-place multi-line redraw that hid the cursor (\x1b[?25l) and has not
	// restored it (\x1b[?25h) yet, or has not yet written as many lines as it
	// moved up.
	if up := leadingCursorUpCount(b); up > 0 {
		lastHide := bytes.LastIndex(b, []byte("\x1b[?25l"))
		lastShow := bytes.LastIndex(b, []byte("\x1b[?25h"))
		if lastHide >= 0 && lastShow < lastHide {
			return true
		}
		if !bytes.HasSuffix(b, []byte("\x1b[?25h")) && !bytes.HasSuffix(b, []byte(syncUpdateEnd)) {
			if bytes.Count(b, []byte("\n")) < up || len(b) >= 4096 {
				return true
			}
		}
	}
	return hasIncompleteTrailingSequence(b)
}

func hasIncompleteTrailingSequence(b []byte) bool {
	// Check for an incomplete trailing UTF-8 rune.
	start := max(len(b)-4, 0)
	for i := len(b) - 1; i >= start; i-- {
		if (b[i] & 0x80) == 0 {
			break
		}
		if (b[i] & 0xc0) == 0xc0 {
			if !utf8.FullRune(b[i:]) {
				return true
			}
			break
		}
	}
	// Check for an incomplete trailing CSI escape sequence (\x1b[...).
	escIdx := bytes.LastIndexByte(b, 0x1b)
	if escIdx >= 0 && len(b)-escIdx <= 16 {
		tail := b[escIdx:]
		if len(tail) == 1 {
			return true
		}
		if tail[1] == '[' {
			for i := 2; i < len(tail); i++ {
				if tail[i] >= 0x40 && tail[i] <= 0x7e {
					return false
				}
			}
			return true
		}
	}
	return false
}

func leadingCursorUpCount(b []byte) int {
	search := b
	if len(search) > 64 {
		search = search[:64]
	}
	return parseLeadingCursorUp(search)
}

func isMultiLineRedraw(b []byte) bool {
	return leadingCursorUpCount(b) > 0
}

func shouldWrapSyncOutput(b []byte) bool {
	if bytes.Contains(b, []byte(syncUpdateBegin)) {
		return false
	}
	return isMultiLineRedraw(b) && (bytes.Contains(b, []byte("\x1b[K")) || bytes.Contains(b, []byte("\x1b[J")))
}

func openPTY() (*os.File, *os.File, error) {
	return pty.Open()
}

func makeRaw(fd int) (*term.State, error) {
	return term.MakeRaw(fd)
}

const (
	terminalStatusReset = "\x18\x1b]7501;state=clear\x07\x1b]21337;status=\x07\x1b]9;4;0\x07"
	mouseTrackingReset  = "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l"
)

func restoreTerm(fd int, state *term.State) {
	_ = term.Restore(fd, state)
	// Detach ends this terminal's attachment, not the background agent's turn.
	// Cancel a truncated escape sequence before clearing its inherited status.
	fmt.Print(terminalStatusReset + "\x1b[?25h\x1b[0m")
}

func clearLocalScreen() {
	fmt.Print("\x1b[H\x1b[2J")
}

func sendWindowSize(w io.Writer) error {
	width, height, err := term.GetSize(0)
	if err != nil {
		return nil
	}
	return writeResizeFrame(w, height, width)
}

func sendWindowSizeBounce(w io.Writer, tracker *redrawTracker, stop <-chan struct{}) error {
	width, height, err := term.GetSize(0)
	if err != nil || width <= 0 || height <= 0 {
		return nil
	}
	go runWindowSizeBounce(w, height, width, tracker, stop)
	return nil
}

func sendWindowSizeSmartReattach(w io.Writer, tracker *redrawTracker, renderer *clientSessionRenderer, stop <-chan struct{}) error {
	width, height, err := term.GetSize(0)
	if err != nil || width <= 0 || height <= 0 {
		return nil
	}
	go func() {
		waitForInitialHistory(tracker, stop)
		select {
		case <-stop:
			return
		default:
		}
		if renderer != nil && renderer.HasServerScrollback() {
			_ = writeResizeFrame(w, height, width)
			return
		}
		runWindowSizeBounce(w, height, width, tracker, stop)
	}()
	return nil
}

func runWindowSizeBounce(w io.Writer, height, width int, tracker *redrawTracker, stop <-chan struct{}) {
	if !tracker.tryStartBounce() {
		return
	}
	defer tracker.finishBounce()

	waitForInitialHistory(tracker, stop)
	select {
	case <-stop:
		return
	default:
	}

	baseClears, _, _ := tracker.snapshot()
	bounceCols := int(bounceColumnCount(uint16(width)))
	if err := writeResizeFrame(w, height, bounceCols); err != nil {
		return
	}

	// Wait for the child process to observe the bounced width and finish
	// flushing any resulting full-screen redraw before restoring the real
	// terminal width. Heavy sessions (~300 KB scrollback) can take 200-350ms
	// to begin emitting \x1b[2J and ~150ms more to flush the stream.
	sawPhase1 := waitForRedrawCycle(tracker, baseClears, 450*time.Millisecond, 35*time.Millisecond, 500*time.Millisecond, stop)
	select {
	case <-stop:
		return
	default:
	}

	phase2Base, _, _ := tracker.snapshot()
	if err := writeResizeFrame(w, height, width); err != nil {
		return
	}

	if sawPhase1 {
		if !waitForRedrawCycle(tracker, phase2Base, 450*time.Millisecond, 35*time.Millisecond, 500*time.Millisecond, stop) {
			select {
			case <-stop:
				return
			default:
			}
			_ = writeResizeFrame(w, height, bounceCols)
			if !sleepOrStop(150*time.Millisecond, stop) {
				return
			}
			_ = writeResizeFrame(w, height, width)
		}
	}
}

func waitForInitialHistory(tracker *redrawTracker, stop <-chan struct{}) {
	if tracker == nil {
		sleepOrStop(15*time.Millisecond, stop)
		return
	}
	deadline := time.Now().Add(25 * time.Millisecond)
	for time.Now().Before(deadline) {
		_, lastRead, _ := tracker.snapshot()
		if !lastRead.IsZero() && time.Since(lastRead) >= 8*time.Millisecond {
			return
		}
		if !sleepOrStop(5*time.Millisecond, stop) {
			return
		}
	}
}

func waitForRedrawCycle(tracker *redrawTracker, baseClears int, clearWait, settleQuiet, maxSettle time.Duration, stop <-chan struct{}) bool {
	if tracker == nil {
		sleepOrStop(150*time.Millisecond, stop)
		return false
	}
	clearDeadline := time.Now().Add(clearWait)
	for {
		clears, _, _ := tracker.snapshot()
		if clears > baseClears {
			break
		}
		rem := time.Until(clearDeadline)
		if rem <= 0 {
			return false
		}
		wait := min(rem, 15*time.Millisecond)
		select {
		case <-stop:
			return false
		case <-tracker.notify:
		case <-time.After(wait):
		}
	}

	settleDeadline := time.Now().Add(maxSettle)
	for {
		_, lastRead, settled := tracker.snapshot()
		quietTarget := settleQuiet
		if settled && quietTarget > 25*time.Millisecond {
			quietTarget = 25 * time.Millisecond
		}
		if !lastRead.IsZero() && time.Since(lastRead) >= quietTarget {
			return true
		}
		rem := time.Until(settleDeadline)
		if rem <= 0 {
			return true
		}
		wait := min(rem, 10*time.Millisecond)
		select {
		case <-stop:
			return false
		case <-tracker.notify:
		case <-time.After(wait):
		}
	}
}

func sleepOrStop(d time.Duration, stop <-chan struct{}) bool {
	select {
	case <-stop:
		return false
	case <-time.After(d):
		return true
	}
}

func writeResizeFrame(w io.Writer, rows, cols int) error {
	payload := make([]byte, 8)
	binary.BigEndian.PutUint32(payload[:4], uint32(rows))
	binary.BigEndian.PutUint32(payload[4:], uint32(cols))
	return writeFrame(w, frameResize, payload)
}

func watchWindowSizeWithTracker(w io.Writer, tracker *redrawTracker, stop <-chan struct{}) {
	watchWindowSizeWithRenderer(w, tracker, nil, stop)
}

func watchWindowSizeWithRenderer(w io.Writer, tracker *redrawTracker, renderer *clientSessionRenderer, stop <-chan struct{}) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	for {
		select {
		case <-stop:
			return
		case <-ch:
			tracker.resetCompactor()
			if renderer != nil {
				width, height, err := term.GetSize(0)
				if err == nil && width > 0 && height > 0 {
					sizeChanged, hasScrollback, _ := renderer.ResizeAndRepaint(height, width)
					if !sizeChanged && !hasScrollback {
						_ = sendWindowSizeBounce(w, tracker, stop)
						continue
					}
					_ = writeResizeFrame(w, height, width)
					continue
				}
			}
			_ = sendWindowSize(w)
		}
	}
}

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	var header [5]byte
	header[0] = typ
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n > 1<<20 {
		return 0, nil, errors.New("frame too large")
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func detachSession(sock string) error {
	conn, err := dialSession(sock)
	if err != nil {
		removeSessionFiles(sock)
		return err
	}
	defer conn.Close()
	return writeFrame(conn, frameDetachAll, nil)
}

func isDetachInput(buf []byte) bool {
	key := detachKeyByte()
	if len(buf) == 1 && buf[0] == key {
		return true
	}
	return enhancedDetachInput(buf, key)
}

func isSwitchInput(buf []byte) bool {
	key := switchKeyByte()
	if len(buf) == 1 && buf[0] == key {
		return true
	}
	return enhancedDetachInput(buf, key)
}

func detachKeyByte() byte {
	return parseCtrlKey(os.Getenv("D_DETACH"), defaultDetachKey)
}

func switchKeyByte() byte {
	return parseCtrlKey(os.Getenv("D_SWITCH"), defaultSwitchKey)
}

func parseCtrlKey(key, fallback string) byte {
	if key == "" {
		key = fallback
	}
	if len(key) >= 2 && key[0] == '^' {
		if key[1] == '?' {
			return 0x7f
		}
		return key[1] & 0x1f
	}
	return key[0]
}

func enhancedDetachInput(buf []byte, ctrl byte) bool {
	if len(buf) < 6 || buf[0] != 0x1b || buf[1] != '[' {
		return false
	}
	key := ctrlToKeyCode(ctrl)
	s := string(buf[2:])
	var code, mod int
	if strings.HasSuffix(s, "u") {
		if _, err := fmt.Sscanf(s, "%d;%du", &code, &mod); err == nil {
			return ctrlModifier(mod) && code == key
		}
	}
	if strings.HasSuffix(s, "~") {
		if _, err := fmt.Sscanf(s, "27;%d;%d~", &mod, &code); err == nil {
			return ctrlModifier(mod) && code == key
		}
	}
	return false
}

func isMouseWheelInput(buf []byte) bool {
	if len(buf) < 6 || buf[0] != 0x1b || buf[1] != '[' {
		return false
	}
	if isSGRMouseWheel(buf) || isURXVTMouseWheel(buf) || isX10MouseWheel(buf) {
		return true
	}
	return false
}

func isSGRMouseWheel(buf []byte) bool {
	if len(buf) < 9 || buf[2] != '<' {
		return false
	}
	last := buf[len(buf)-1]
	if last != 'M' && last != 'm' {
		return false
	}
	var cb, x, y int
	if _, err := fmt.Sscanf(string(buf[3:]), "%d;%d;%d%c", &cb, &x, &y, &last); err != nil {
		return false
	}
	return cb&64 != 0
}

func isURXVTMouseWheel(buf []byte) bool {
	if len(buf) < 8 || buf[len(buf)-1] != 'M' {
		return false
	}
	var cb, x, y int
	if _, err := fmt.Sscanf(string(buf[2:]), "%d;%d;%dM", &cb, &x, &y); err != nil {
		return false
	}
	return cb&64 != 0
}

func isX10MouseWheel(buf []byte) bool {
	if len(buf) != 6 || buf[2] != 'M' {
		return false
	}
	cb := int(buf[3]) - 32
	return cb&64 != 0
}

func ctrlToKeyCode(ctrl byte) int {
	if ctrl >= 1 && ctrl <= 26 {
		return int('a' + ctrl - 1)
	}
	switch ctrl {
	case 28:
		return '\\'
	case 29:
		return ']'
	case 30:
		return '^'
	case 31:
		return '_'
	default:
		return int(ctrl)
	}
}

func ctrlModifier(mod int) bool {
	return mod > 1 && ((mod-1)&4) != 0
}

func copyLines(w io.WriteCloser, lines []string) {
	defer w.Close()
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	for _, line := range lines {
		fmt.Fprintln(bw, line)
	}
}
