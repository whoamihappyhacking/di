package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

const (
	defaultDetachKey = "^]"
	frameInput       = 'i'
	frameResize      = 'w'
	frameDetachAll   = 'D'
)

func main() {
	var err error
	if len(os.Args) > 1 && os.Args[1] == "--server" {
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
	if err := startServer(sock, args); err != nil {
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
	if _, err := exec.LookPath("fzf"); err != nil {
		return errors.New("di: fzf is not installed")
	}
	socks, err := allSockets()
	if err != nil {
		return err
	}
	if len(socks) == 0 {
		return errors.New("di: no sessions found")
	}
	cmd := exec.Command("fzf", "--prompt=di> ", "--height=40%", "--reverse")
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
		return filepath.Join(xdg, "di"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "di"), nil
}

func allSockets() ([]string, error) {
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
	conn, err := net.Dial("unix", sock)
	if err != nil {
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

	server := &ptyServer{master: master, clients: map[net.Conn]struct{}{}}
	go server.broadcastPTY()
	go func() {
		_ = cmd.Wait()
		_ = ln.Close()
		_ = master.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil
		}
		server.add(conn)
		go server.handle(conn)
	}
}

type ptyServer struct {
	mu      sync.Mutex
	master  *os.File
	clients map[net.Conn]struct{}
	history []byte
}

func (s *ptyServer) add(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[conn] = struct{}{}
	if len(s.history) > 0 {
		_, _ = conn.Write(s.history)
	}
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
			s.closeClients()
			return
		}
	}
}

func (s *ptyServer) appendHistory(p []byte) {
	const maxHistory = 1 << 20
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
	conn, err := net.Dial("unix", sock)
	if err != nil {
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
