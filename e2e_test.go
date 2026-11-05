package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

// The end-to-end test drives the real d/di binaries in a pty against a fake
// agent that renders like Ink-based CLIs (static transcript plus an in-place
// redrawn footer, synchronized output, kitty keyboard, clear-and-reprint on
// resize). Whatever the terminal ends up showing through di must match what
// it shows when the agent runs directly.

func TestFakeAgentHelper(t *testing.T) {
	if os.Getenv("DI_FAKE_AGENT") != "1" {
		return
	}
	runFakeAgent()
	os.Exit(0)
}

type fakeAgent struct {
	mu         sync.Mutex
	transcript []string
	input      string
	busy       bool
	frame      int
	footer     int // rows of the dynamic region currently on screen
}

func runFakeAgent() {
	state, err := term.MakeRaw(0)
	if err != nil {
		os.Exit(2)
	}
	defer term.Restore(0, state)
	a := &fakeAgent{transcript: []string{"✻ Welcome to fake agent", ""}}
	os.Stdout.WriteString("\x1b[?2004h\x1b[>1u\x1b[?1004h\x1b]0;✳ fake task\x07")
	a.mu.Lock()
	a.reprintLocked()
	a.mu.Unlock()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			a.mu.Lock()
			a.reprintLocked()
			a.mu.Unlock()
		}
	}()

	buf := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		for _, b := range buf[:n] {
			a.mu.Lock()
			switch {
			case b == 3 || b == 4:
				a.mu.Unlock()
				os.Stdout.WriteString("\x1b[<u\x1b[?2004l\x1b[?1004l\r\n")
				return
			case b == '\r':
				msg := a.input
				a.input = ""
				a.appendLocked("> "+msg, "")
				a.busy = true
				go a.turn(msg)
			case b >= 0x20 && b < 0x7f:
				a.input += string(b)
				a.footerLocked()
			}
			a.mu.Unlock()
		}
	}
}

func (a *fakeAgent) turn(msg string) {
	for i := 0; i < 12; i++ {
		time.Sleep(60 * time.Millisecond)
		a.mu.Lock()
		a.frame++
		a.footerLocked()
		a.mu.Unlock()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	lines := []string{"⏺ Reply to " + msg + ": done ✅", ""}
	for i := 1; i <= 8; i++ {
		lines = append(lines, fmt.Sprintf("  %d. 修改了文件 file_%d.go \x1b[32m+%d\x1b[0m %s", i, i, i*3, strings.Repeat("detail ", i)))
	}
	lines = append(lines, "")
	a.busy = false
	a.appendLocked(lines...)
	os.Stdout.WriteString("\a")
}

func (a *fakeAgent) footerLines() []string {
	cols, _, _ := term.GetSize(1)
	cols = max(cols, 20)
	var lines []string
	if a.busy {
		lines = append(lines, fmt.Sprintf("%c Thinking… (%ds · esc to interrupt)", []rune("⠋⠙⠹⠸⠼⠴⠦⠧")[a.frame%8], a.frame/10))
	}
	bar := strings.Repeat("─", cols-2)
	lines = append(lines, "╭"+bar+"╮", "│ > "+a.input+strings.Repeat(" ", max(cols-5-len(a.input), 0))+"│", "╰"+bar+"╯", "  ? for shortcuts")
	return lines
}

// footerLocked redraws the dynamic region in place, like Ink.
func (a *fakeAgent) footerLocked() {
	var b strings.Builder
	b.WriteString("\x1b[?2026h")
	a.eraseFooter(&b)
	a.writeFooter(&b)
	b.WriteString("\x1b[?2026l")
	os.Stdout.WriteString(b.String())
}

func (a *fakeAgent) eraseFooter(b *strings.Builder) {
	if a.footer > 1 {
		fmt.Fprintf(b, "\r\x1b[%dA", a.footer-1)
	}
	b.WriteString("\r\x1b[J")
}

func (a *fakeAgent) writeFooter(b *strings.Builder) {
	lines := a.footerLines()
	b.WriteString(strings.Join(lines, "\r\n"))
	a.footer = len(lines)
}

func (a *fakeAgent) appendLocked(lines ...string) {
	var b strings.Builder
	b.WriteString("\x1b[?2026h")
	a.eraseFooter(&b)
	for _, line := range lines {
		b.WriteString(line + "\r\n")
	}
	a.transcript = append(a.transcript, lines...)
	a.writeFooter(&b)
	b.WriteString("\x1b[?2026l")
	os.Stdout.WriteString(b.String())
}

// reprintLocked clears screen and scrollback and prints everything again, the
// way Claude Code reacts to a terminal resize.
func (a *fakeAgent) reprintLocked() {
	var b strings.Builder
	b.WriteString("\x1b[?2026h\x1b[2J\x1b[3J\x1b[H")
	for _, line := range a.transcript {
		b.WriteString(line + "\r\n")
	}
	a.writeFooter(&b)
	b.WriteString("\x1b[?2026l")
	os.Stdout.WriteString(b.String())
}

// ptyTerm runs a command in a pty and mirrors its output into an emulator
// standing in for the user's terminal.
type ptyTerm struct {
	t      *testing.T
	cmd    *exec.Cmd
	master *os.File
	mu     sync.Mutex
	screen *vterm
	raw    bytes.Buffer
	done   chan struct{}
}

func startPtyTerm(t *testing.T, rows, cols int, env []string, name string, args ...string) *ptyTerm {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = env
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		t.Fatal(err)
	}
	p := &ptyTerm{t: t, cmd: cmd, master: master, screen: newVTerm(rows, cols), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		buf := make([]byte, 32<<10)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				p.mu.Lock()
				p.screen.Write(buf[:n])
				p.raw.Write(buf[:n])
				// Answer terminal queries like a real terminal would.
				if replies := p.screen.DrainReplies(); len(replies) > 0 {
					_, _ = master.Write(replies)
				}
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = master.Close()
	})
	return p
}

func (p *ptyTerm) send(s string) {
	_, _ = p.master.Write([]byte(s))
}

func (p *ptyTerm) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(captureState(p.screen).rows, "\n")
}

func (p *ptyTerm) waitFor(want string) {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(p.text(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Fatalf("timed out waiting for %q; screen:\n%s", want, p.text())
}

func (p *ptyTerm) waitExit() {
	p.t.Helper()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		p.t.Fatalf("process did not exit; screen:\n%s", p.text())
	}
	_ = p.cmd.Wait()
}

func (p *ptyTerm) resize(rows, cols int) {
	p.mu.Lock()
	p.screen.Resize(rows, cols)
	p.mu.Unlock()
	_ = pty.Setsize(p.master, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

func (p *ptyTerm) state() (screenState, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := captureState(p.screen)
	return st, scrollbackText(p.screen)
}

func settle() { time.Sleep(400 * time.Millisecond) }

func assertSameTerminal(t *testing.T, label string, got, want *ptyTerm) {
	t.Helper()
	gs, gsb := got.state()
	ws, wsb := want.state()
	if g, w := strings.Join(gs.rows, "\n"), strings.Join(ws.rows, "\n"); g != w {
		t.Fatalf("%s: screen differs\n--- through di ---\n%s\n--- direct ---\n%s", label, g, w)
	}
	if gs.curR != ws.curR || gs.curC != ws.curC || gs.cursorVis != ws.cursorVis {
		t.Fatalf("%s: cursor %d,%d,%v want %d,%d,%v", label, gs.curR, gs.curC, gs.cursorVis, ws.curR, ws.curC, ws.cursorVis)
	}
	if g, w := strings.Join(gsb, "\n"), strings.Join(wsb, "\n"); g != w {
		t.Fatalf("%s: scrollback differs\n--- through di ---\n%s\n--- direct ---\n%s", label, g, w)
	}
	if gs.bracketed != ws.bracketed || fmt.Sprint(gs.kitty) != fmt.Sprint(ws.kitty) || gs.title != ws.title {
		t.Fatalf("%s: modes differ: paste %v/%v kitty %v/%v title %q/%q", label, gs.bracketed, ws.bracketed, gs.kitty, ws.kitty, gs.title, ws.title)
	}
}

func TestEndToEndAgentSessionMatchesDirectRun(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and drives the real binary")
	}
	tmp, err := os.MkdirTemp("", "di-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	bin := filepath.Join(tmp, "bin", "d")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// Session servers outlive the test process; end them even when the test fails.
	defer func() {
		socks, _ := filepath.Glob(filepath.Join(tmp, "di", "*.sock"))
		for _, sock := range socks {
			_ = exec.Command(bin, "--kill", sock).Run()
		}
	}()
	env := append(os.Environ(), "XDG_RUNTIME_DIR="+tmp, "DI_FAKE_AGENT=1", "SHELL=/bin/sh", "TERM=xterm-256color")
	agent := []string{os.Args[0], "-test.run=^TestFakeAgentHelper$"}
	// Every d invocation must use the private session directory, never the user's.
	listCmd := func() *exec.Cmd {
		cmd := exec.Command(bin, "--list")
		cmd.Env = env
		return cmd
	}

	const rows, cols = 16, 56
	direct := startPtyTerm(t, rows, cols, env, agent[0], agent[1:]...)
	viaDi := startPtyTerm(t, rows, cols, env, bin, agent...)
	for _, p := range []*ptyTerm{direct, viaDi} {
		p.waitFor("? for shortcuts")
		p.send("hello")
		p.waitFor("│ > hello")
		p.send("\r")
		p.waitFor("Thinking")
	}

	// Detach in the middle of the turn; the agent finishes in the background.
	viaDi.send("\x1d")
	viaDi.waitExit()
	if !strings.Contains(viaDi.text(), "[di] detached") {
		t.Fatalf("no detach message:\n%s", viaDi.text())
	}
	viaDi.mu.Lock()
	host := viaDi.screen
	leftModes := host.bracketedPaste || host.primary.kittyLen != 0 || host.focusEvents
	history := strings.Join(scrollbackText(host), "\n")
	viaDi.mu.Unlock()
	if leftModes {
		t.Fatal("detach left session modes enabled in the terminal")
	}
	if !strings.Contains(history, "> hello") || !strings.Contains(history, "Welcome to fake agent") {
		t.Fatalf("the session's last screen is not in the terminal's scrollback after detach:\n%s", history)
	}
	direct.waitFor("8. 修改了文件")
	settle()

	list, err := listCmd().Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(list), "待处理") || !strings.Contains(string(list), "fake task") {
		t.Fatalf("list after a bell while detached:\n%s", list)
	}

	// Reattach from a fresh terminal: it must look exactly like the direct run.
	socks, _ := filepath.Glob(filepath.Join(tmp, "di", "*.sock"))
	if len(socks) != 1 {
		t.Fatalf("sessions: %v", socks)
	}
	again := startPtyTerm(t, rows, cols, env, bin, "--attach", sessionName(socks[0]))
	again.waitFor("8. 修改了文件")
	settle()
	assertSameTerminal(t, "reattach", again, direct)

	// Typing and resizing behave as if di were not there.
	for _, p := range []*ptyTerm{again, direct} {
		p.send("next")
		p.waitFor("│ > next")
		p.resize(rows+4, cols-10)
		p.waitFor("│ > next")
	}
	settle()
	assertSameTerminal(t, "resize", again, direct)

	if out, _ := listCmd().Output(); !strings.Contains(string(out), "当前") && !strings.Contains(string(out), "空闲") && !strings.Contains(string(out), "运行中") {
		t.Fatalf("list while attached:\n%s", out)
	}
	again.send("\x04")
	direct.send("\x04")
	again.waitExit()
	direct.waitExit()
	// When the program exits by itself the terminal must look exactly as if it
	// had run directly: same screen, cursor right after its last output.
	assertSameTerminal(t, "exit", again, direct)

	// d run from a shell inside a session switches the outer terminal to the
	// new session instead of nesting; Ctrl-] then detaches from it directly.
	shell := startPtyTerm(t, rows, cols, env, bin, "sh")
	shell.waitFor("$")
	shell.send(shellQuote(bin) + " " + shellQuote(agent[0]) + " " + shellQuote(agent[1]) + "\r")
	shell.waitFor("? for shortcuts")
	shell.send("\x1d")
	shell.waitExit()
	if !strings.Contains(shell.text(), "[di] detached") {
		t.Fatalf("after detaching from the switched-to session:\n%s", shell.text())
	}
	out, _ := listCmd().Output()
	if n := strings.Count(strings.TrimSpace(string(out)), "\n") + 1; n != 2 {
		t.Fatalf("expected the shell and the agent session:\n%s", out)
	}
	for _, sock := range must(filepath.Glob(filepath.Join(tmp, "di", "*.sock"))) {
		k := exec.Command(bin, "--kill", sock)
		k.Env = env
		_ = k.Run()
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := listCmd().Output()
		if len(bytes.TrimSpace(out)) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session still listed after exit:\n%s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
