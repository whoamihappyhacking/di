package main

import (
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Wire protocol. Every message in both directions is a frame:
// [type byte][payload length uint32 big endian][payload].
//
// A connection's first client frame selects what it is: an attach, a status
// query, a preview request, or a control command. Clients from older di
// builds open with a bare resize or input frame and receive an unframed
// stream instead.
const (
	frameAttach    = 'a' // client: rows, cols
	frameInput     = 'i' // client: raw input bytes
	frameResize    = 'w' // client: rows, cols
	frameDetachAll = 'D' // client: detach every attached client
	frameStatus    = 'q' // client: ask for sessionStatus
	framePreview   = 'p' // client: rows, cols; ask for status plus a screen preview
	frameKill      = 'K' // client: terminate the session
	frameSwitchReq = 'S' // client: ask attached clients to show another session (payload: socket path)

	frameHello        = 'H' // server: helloInfo JSON, first frame of an attach
	frameOutput       = 'o' // server: terminal output, written verbatim by the client
	frameExit         = 'x' // server: exitInfo JSON, the session has ended
	frameDetach       = 'd' // server: this client was detached by someone else
	frameSwitch       = 's' // server: switch this client to the session in the payload
	frameReply        = 'Q' // server: sessionStatus JSON
	framePreviewReply = 'P'

	maxFrameLen = 64 << 20
)

const (
	dialTimeout       = 300 * time.Millisecond
	serverStartWait   = 2 * time.Second
	childStartTimeout = 2 * time.Second
	busyWindow        = 3 * time.Second
	exitedLinger      = 72 * time.Hour
	// A client that cannot keep up gets a fresh snapshot instead of an
	// ever-growing backlog, so a stalled terminal never blocks the session.
	maxClientBacklog = 32 << 20
)

type helloInfo struct {
	Version  int  `json:"v"`
	Exited   bool `json:"exited,omitempty"`
	ExitCode int  `json:"exit_code,omitempty"`
}

type exitInfo struct {
	Code int `json:"code"`
}

// sessionStatus is what the picker and `d --list` show for each session.
type sessionStatus struct {
	State      string    `json:"state"` // busy | waiting | new | idle | exited
	Attached   int       `json:"attached"`
	Title      string    `json:"title,omitempty"`
	Activity   string    `json:"activity,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	Notice     string    `json:"notice,omitempty"`
	LastOutput time.Time `json:"last_output,omitzero"`
	LastSeen   time.Time `json:"last_seen,omitzero"`
	ExitCode   int       `json:"exit_code,omitempty"`
	ExitedAt   time.Time `json:"exited_at,omitzero"`
	PID        int       `json:"pid,omitempty"`
	Rows       int       `json:"rows,omitempty"`
	Cols       int       `json:"cols,omitempty"`
	Screen     string    `json:"screen,omitempty"`
	// Restore is set by servers that keep their own restore record; clients
	// write one for sessions run by older builds.
	Restore bool `json:"restore,omitempty"`
	Legacy  bool `json:"-"`
}

type sessionServer struct {
	mu      sync.Mutex
	sock    string
	master  *os.File
	cmd     *exec.Cmd
	vt      *vterm
	clients map[*serverClient]struct{}
	primary *serverClient // the most recently attached client
	rows    int
	cols    int

	firstAttach     chan struct{}
	firstAttachOnce sync.Once
	childDone       chan struct{}
	childExitCode   int
	started         bool
	pid             int

	exited    bool
	exitCode  int
	exitedAt  time.Time
	killed    bool
	done      chan struct{}
	closeOnce sync.Once

	// restorePath is this session's restore record, kept while the command
	// runs; restoreData is its content, to put it back when the command's
	// exit turns out to have been part of a shutdown. terminated is set when
	// the server itself is told to stop, as on a system shutdown; the record
	// then stays to mark the interruption.
	restorePath      string
	restoreData      []byte
	restoreDroppedAt time.Time
	terminated       bool

	lastOutput  time.Time
	lastSeen    time.Time
	attention   bool
	bellsSeen   int
	noticesSeen int
}

func runServer(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: d --server <socket> <command> [args...]")
	}
	sock, cmdArgs := args[0], args[1:]
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return err
	}
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer os.Remove(metaPath(sock))
	defer os.Remove(sock)
	defer ln.Close()
	// The record is written once the socket exists, so a record without a
	// socket always means the server went away.
	var restorePath string
	var restoreData []byte
	if meta := readSessionMeta(sock); len(meta.Command) > 0 {
		restorePath, restoreData, err = writeRestoreRecord(sock, meta, false)
		if err != nil {
			restorePath = "" // the session runs all the same, just without a record
		}
	}

	master, slave, err := pty.Open()
	if err != nil {
		return err
	}
	defer master.Close()

	rows := positiveEnvInt("DI_INITIAL_ROWS", 24)
	cols := positiveEnvInt("DI_INITIAL_COLS", 80)
	_ = pty.Setsize(master, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.Env = append(filterInternalEnv(os.Environ()), "DI_SESSION="+sock)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

	s := &sessionServer{
		sock:        sock,
		master:      master,
		cmd:         cmd,
		vt:          newVTerm(rows, cols),
		clients:     map[*serverClient]struct{}{},
		rows:        rows,
		cols:        cols,
		firstAttach: make(chan struct{}),
		childDone:   make(chan struct{}),
		done:        make(chan struct{}),

		restorePath: restorePath,
		restoreData: restoreData,
	}
	sigs := make(chan os.Signal, 1)
	// What a system shutdown sends; the server has no terminal to get them from.
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			s.terminate()
		case <-s.done:
		}
	}()
	go s.restoreHeartbeat()
	go s.acceptLoop(ln)

	// Start the program once the launching client is attached, so terminal
	// queries it sends on startup are answered by the real terminal.
	select {
	case <-s.firstAttach:
	case <-time.After(childStartTimeout):
	case <-s.done:
	}
	s.mu.Lock()
	killed, terminated := s.killed, s.terminated
	s.mu.Unlock()
	if killed || terminated {
		_ = slave.Close()
		s.shutdown()
	} else if err := cmd.Start(); err != nil {
		_ = slave.Close()
		s.handleOutput([]byte(fmt.Sprintf("di: %v\r\n", err)))
		s.mu.Lock()
		s.childExitCode = 127
		s.mu.Unlock()
		close(s.childDone)
		s.finish()
	} else {
		_ = slave.Close()
		s.mu.Lock()
		s.started = true
		s.pid = cmd.Process.Pid
		s.mu.Unlock()
		go func() {
			code := exitCodeOf(cmd.Wait())
			s.mu.Lock()
			s.childExitCode = code
			s.mu.Unlock()
			s.dropRestore()
			close(s.childDone)
		}()
		go s.readPTY()
	}

	<-s.done
	// The record goes before the socket, so no client ever sees a record
	// without a socket for a session that simply ended.
	s.dropRestore()
	_ = ln.Close()
	s.mu.Lock()
	for c := range s.clients {
		c.finish()
	}
	s.mu.Unlock()
	return nil
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exitErr.ExitCode()
	}
	return 1
}

func filterInternalEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "DI_INITIAL_COLS=") && !strings.HasPrefix(kv, "DI_INITIAL_ROWS=") && !strings.HasPrefix(kv, "DI_SESSION=") {
			out = append(out, kv)
		}
	}
	return out
}

func (s *sessionServer) shutdown() {
	s.closeOnce.Do(func() { close(s.done) })
}

// terminate stops the server on a signal, as when the system shuts down. The
// program loses its terminal with the server, and the restore record stays.
func (s *sessionServer) terminate() {
	s.mu.Lock()
	s.terminated = true
	// A shutdown signals every process at once, so the command may have
	// exited just before this signal arrived: it was interrupted all the same.
	if !s.restoreDroppedAt.IsZero() && time.Since(s.restoreDroppedAt) < restoreGrace && !s.killed {
		_ = writeFileAtomic(s.restorePath, s.restoreData)
	}
	s.mu.Unlock()
	s.shutdown()
}

// dropRestore removes the restore record once the session has ended for good.
func (s *sessionServer) dropRestore() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.restorePath == "" || s.terminated || !s.restoreDroppedAt.IsZero() {
		return
	}
	_ = os.Remove(s.restorePath)
	s.restoreDroppedAt = time.Now()
}

// restoreHeartbeat refreshes the record's mtime while the command runs, so a
// record left behind dates the interruption.
func (s *sessionServer) restoreHeartbeat() {
	if s.restorePath == "" {
		return
	}
	tick := time.NewTicker(restoreHeartbeat)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			now := time.Now()
			_ = os.Chtimes(s.restorePath, now, now)
		case <-s.childDone:
			return
		case <-s.done:
			return
		}
	}
}

func (s *sessionServer) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *sessionServer) readPTY() {
	buf := make([]byte, 64<<10)
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
			s.handleOutput(buf[:n])
		}
		if err != nil {
			break
		}
	}
	s.finish()
}

// handleOutput forwards program output untouched to every attached client and
// keeps the emulator current for snapshots, previews and status.
func (s *sessionServer) handleOutput(p []byte) {
	s.mu.Lock()
	s.vt.Write(p)
	s.lastOutput = time.Now()
	attached := len(s.clients) > 0
	if !attached && (s.vt.bells != s.bellsSeen || s.vt.notices != s.noticesSeen) {
		s.attention = true
	}
	s.bellsSeen, s.noticesSeen = s.vt.bells, s.vt.notices
	replies := s.vt.DrainReplies()
	statusReplies := s.vt.DrainStatusReplies()
	for c := range s.clients {
		c.sendOutput(p, s.vt)
	}
	s.mu.Unlock()
	// While a terminal is attached it answers queries itself; otherwise answer
	// from the emulator so the program never stalls waiting for a reply.
	if !attached && len(replies) > 0 {
		_, _ = s.master.Write(replies)
	}
	if len(statusReplies) > 0 {
		_, _ = s.master.Write(statusReplies)
	}
}

func (s *sessionServer) finish() {
	select {
	case <-s.childDone:
	case <-time.After(2 * time.Second):
	}
	s.mu.Lock()
	s.exited = true
	s.exitCode = s.childExitCode
	s.exitedAt = time.Now()
	hadClients := len(s.clients) > 0
	waits := make([]chan struct{}, 0, len(s.clients))
	for c := range s.clients {
		c.send(frameExit, mustJSON(exitInfo{Code: s.exitCode}))
		c.finish()
		waits = append(waits, c.flushed)
		s.removeClientLocked(c)
	}
	killed := s.killed
	s.mu.Unlock()

	if hadClients || killed {
		waitAll(waits, 2*time.Second)
		s.shutdown()
		return
	}
	// Nobody saw the ending: keep the final screen until someone attaches.
	go func() {
		select {
		case <-time.After(exitedLinger):
			s.shutdown()
		case <-s.done:
		}
	}()
}

func waitAll(chans []chan struct{}, timeout time.Duration) {
	deadline := time.After(timeout)
	for _, ch := range chans {
		select {
		case <-ch:
		case <-deadline:
			return
		}
	}
}

func (s *sessionServer) handleConn(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	typ, payload, err := readFrame(conn)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return
	}
	switch typ {
	case frameAttach:
		rows, cols := parseSizePayload(payload)
		s.attach(conn, rows, cols, false, nil)
	case frameResize:
		rows, cols := parseSizePayload(payload)
		s.attach(conn, rows, cols, true, nil)
	case frameInput:
		s.attach(conn, 0, 0, true, payload)
	case frameStatus:
		_ = writeFrame(conn, frameReply, mustJSON(s.status(0, 0)))
		_ = conn.Close()
	case framePreview:
		rows, cols := parseSizePayload(payload)
		_ = writeFrame(conn, framePreviewReply, mustJSON(s.status(rows, cols)))
		_ = conn.Close()
	case frameDetachAll:
		s.detachAll()
		_ = conn.Close()
	case frameKill:
		s.kill()
		_ = conn.Close()
	case frameSwitchReq:
		_ = writeFrame(conn, frameReply, mustJSON(map[string]int{"switched": s.switchClients(payload)}))
		_ = conn.Close()
	default:
		_ = conn.Close()
	}
}

func (s *sessionServer) attach(conn net.Conn, rows, cols int, legacy bool, input []byte) {
	c := newServerClient(conn, legacy)
	go c.writeLoop()

	s.mu.Lock()
	if rows > 0 && cols > 0 {
		s.resizeLocked(rows, cols)
	}
	exited := s.exited
	if !legacy {
		c.send(frameHello, mustJSON(helloInfo{Version: 2, Exited: exited, ExitCode: s.exitCode}))
	}
	c.send(frameOutput, s.vt.Snapshot())
	if exited {
		c.send(frameExit, mustJSON(exitInfo{Code: s.exitCode}))
		c.finish()
	} else {
		s.clients[c] = struct{}{}
		s.primary = c
	}
	s.lastSeen = time.Now()
	s.attention = false
	s.mu.Unlock()
	s.firstAttachOnce.Do(func() { close(s.firstAttach) })

	if exited {
		waitAll([]chan struct{}{c.flushed}, 5*time.Second)
		s.shutdown()
		return
	}
	if len(input) > 0 {
		_, _ = s.master.Write(input)
	}
	s.serveClient(c)

	s.mu.Lock()
	if _, ok := s.clients[c]; ok {
		s.removeClientLocked(c)
		s.lastSeen = time.Now()
	}
	s.mu.Unlock()
	c.close()
}

func (s *sessionServer) serveClient(c *serverClient) {
	for {
		typ, payload, err := readFrame(c.conn)
		if err != nil {
			return
		}
		switch typ {
		case frameInput:
			s.mu.Lock()
			live, primary := !s.exited, s.primary == c
			s.mu.Unlock()
			if !primary {
				// Every attached terminal answers the program's queries; only
				// the primary one's answers may reach it.
				payload = stripTerminalReplies(payload)
			}
			if live && len(payload) > 0 {
				_, _ = s.master.Write(payload)
			}
		case frameResize:
			rows, cols := parseSizePayload(payload)
			s.mu.Lock()
			s.resizeLocked(rows, cols)
			s.mu.Unlock()
		case frameDetachAll:
			s.detachAll()
			return
		case frameKill:
			s.kill()
		}
	}
}

func (s *sessionServer) resizeLocked(rows, cols int) {
	rows = clampInt(rows, 1, maxVTermRows)
	cols = clampInt(cols, 1, maxVTermCols)
	if rows == s.rows && cols == s.cols {
		return
	}
	s.rows, s.cols = rows, cols
	_ = pty.Setsize(s.master, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	s.vt.Resize(rows, cols)
}

func (s *sessionServer) detachAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		c.send(frameDetach, nil)
		c.finish()
		s.removeClientLocked(c)
	}
	s.lastSeen = time.Now()
}

func (s *sessionServer) removeClientLocked(c *serverClient) {
	delete(s.clients, c)
	if s.primary == c {
		s.primary = nil
		for other := range s.clients {
			s.primary = other
			break
		}
	}
}

// terminalReplyRe matches what terminals send back for queries: cursor and
// status reports, device attributes, mode and keyboard-flag reports, window
// size reports, color/clipboard OSC replies and DCS replies.
var terminalReplyRe = regexp.MustCompile(`\x1b\[\??[0-9]+;[0-9]+R|\x1b\[[?>=][0-9;]*c|\x1b\[\??[0-9;]*\$y|\x1b\[\?[0-9]*u|\x1b\[[0-9]n|\x1b\[[0-9]+;[0-9]+;[0-9]+t|\x1b\](?:4;[0-9]+|1[0-2]|52|7501);[^\x07\x1b]*(?:\x07|\x1b\\)|\x1bP[^\x1b]*\x1b\\`)

func stripTerminalReplies(p []byte) []byte {
	return terminalReplyRe.ReplaceAll(p, nil)
}

// switchClients hands every attached terminal over to another session. It is
// how d and di run inside a session change what the outer terminal shows
// instead of nesting a second attach.
func (s *sessionServer) switchClients(target []byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for c := range s.clients {
		if !c.legacy {
			c.send(frameSwitch, target)
			n++
		}
	}
	return n
}

func (s *sessionServer) kill() {
	s.mu.Lock()
	s.killed = true
	started, exited, pid := s.started, s.exited, s.pid
	s.mu.Unlock()
	if exited || !started || pid <= 0 {
		s.shutdown()
		return
	}
	// The program leads its own session: signal the whole process group, the
	// way closing a terminal window would, then escalate if it lingers.
	_ = syscall.Kill(-pid, syscall.SIGHUP)
	go func() {
		for _, step := range []struct {
			wait time.Duration
			sig  syscall.Signal
		}{{3 * time.Second, syscall.SIGTERM}, {3 * time.Second, syscall.SIGKILL}} {
			select {
			case <-s.childDone:
				return
			case <-time.After(step.wait):
				_ = syscall.Kill(-pid, step.sig)
			}
		}
	}()
}

func (s *sessionServer) status(previewRows, previewCols int) sessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	st := sessionStatus{
		Attached:   len(s.clients),
		Title:      s.vt.CleanTitle(),
		Notice:     s.vt.lastNotice,
		LastOutput: s.lastOutput,
		LastSeen:   s.lastSeen,
		PID:        s.pid,
		Rows:       s.rows,
		Cols:       s.cols,
		Restore:    true,
	}
	st.Activity, st.Summary = s.vt.Summary()
	if !s.attention {
		st.Notice = ""
	}
	switch {
	case s.exited:
		st.State = "exited"
		st.ExitCode = s.exitCode
		st.ExitedAt = s.exitedAt
	default:
		st.State = activityState(s.vt.agentState, s.attention, len(s.clients) > 0, s.lastOutput, s.lastSeen, now)
	}
	if previewRows > 0 && previewCols > 0 {
		st.Screen = s.vt.Preview(previewCols, previewRows)
	}
	return st
}

// activityState classifies a running session from generic signals: explicit
// OSC 7501 reports when the program sends them, otherwise output recency and
// whether anything happened since someone last looked.
func activityState(agentState string, attention, attached bool, lastOutput, lastSeen, now time.Time) string {
	unseen := !attached && lastOutput.After(lastSeen)
	switch {
	case agentStateIs(agentState, "blocked"):
		return "waiting"
	case attention && !attached:
		return "waiting"
	case agentStateIs(agentState, "working"):
		return "busy"
	case !lastOutput.IsZero() && now.Sub(lastOutput) < busyWindow && agentState == "":
		return "busy"
	case unseen:
		return "new"
	default:
		return "idle"
	}
}

func agentStateIs(body, want string) bool {
	for _, field := range strings.FieldsFunc(body, func(r rune) bool { return r == ';' || r == ':' }) {
		if field == "state="+want {
			return true
		}
	}
	return false
}

// serverClient queues output for one connection and writes it from its own
// goroutine, so a slow or stopped terminal never stalls the program.
type serverClient struct {
	conn     net.Conn
	legacy   bool
	mu       sync.Mutex
	cond     *sync.Cond
	queue    []queuedFrame
	queued   int  // output bytes waiting in queue
	closing  bool // flush the queue, then close
	closed   bool // drop everything and close now
	flushed  chan struct{}
	stopOnce sync.Once
}

type queuedFrame struct {
	typ     byte
	payload []byte
}

func newServerClient(conn net.Conn, legacy bool) *serverClient {
	c := &serverClient{conn: conn, legacy: legacy, flushed: make(chan struct{})}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// send queues a frame; the payload must not be modified afterwards.
func (c *serverClient) send(typ byte, payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.appendLocked(typ, payload)
}

func (c *serverClient) appendLocked(typ byte, payload []byte) {
	if c.closed || c.closing || (c.legacy && typ != frameOutput) {
		return
	}
	c.queue = append(c.queue, queuedFrame{typ, payload})
	if typ == frameOutput {
		c.queued += len(payload)
	}
	c.cond.Signal()
}

// sendOutput queues program output. When the terminal has fallen too far
// behind, its queued output is replaced by one snapshot of the current
// screen; control frames such as hello and switch are kept.
func (c *serverClient) sendOutput(p []byte, vt *vterm) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.queued+len(p) > maxClientBacklog {
		kept := c.queue[:0]
		for _, f := range c.queue {
			if f.typ != frameOutput {
				kept = append(kept, f)
			}
		}
		c.queue, c.queued = kept, 0
		c.appendLocked(frameOutput, vt.Snapshot())
		return
	}
	c.appendLocked(frameOutput, append([]byte(nil), p...))
}

func (c *serverClient) finish() {
	c.mu.Lock()
	c.closing = true
	c.cond.Signal()
	c.mu.Unlock()
}

func (c *serverClient) close() {
	c.mu.Lock()
	c.closed = true
	c.cond.Signal()
	c.mu.Unlock()
	_ = c.conn.Close()
}

func (c *serverClient) writeLoop() {
	defer c.stopOnce.Do(func() {
		_ = c.conn.Close()
		close(c.flushed)
	})
	var buf []byte
	for {
		c.mu.Lock()
		for len(c.queue) == 0 && !c.closing && !c.closed {
			c.cond.Wait()
		}
		if c.closed || len(c.queue) == 0 {
			c.mu.Unlock()
			return
		}
		buf = buf[:0]
		for _, f := range c.queue {
			if c.legacy {
				buf = append(buf, f.payload...)
			} else {
				buf = appendFrame(buf, f.typ, f.payload)
			}
		}
		c.queue, c.queued = c.queue[:0], 0
		c.mu.Unlock()
		if _, err := c.conn.Write(buf); err != nil {
			c.mu.Lock()
			c.closed = true
			c.mu.Unlock()
			return
		}
		if cap(buf) > 4<<20 {
			buf = nil // do not pin a large snapshot buffer
		}
	}
}

func appendFrame(dst []byte, typ byte, payload []byte) []byte {
	var header [5]byte
	header[0] = typ
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	dst = append(dst, header[:]...)
	return append(dst, payload...)
}

var frameWriteMu sync.Mutex

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	buf := appendFrame(make([]byte, 0, 5+len(payload)), typ, payload)
	frameWriteMu.Lock()
	defer frameWriteMu.Unlock()
	_, err := w.Write(buf)
	return err
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n > maxFrameLen {
		return 0, nil, errors.New("frame too large")
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func sizePayload(rows, cols int) []byte {
	payload := make([]byte, 8)
	binary.BigEndian.PutUint32(payload[:4], uint32(max(rows, 0)))
	binary.BigEndian.PutUint32(payload[4:], uint32(max(cols, 0)))
	return payload
}

func parseSizePayload(payload []byte) (rows, cols int) {
	if len(payload) < 8 {
		return 0, 0
	}
	return int(binary.BigEndian.Uint32(payload[:4])), int(binary.BigEndian.Uint32(payload[4:8]))
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
