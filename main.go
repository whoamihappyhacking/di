package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// Linux allows 107 pathname bytes; 103 also fits macOS sockaddr_un.
const maxSocketPathLen = 103

type sessionMeta struct {
	Name      string   `json:"name"`
	Command   []string `json:"command"`
	PWD       string   `json:"pwd"`
	StartedAt string   `json:"started_at"`
}

func main() {
	if err := run(filepath.Base(os.Args[0]), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(prog string, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help":
			fmt.Print(helpText())
			return nil
		case "install":
			return installSelf()
		case "--server":
			return runServer(args[1:])
		case "--preview":
			if len(args) < 2 {
				return nil
			}
			return previewSession(args[1])
		case "--picker-list":
			return printPickerList(args[1:])
		case "--list", "-l":
			return listSessions()
		case "--attach", "-a":
			if len(args) < 2 {
				return errors.New("usage: d --attach <name>")
			}
			sock, err := resolveSession(args[1])
			if err != nil {
				return err
			}
			return attachSession(sock)
		case "--detach":
			if len(args) < 2 {
				return errors.New("usage: d --detach <name>")
			}
			sock, err := resolveSession(args[1])
			if err != nil {
				return err
			}
			return detachSession(sock)
		case "--kill", "-k":
			if len(args) < 2 {
				return errors.New("usage: d --kill <name>")
			}
			sock, err := resolveSession(args[1])
			if err != nil {
				return err
			}
			return killSession(sock)
		case "--kill-confirm":
			if len(args) < 2 {
				return nil
			}
			return confirmKillSession(args[1])
		}
	}
	if prog == "di" {
		return pickAndAttach(strings.Join(args, " "))
	}
	return runD(args)
}

func runD(args []string) error {
	if len(args) == 0 {
		return errors.New(usage())
	}
	if strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("d: unknown option %s\n%s", args[0], usage())
	}
	commandArgs := expandCommandAlias(args)
	if _, err := exec.LookPath(commandArgs[0]); err != nil {
		return fmt.Errorf("d: %s: command not found", args[0])
	}
	dir, err := sessionDir()
	if err != nil {
		return err
	}
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
	if outer := insideSession(); outer != "" && requestSwitch(outer, sock) {
		// The terminal showing this session switches over; nothing is nested.
		return nil
	}
	if !term.IsTerminal(0) || !term.IsTerminal(1) {
		fmt.Printf("started %s in the background\n", sessionName(sock))
		return nil
	}
	return attachSession(sock)
}

func usage() string {
	return "usage: d <command> [args...]\n       di [query]\n       d --list | --attach <name> | --detach <name> | --kill <name>\n       d install"
}

func helpText() string {
	return `di - detachable terminal sessions for long-running commands and agents

Usage:
  d <command> [args...]       start a new session and attach to it
  di [query]                  pick a session with fzf (query pre-filters) and attach
  d --list                    list sessions with their state
  d --attach <name>           attach to a session by name (unique prefix or substring)
  d --detach <name>           detach every client attached to a session
  d --kill <name>             end a session (or dismiss one that has exited)
  d install                   install this binary as ~/.local/bin/d and link di
  d --help, di --help         show this help

Keys while attached:
  Ctrl-]                      detach; the command keeps running
  Ctrl-\                      switch to another session

Keys in the picker:
  Enter                       attach
  Ctrl-\                      move to the next session
  Ctrl-X                      end the selected session
  Ctrl-R                      refresh states

Session states:
  ● 运行中 busy               produced output in the last few seconds
  ◆ 待处理 waiting            rang the bell or sent a notification while detached
  ◆ 有更新 new                went quiet with output you have not seen yet
  ○ 空闲   idle               nothing new since you last looked
  ✕ 已退出 exited             the command ended; attach once to see its last screen

Output passes straight through while attached, so scrollback, mouse, colors and
key handling behave exactly as when the command runs in the terminal directly.
Reattaching restores the screen, scrollback and terminal modes. Running d or
di inside a session switches the outer terminal instead of nesting.

Environment:
  D_DETACH=^B                 override the detach key
  D_SWITCH=^\                 override the session switch key

Notes:
  d reads bash/zsh aliases from $SHELL startup files for each new session.
  Alias commands run in that shell; additional arguments stay literal.
  The picker requires fzf; starting and listing sessions do not.
`
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
		line = strings.TrimSpace(strings.TrimPrefix(line, "alias ")) // bash
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
	if len(value) < 2 {
		return value
	}
	switch value[0] {
	case '\'':
		if value[len(value)-1] != '\'' {
			return value
		}
		return strings.ReplaceAll(value[1:len(value)-1], `'\''`, `'`)
	case '"':
		if value[len(value)-1] != '"' {
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

func sessionName(sock string) string {
	return strings.TrimSuffix(filepath.Base(sock), ".sock")
}

// resolveSession accepts a socket path, an exact session name, or a unique
// prefix or substring of one.
func resolveSession(arg string) (string, error) {
	if strings.Contains(arg, "/") && isSocket(arg) {
		return arg, nil
	}
	dir, err := sessionDir()
	if err != nil {
		return "", err
	}
	exact := filepath.Join(dir, arg+".sock")
	if isSocket(exact) {
		return exact, nil
	}
	entries, _ := os.ReadDir(dir)
	var prefix, contains []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".sock")
		if !ok {
			continue
		}
		if strings.HasPrefix(name, arg) {
			prefix = append(prefix, name)
		} else if strings.Contains(name, arg) {
			contains = append(contains, name)
		}
	}
	for _, matches := range [][]string{prefix, contains} {
		switch len(matches) {
		case 0:
			continue
		case 1:
			return filepath.Join(dir, matches[0]+".sock"), nil
		default:
			return "", fmt.Errorf("d: %q matches several sessions:\n  %s", arg, strings.Join(matches, "\n  "))
		}
	}
	return "", fmt.Errorf("d: no session matches %q", arg)
}

func labelFor(args []string) string {
	re := regexp.MustCompile(`[^A-Za-z0-9._+-]+`)
	label := strings.Trim(re.ReplaceAllString(strings.Join(args, " "), "-"), "-")
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
		Name:      sessionName(sock),
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
	if data, err := os.ReadFile(metaPath(sock)); err == nil {
		_ = json.Unmarshal(data, &meta)
	}
	if meta.Name == "" {
		meta.Name = sessionName(sock)
	}
	return meta
}

func isSocket(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
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
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	env := os.Environ()
	if cols, rows := terminalSize(); cols > 0 && rows > 0 {
		env = append(env, fmt.Sprintf("DI_INITIAL_COLS=%d", cols), fmt.Sprintf("DI_INITIAL_ROWS=%d", rows))
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
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("d: session socket was not created: %s", sock)
}

func positiveEnvInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
