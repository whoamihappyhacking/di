package main

import (
	"bufio"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const defaultDetachKey = "^G"

//go:embed assets/dtach-linux-amd64
var embeddedDtach []byte

func main() {
	name := filepath.Base(os.Args[0])
	var err error
	if len(os.Args) > 1 && os.Args[1] == "install" {
		err = installSelf()
	} else if name == "di" {
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

	if _, err := dtachPath(); err != nil {
		return err
	}
	dir, err := sessionDir()
	if err != nil {
		return err
	}
	switch args[0] {
	case "--list":
		return listSessions(dir)
	case "--detach":
		if len(args) < 2 {
			return errors.New("usage: d --detach <name>")
		}
		return detachSession(filepath.Join(dir, args[1]+".sock"))
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sock := filepath.Join(dir, labelFor(args)+".sock")
	if isSocket(sock) {
		return attach(sock)
	}
	if err := createDetached(sock, args); err != nil {
		return err
	}
	if err := waitSocket(sock, 2*time.Second); err != nil {
		return err
	}
	return attach(sock)
}

func usage() string {
	return "usage: d <command> [args...]\n       d install\n       d --list\n       d --detach <name>"
}

func installSelf() error {
	if _, err := exec.LookPath("go"); err != nil {
		return errors.New("d install: go is not installed")
	}
	root, err := sourceRoot()
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

	tmp, err := os.CreateTemp(binDir, ".d-build-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	cmd := exec.Command("go", "build", "-o", tmpPath, ".")
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}

	dPath := filepath.Join(binDir, "d")
	diPath := filepath.Join(binDir, "di")
	if err := os.Rename(tmpPath, dPath); err != nil {
		return err
	}
	if err := os.Chmod(dPath, 0o755); err != nil {
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

func sourceRoot() (string, error) {
	if cwd, err := os.Getwd(); err == nil && hasProjectFiles(cwd) {
		return cwd, nil
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for {
			if hasProjectFiles(dir) {
				return dir, nil
			}
			next := filepath.Dir(dir)
			if next == dir {
				break
			}
			dir = next
		}
	}
	home, err := os.UserHomeDir()
	if err == nil {
		root := filepath.Join(home, "pj", "di")
		if hasProjectFiles(root) {
			return root, nil
		}
	}
	return "", errors.New("d install: run from the di source directory or place it at ~/pj/di")
}

func hasProjectFiles(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		return false
	}
	return true
}

func pickAndAttach() error {
	if _, err := dtachPath(); err != nil {
		return err
	}
	if _, err := exec.LookPath("fzf"); err != nil {
		return errors.New("di: fzf is not installed")
	}
	socks, err := allSockets()
	if err != nil {
		return err
	}
	if len(socks) == 0 {
		return errors.New("di: no dtach sessions found")
	}
	cmd := exec.Command("fzf", "--prompt=dtach> ", "--height=40%", "--reverse")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	go copyLines(stdin, socks)
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	selected := strings.TrimSpace(string(out))
	if selected == "" {
		return nil
	}
	return attach(selected)
}

func sessionDir() (string, error) {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "dtach"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "dtach"), nil
}

func allSockets() ([]string, error) {
	dir, err := sessionDir()
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	dirs := []string{dir}
	if home != "" {
		fallback := filepath.Join(home, ".local", "state", "dtach")
		if fallback != dir {
			dirs = append(dirs, fallback)
		}
	}
	var socks []string
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			path := filepath.Join(d, e.Name())
			if strings.HasSuffix(e.Name(), ".sock") && isSocket(path) {
				socks = append(socks, path)
			}
		}
	}
	return socks, nil
}

func listSessions(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if strings.HasSuffix(e.Name(), ".sock") && isSocket(path) {
			fmt.Println(strings.TrimSuffix(e.Name(), ".sock"))
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

func isSocket(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSocket != 0
}

func createDetached(sock string, args []string) error {
	dtach, err := dtachPath()
	if err != nil {
		return err
	}
	dargs := append([]string{"-n", sock, "-r", "ctrl_l"}, args...)
	cmd := exec.Command(dtach, dargs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
	dtach, err := dtachPath()
	if err != nil {
		return err
	}
	if mouseEnabled() {
		fmt.Print("\x1b[?1000h\x1b[?1002h\x1b[?1006h")
		defer fmt.Print("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l")
	}
	cmd := exec.Command(dtach, "-a", sock, "-e", detachKey(), "-r", "ctrl_l")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func detachKey() string {
	if key := os.Getenv("D_DETACH"); key != "" {
		return key
	}
	return defaultDetachKey
}

func dtachPath() (string, error) {
	if path, err := exec.LookPath("dtach"); err == nil {
		return path, nil
	}
	return embeddedDtachPath()
}

func embeddedDtachPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(embeddedDtach)
	short := hex.EncodeToString(sum[:])[:16]
	dir := filepath.Join(home, ".cache", "di")
	path := filepath.Join(dir, "dtach-linux-amd64-"+short)
	if executableFile(path) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".dtach-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(embeddedDtach); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return path, nil
}

func executableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

func mouseEnabled() bool {
	switch strings.ToLower(os.Getenv("D_MOUSE")) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func detachSession(sock string) error {
	pids, err := attachPIDs(sock)
	if err != nil {
		return err
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	return nil
}

func attachPIDs(sock string) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		var pid int
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		fields := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		if len(fields) >= 3 && filepath.Base(fields[0]) == "dtach" && fields[1] == "-a" && fields[2] == sock {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func copyLines(w io.WriteCloser, lines []string) {
	defer w.Close()
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	for _, line := range lines {
		fmt.Fprintln(bw, line)
	}
}
