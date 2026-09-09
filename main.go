package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
	"golang.org/x/term"
)

const (
	defaultDetachKey = "^]"
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
	args = expandCommandAlias(args)

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
	if err := startServer(sock, args); err != nil {
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
	aliases := loadShellAliases()
	return expandAliasArgs(args, aliases)
}

func expandAliasArgs(args []string, aliases map[string]string) []string {
	if len(args) == 0 || len(aliases) == 0 {
		return args
	}
	value, ok := aliases[args[0]]
	if !ok || value == "" {
		return args
	}
	words := splitAliasWords(value)
	if len(words) == 0 {
		return args
	}
	return append(words, args[1:]...)
}

type cachedAliases struct {
	Key     string            `json:"key"`
	Aliases map[string]string `json:"aliases"`
}

var (
	aliasOnce sync.Once
	aliasMap  map[string]string
)

func loadShellAliases() map[string]string {
	aliasOnce.Do(func() { aliasMap = loadAliasesFromCache() })
	return aliasMap
}

func loadAliasesFromCache() map[string]string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		return nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return queryShellAliases(shell)
	}
	return loadAliasesFrom(shell, filepath.Join(dir, "di", "aliases.json"))
}

func loadAliasesFrom(shell, cacheFile string) map[string]string {
	key := aliasCacheKey(shell)
	if data, err := os.ReadFile(cacheFile); err == nil {
		var cached cachedAliases
		if json.Unmarshal(data, &cached) == nil && cached.Key == key {
			return cached.Aliases
		}
	}
	aliases := queryShellAliases(shell)
	if aliases == nil {
		return nil
	}
	data, err := json.Marshal(cachedAliases{Key: key, Aliases: aliases})
	if err != nil {
		return aliases
	}
	if os.MkdirAll(filepath.Dir(cacheFile), 0o700) == nil {
		writeAliasCache(cacheFile, data)
	}
	return aliases
}

func writeAliasCache(cacheFile string, data []byte) {
	tmp, err := os.CreateTemp(filepath.Dir(cacheFile), "aliases-*.tmp")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), cacheFile)
}

func aliasCacheKey(shell string) string {
	home, _ := os.UserHomeDir()
	var files []string
	switch filepath.Base(shell) {
	case "zsh":
		if zd := os.Getenv("ZDOTDIR"); zd != "" {
			files = []string{filepath.Join(zd, ".zshenv"), filepath.Join(zd, ".zprofile"), filepath.Join(zd, ".zshrc"), filepath.Join(zd, ".zlogin")}
		} else {
			files = []string{filepath.Join(home, ".zshenv"), filepath.Join(home, ".zprofile"), filepath.Join(home, ".zshrc"), filepath.Join(home, ".zlogin")}
		}
	case "bash":
		files = []string{"/etc/bash.bashrc", filepath.Join(home, ".bashrc"), filepath.Join(home, ".profile")}
	}
	h := sha256.New()
	h.Write([]byte(shell))
	for _, f := range files {
		if info, err := os.Stat(f); err == nil {
			fmt.Fprintf(h, "\x00%s:%d:%d", f, info.ModTime().UnixNano(), info.Size())
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func queryShellAliases(shell string) map[string]string {
	if shell == "" {
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
		line = strings.TrimPrefix(line, "alias ")
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

func splitAliasWords(value string) []string {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(value); {
		switch value[i] {
		case ' ', '\t', '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
			i++
		case '\'':
			inWord = true
			i++
			for i < len(value) && value[i] != '\'' {
				cur.WriteByte(value[i])
				i++
			}
			if i < len(value) {
				i++
			}
		case '"':
			inWord = true
			i++
			for i < len(value) && value[i] != '"' {
				c := value[i]
				if c == '\\' && i+1 < len(value) {
					i++
					c = value[i]
				}
				cur.WriteByte(c)
				i++
			}
			if i < len(value) {
				i++
			}
		case '\\':
			inWord = true
			if i+1 < len(value) {
				i++
				cur.WriteByte(value[i])
			}
			i++
		default:
			inWord = true
			cur.WriteByte(value[i])
			i++
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
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

Environment:
  D_DETACH=^B                 override the detach key

Notes:
  d expands the first command token using shell aliases ($SHELL -ic alias);
  extra arguments are appended after the expansion, so d app -x runs app -f
  /etc/app.conf -x for an alias app='app -f /etc/app.conf'.
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
	if _, err := exec.LookPath("fzf"); err != nil {
		return errors.New("di: fzf is not installed")
	}
	sessions, err := allSessions()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		return errors.New("di: no sessions found")
	}
	width, height := terminalSize()
	lines := make([]string, 0, len(sessions))
	for _, session := range sessions {
		lines = append(lines, session.displayLine(width))
	}
	cmdArgs := fzfArgs(os.Args[0], height)
	cmd := exec.Command("fzf", cmdArgs...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	go copyLines(stdin, lines)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 130 {
			return nil
		}
		return err
	}
	selected := strings.TrimSpace(string(out))
	if selected == "" {
		return nil
	}
	fields := strings.Split(selected, "\t")
	if len(fields) == 0 || fields[0] == "" {
		return nil
	}
	return attach(fields[0])
}

func fzfArgs(executable string, height int) []string {
	preview := fmt.Sprintf("%s --preview {1}", shellQuote(executable))
	if height <= 0 {
		height = 24
	}
	previewHeight := max(height-6, 1)
	return []string{
		"--prompt=di> ",
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
	terminal := vt10x.New(vt10x.WithSize(cols, rows))
	if _, err := terminal.Write(output); err != nil {
		return nil
	}

	lines := strings.Split(terminal.String(), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func sessionPreviewOutput(sock string) ([]byte, error) {
	conn, err := dialSession(sock)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// The server keeps the socket open after replaying history. A short read
	// deadline lets fzf refresh previews without waiting for the session.
	if err := conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		return nil, err
	}
	var output []byte
	buf := make([]byte, 4096)
	for {
		n, readErr := conn.Read(buf)
		if n > 0 {
			output = append(output, buf[:n]...)
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
	Sock string
	Meta sessionMeta
}

func (s sessionInfo) displayLine(width int) string {
	name := s.Meta.Name
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(s.Sock), ".sock")
	}
	pwd := s.Meta.PWD
	if pwd == "" {
		pwd = "-"
	}
	cmd := strings.Join(s.Meta.Command, " ")
	if cmd == "" {
		cmd = name
	}

	if width <= 0 {
		width = 120
	}
	available := width - 8
	if available < 48 {
		available = 48
	}
	pwdWidth := clampInt(available*3/10, 18, 32)
	nameWidth := clampInt(available/5, 16, 28)
	cmdWidth := available - pwdWidth - nameWidth - 2
	if cmdWidth < 20 {
		cmdWidth = 20
	}
	return fmt.Sprintf(
		"%s\t%-*s\t%-*s\t%-*s",
		s.Sock,
		pwdWidth, fitField(pwd, pwdWidth),
		cmdWidth, fitField(cmd, cmdWidth),
		nameWidth, fitField(name, nameWidth),
	)
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
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
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
	for _, session := range sessions {
		meta := session.Meta
		name := meta.Name
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(session.Sock), ".sock")
		}
		fmt.Printf("%-56s\t%-56s\t%s\n", meta.PWD, strings.Join(meta.Command, " "), name)
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
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("d: session socket was not created: %s", sock)
}

func attach(sock string) error {
	conn, err := dialSession(sock)
	if err != nil {
		removeSessionFiles(sock)
		return err
	}
	defer conn.Close()

	oldTerm, err := makeRaw(0)
	if err != nil {
		return err
	}
	defer restoreTerm(0, oldTerm)

	_ = sendWindowSize(conn)
	go watchWindowSize(conn)

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(os.Stdout, conn)
		close(done)
	}()

	inputErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				if isDetachInput(chunk) {
					inputErr <- nil
					return
				}
				if isMouseWheelInput(chunk) {
					continue
				}
				if err := writeFrame(conn, frameInput, chunk); err != nil {
					inputErr <- err
					return
				}
			}
			if err != nil {
				inputErr <- err
				return
			}
		}
	}()

	select {
	case <-done:
		return nil
	case err := <-inputErr:
		if err == nil {
			clearLocalScreen()
		}
		return err
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

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = slave.Close()

	server := &ptyServer{
		master:      master,
		clients:     map[net.Conn]struct{}{},
		clientReady: make(chan struct{}),
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
	mu          sync.Mutex
	master      *os.File
	clients     map[net.Conn]struct{}
	history     []byte
	readyOnce   sync.Once
	clientReady chan struct{}
	clientWG    sync.WaitGroup
	closing     bool
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
	if len(s.history) > 0 {
		_, _ = conn.Write(s.history)
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

func (s *ptyServer) broadcastPTY() {
	buf := make([]byte, 4096)
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.appendHistory(buf[:n])
			for conn := range s.clients {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					_ = conn.Close()
					delete(s.clients, conn)
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (s *ptyServer) appendHistory(p []byte) {
	const maxHistory = 64 << 10
	s.history = append(s.history, p...)
	if len(s.history) > maxHistory {
		s.history = append([]byte(nil), s.history[len(s.history)-maxHistory:]...)
	}
}

func (s *ptyServer) handle(conn net.Conn) {
	defer s.remove(conn)
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
				rows := binary.BigEndian.Uint32(payload[:4])
				cols := binary.BigEndian.Uint32(payload[4:])
				_ = pty.Setsize(s.master, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
			}
		case frameDetachAll:
			s.closeClients()
			return
		}
	}
}

func openPTY() (*os.File, *os.File, error) {
	return pty.Open()
}

func makeRaw(fd int) (*term.State, error) {
	return term.MakeRaw(fd)
}

func restoreTerm(fd int, state *term.State) {
	_ = term.Restore(fd, state)
	fmt.Print("\x1b[?25h\x1b[0m")
}

func clearLocalScreen() {
	fmt.Print("\x1b[H\x1b[2J")
}

func sendWindowSize(w io.Writer) error {
	width, height, err := term.GetSize(0)
	if err != nil {
		return nil
	}
	payload := make([]byte, 8)
	binary.BigEndian.PutUint32(payload[:4], uint32(height))
	binary.BigEndian.PutUint32(payload[4:], uint32(width))
	return writeFrame(w, frameResize, payload)
}

func watchWindowSize(w io.Writer) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	for range ch {
		_ = sendWindowSize(w)
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

func detachKeyByte() byte {
	key := os.Getenv("D_DETACH")
	if key == "" {
		key = defaultDetachKey
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
