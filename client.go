package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	defaultDetachKey = "^]"
	defaultSwitchKey = `^\`
)

type attachEnd int

const (
	endDetach   attachEnd = iota // the user pressed the detach key
	endSwitch                    // the user pressed the switch key
	endExit                      // the program exited
	endRemote                    // another client detached this one
	endLost                      // the connection dropped without a reason
	endSwitchTo                  // d or di run inside the session asked to show another one
)

func attachSession(sock string) error {
	current := sock
	for {
		if self := os.Getenv("DI_SESSION"); self != "" && sameSession(self, current) {
			return errors.New("di: refusing to attach a session to itself")
		}
		maybeExecUpdatedBinary(current)
		end, code, wasExited, target, err := attachOnce(current)
		if err != nil {
			return err
		}
		meta := readSessionMeta(current)
		label := formatDisplayCommand(meta.Command, cleanSessionName(meta.Name, current))
		switch end {
		case endSwitchTo:
			current = target
		case endSwitch:
			selected, err := pickSession(current, "")
			if err != nil {
				return err
			}
			if selected != "" {
				current = selected
			} else if !sessionReachable(current) {
				return nil
			}
		case endDetach:
			fmt.Printf("\x1b[2m[di] detached: %s\x1b[0m\r\n", label)
			return nil
		case endRemote:
			fmt.Printf("\x1b[2m[di] detached from %s by another client\x1b[0m\r\n", label)
			return nil
		case endLost:
			fmt.Printf("\x1b[2m[di] lost connection to %s; reattach with: di\x1b[0m\r\n", label)
			return nil
		case endExit:
			switch {
			case wasExited:
				fmt.Printf("\r\n\x1b[2m[di] %s had already ended (status %d); session closed\x1b[0m\r\n", label, code)
			case code != 0:
				fmt.Printf("\r\n\x1b[2m[di] %s exited with status %d\x1b[0m\r\n", label, code)
			}
			return nil
		}
	}
}

func sameSession(a, b string) bool {
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(ia, ib)
	}
	return filepath.Clean(a) == filepath.Clean(b)
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

// attachOnce connects to a session and passes bytes straight through until
// the user detaches or switches, or the session ends.
func attachOnce(sock string) (end attachEnd, code int, wasExited bool, target string, err error) {
	conn, err := dialSession(sock)
	if err != nil {
		if !isSocket(sock) || errors.Is(err, syscall.ECONNREFUSED) {
			removeSessionFiles(sock)
		}
		return 0, 0, false, "", fmt.Errorf("di: session is gone: %s", cleanSessionName("", sock))
	}
	defer conn.Close()

	cols, rows := terminalSize()
	if err := writeFrame(conn, frameAttach, sizePayload(rows, cols)); err != nil {
		return 0, 0, false, "", err
	}
	oldState, err := term.MakeRaw(0)
	if err != nil {
		return 0, 0, false, "", err
	}

	stream, err := openAttachStream(conn, rows, cols)
	if err != nil {
		_ = term.Restore(0, oldState)
		return 0, 0, false, "", err
	}

	out := &hostWriter{tracker: newVTerm(rows, cols), w: os.Stdout}
	out.tracker.maxScrollback = 0
	out.write([]byte("\x1b[22;0t")) // keep the shell's window title to restore later

	type outputResult struct {
		end    attachEnd
		code   int
		target string
	}
	outCh := make(chan outputResult, 1)
	go func() {
		end, code, target := stream.copyTo(out)
		outCh <- outputResult{end, code, target}
	}()

	stopWinch := make(chan struct{})
	var winchWG sync.WaitGroup
	winchWG.Add(1)
	go func() {
		defer winchWG.Done()
		watchWindowSize(conn, out, stopWinch)
	}()

	stopInput := make(chan struct{})
	inCh := make(chan attachEnd, 1)
	go func() { inCh <- forwardInput(conn, stopInput) }()

	// Leave the terminal usable if the client itself is told to stop.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	defer signal.Stop(sigCh)

	inputDone := false
	select {
	case res := <-outCh:
		end, code, target = res.end, res.code, res.target
	case end = <-inCh:
		inputDone = true
		_ = conn.Close()
		<-outCh
	case <-sigCh:
		end = endDetach
		_ = conn.Close()
		<-outCh
	}
	close(stopInput)
	if !inputDone {
		<-inCh
	}
	if end == endLost && stream.legacy && legacySessionEnded(sock) {
		// Older servers just hang up when the program exits.
		end = endExit
	}
	close(stopWinch)
	winchWG.Wait()

	out.mu.Lock()
	restore := hostRestoreSequence(out.tracker) + "\x1b[23;0t"
	if end != endExit {
		// Keep the session's last screen in scrollback rather than erasing it.
		restore += scrollIntoHistory(out.tracker.primary.rows, out.tracker.usedRows())
	}
	out.mu.Unlock()
	_, _ = os.Stdout.WriteString(restore)
	_ = term.Restore(0, oldState)
	return end, code, stream.hello.Exited, target, nil
}

// hostWriter writes session output to the terminal and mirrors it into a
// mode tracker so detach can undo exactly the modes the session turned on.
type hostWriter struct {
	mu      sync.Mutex
	tracker *vterm
	w       io.Writer // the terminal; os.Stdout
}

func (h *hostWriter) write(p []byte) error {
	h.mu.Lock()
	h.tracker.Write(p)
	h.tracker.DrainReplies()
	h.tracker.DrainStatusReplies()
	h.mu.Unlock()
	_, err := h.w.Write(p)
	return err
}

func (h *hostWriter) resize(rows, cols int) {
	h.mu.Lock()
	h.tracker.Resize(rows, cols)
	h.mu.Unlock()
}

// attachStream reads what the server sends after an attach request. Current
// servers send frames; servers started by older di builds send raw bytes.
type attachStream struct {
	conn    net.Conn
	legacy  bool
	pending []byte
	hello   helloInfo
}

func openAttachStream(conn net.Conn, rows, cols int) (*attachStream, error) {
	s := &attachStream{conn: conn}
	head, err := readHead(conn, 5, 2*time.Second)
	if len(head) == 5 && head[0] == frameHello {
		n := int(uint32(head[1])<<24 | uint32(head[2])<<16 | uint32(head[3])<<8 | uint32(head[4]))
		if n < 64<<10 {
			payload := make([]byte, n)
			if _, err := io.ReadFull(conn, payload); err != nil {
				return nil, err
			}
			if json.Unmarshal(payload, &s.hello) == nil && s.hello.Version > 0 {
				return s, nil
			}
		}
	}
	if err != nil && len(head) == 0 && !errors.Is(err, os.ErrDeadlineExceeded) {
		return nil, fmt.Errorf("di: session closed: %w", err)
	}
	// Older server: it ignores the attach frame and replays its snapshot right
	// away. Only send a resize when the size differs, because those servers
	// force a full redraw when a client re-sends the current size.
	s.legacy = true
	s.pending = head
	more, _ := readHead(conn, 4096, 20*time.Millisecond)
	s.pending = append(s.pending, more...)
	if r, c, ok := legacySnapshotSize(s.pending); !ok || r != rows || c != cols {
		_ = writeFrame(conn, frameResize, sizePayload(rows, cols))
	}
	return s, nil
}

func readHead(conn net.Conn, n int, timeout time.Duration) ([]byte, error) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer conn.SetReadDeadline(time.Time{})
	buf := make([]byte, n)
	got := 0
	for got < n {
		m, err := conn.Read(buf[got:])
		got += m
		if err != nil {
			return buf[:got], err
		}
		if n > 5 {
			break
		}
	}
	return buf[:got], nil
}

var legacySizeRe = regexp.MustCompile(`\x1b\]7502;vterm=1;snap=begin;rows=(\d+);cols=(\d+)`)

func legacySnapshotSize(b []byte) (int, int, bool) {
	m := legacySizeRe.FindSubmatch(b)
	if m == nil {
		return 0, 0, false
	}
	r, _ := strconv.Atoi(string(m[1]))
	c, _ := strconv.Atoi(string(m[2]))
	return r, c, true
}

func (s *attachStream) copyTo(out *hostWriter) (end attachEnd, code int, target string) {
	if s.legacy {
		// Older servers drop a client whose socket stays full for 100ms, which
		// a slow (remote, web) terminal causes during big redraws such as an
		// agent reprinting a resumed conversation. Keep draining the socket
		// into memory and write to the terminal at its own pace.
		q := newByteQueue()
		go func() {
			buf := make([]byte, 64<<10)
			for {
				n, err := s.conn.Read(buf)
				q.push(buf[:n])
				if err != nil {
					q.close()
					return
				}
			}
		}()
		data, open := s.pending, true
		for {
			if len(data) > 0 && out.write(data) != nil {
				return endLost, 0, ""
			}
			if !open {
				return endLost, 0, ""
			}
			data, open = q.pop()
		}
	}
	for {
		typ, payload, err := readFrame(s.conn)
		if err != nil {
			return endLost, 0, ""
		}
		switch typ {
		case frameOutput:
			if out.write(payload) != nil {
				return endLost, 0, ""
			}
		case frameExit:
			var info exitInfo
			_ = json.Unmarshal(payload, &info)
			return endExit, info.Code, ""
		case frameDetach:
			return endRemote, 0, ""
		case frameSwitch:
			if target := string(payload); isSocket(target) {
				return endSwitchTo, 0, target
			}
		}
	}
}

// legacySessionEnded reports whether a session run by an older server is gone:
// those servers remove their socket once the program has exited.
func legacySessionEnded(sock string) bool {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !isSocket(sock) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// byteQueue is an unbounded FIFO of bytes between one producer and one consumer.
type byteQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	data   []byte
	closed bool
}

func newByteQueue() *byteQueue {
	q := &byteQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *byteQueue) push(p []byte) {
	q.mu.Lock()
	q.data = append(q.data, p...)
	q.mu.Unlock()
	q.cond.Signal()
}

func (q *byteQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Signal()
}

// pop waits for data and returns everything queued; open is false once the
// queue is closed and drained.
func (q *byteQueue) pop() (data []byte, open bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.data) == 0 && !q.closed {
		q.cond.Wait()
	}
	data, q.data = q.data, nil
	return data, len(data) > 0 || !q.closed
}

func watchWindowSize(conn net.Conn, out *hostWriter, stop <-chan struct{}) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	for {
		select {
		case <-stop:
			return
		case <-ch:
			cols, rows := terminalSize()
			if cols > 0 && rows > 0 {
				out.resize(rows, cols)
				_ = writeFrame(conn, frameResize, sizePayload(rows, cols))
			}
		}
	}
}

// stdinReader is the only reader of the terminal. It reads only when asked,
// so no read is ever left pending while fzf owns the terminal, and a read
// started for one attachment carries over to the next instead of racing it.
var stdinReader = struct {
	once  sync.Once
	req   chan struct{}
	res   chan stdinChunk
	armed bool // a read was requested and its result not yet taken
}{req: make(chan struct{}, 1), res: make(chan stdinChunk, 1)}

type stdinChunk struct {
	data []byte
	err  error
}

func readStdin(stop <-chan struct{}) (stdinChunk, bool) {
	stdinReader.once.Do(func() {
		go func() {
			buf := make([]byte, 4096)
			for range stdinReader.req {
				n, err := os.Stdin.Read(buf)
				stdinReader.res <- stdinChunk{append([]byte(nil), buf[:n]...), err}
			}
		}()
	})
	if !stdinReader.armed {
		stdinReader.req <- struct{}{}
		stdinReader.armed = true
	}
	select {
	case chunk := <-stdinReader.res:
		stdinReader.armed = false
		return chunk, true
	case <-stop:
		return stdinChunk{}, false
	}
}

// forwardInput copies keyboard input to the session until the detach or
// switch key is pressed, input fails, or stop is closed.
func forwardInput(conn net.Conn, stop <-chan struct{}) attachEnd {
	keys := inputKeys{detach: detachKeyByte(), switchKey: switchKeyByte()}
	for {
		chunk, ok := readStdin(stop)
		if !ok {
			return endLost
		}
		if len(chunk.data) > 0 {
			fwd, action, hit := keys.scan(chunk.data)
			if len(fwd) > 0 {
				if werr := writeFrame(conn, frameInput, fwd); werr != nil {
					return endLost
				}
			}
			if hit {
				return action
			}
		}
		if chunk.err != nil {
			return endLost
		}
	}
}

type inputKeys struct {
	detach    byte
	switchKey byte
	inPaste   bool
	carry     []byte // already forwarded tail of the last chunk that may start a marker or key
}

var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

// scan returns the bytes to forward and whether a di key was pressed. Keys are
// recognized as raw control bytes and in kitty keyboard / modifyOtherKeys
// encodings, anywhere in the chunk, but never inside a bracketed paste.
// Escape sequences split across reads are matched using the carried tail.
func (k *inputKeys) scan(chunk []byte) (fwd []byte, action attachEnd, hit bool) {
	off := len(k.carry)
	data := append(k.carry, chunk...)
	k.carry = nil
	// fwdTo forwards this chunk up to data offset i.
	fwdTo := func(i int) []byte { return chunk[:max(i-off, 0)] }
	for i := 0; i < len(data); {
		if k.inPaste {
			end := bytes.Index(data[i:], pasteEnd)
			if end < 0 {
				k.carry = markerPrefixSuffix(data[i:], pasteEnd)
				return chunk, 0, false
			}
			k.inPaste = false
			i += end + len(pasteEnd)
			continue
		}
		b := data[i]
		if i >= off && b == k.detach {
			return fwdTo(i), endDetach, true
		}
		if i >= off && b == k.switchKey {
			return fwdTo(i), endSwitch, true
		}
		if b == 0x1b {
			rest := data[i:]
			if bytes.HasPrefix(rest, pasteStart) {
				k.inPaste = true
				i += len(pasteStart)
				continue
			}
			end, key, ok := 0, byte(0), false
			if len(rest) > 1 && rest[1] == '[' {
				end, key, ok = parseModifiedKey(rest)
			}
			if ok && (key == k.detach || key == k.switchKey) {
				if key == k.detach {
					return fwdTo(i), endDetach, true
				}
				return fwdTo(i), endSwitch, true
			}
			if end > 0 {
				i += end
				continue
			}
			if len(rest) <= 32 && (len(rest) == 1 || rest[1] == '[') {
				// An escape sequence cut off by the read; finish matching it next time.
				k.carry = append([]byte(nil), rest...)
				return chunk, 0, false
			}
		}
		i++
	}
	return chunk, 0, false
}

// markerPrefixSuffix returns the longest suffix of data that is a proper
// prefix of marker.
func markerPrefixSuffix(data, marker []byte) []byte {
	for n := min(len(marker)-1, len(data)); n > 0; n-- {
		if bytes.HasSuffix(data, marker[:n]) {
			return append([]byte(nil), data[len(data)-n:]...)
		}
	}
	return nil
}

// parseModifiedKey decodes a CSI-u (kitty) or CSI 27;mod;code~ (xterm
// modifyOtherKeys) key press with Ctrl held, returning the equivalent control
// byte. end is the length of the CSI sequence, or 0 if it is incomplete.
func parseModifiedKey(seq []byte) (end int, ctrl byte, ok bool) {
	j := 2
	for j < len(seq) && seq[j] >= 0x30 && seq[j] <= 0x3f {
		j++
	}
	for j < len(seq) && seq[j] >= 0x20 && seq[j] <= 0x2f {
		j++
	}
	if j >= len(seq) || seq[j] < 0x40 || seq[j] > 0x7e {
		return 0, 0, false
	}
	params := strings.Split(string(seq[2:j]), ";")
	var code, mods, event int
	switch seq[j] {
	case 'u':
		if len(params) < 2 {
			return j + 1, 0, false
		}
		code = atoiPrefix(params[0])
		modField := strings.SplitN(params[1], ":", 2)
		mods = atoiPrefix(modField[0])
		if len(modField) == 2 {
			event = atoiPrefix(modField[1])
		}
	case '~':
		if len(params) != 3 || params[0] != "27" {
			return j + 1, 0, false
		}
		mods, code = atoiPrefix(params[1]), atoiPrefix(params[2])
	default:
		return j + 1, 0, false
	}
	// Ctrl without Shift/Alt/Super; lock bits are ignored. Releases (event 3) are not presses.
	m := mods - 1
	if mods < 2 || m&4 == 0 || m&(1|2|8|16|32) != 0 || event == 3 {
		return j + 1, 0, false
	}
	b, valid := keyCodeToCtrl(code)
	return j + 1, b, valid
}

func atoiPrefix(s string) int {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	n, _ := strconv.Atoi(s)
	return n
}

func keyCodeToCtrl(code int) (byte, bool) {
	switch {
	case code >= 'a' && code <= 'z':
		return byte(code-'a') + 1, true
	case code >= '@' && code <= '_':
		return byte(code) & 0x1f, true
	case code == '?':
		return 0x7f, true
	}
	return 0, false
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

func terminalSize() (int, int) {
	width, height, err := term.GetSize(0)
	if err != nil {
		return 0, 0
	}
	return width, height
}

func dialSession(sock string) (net.Conn, error) {
	return net.DialTimeout("unix", sock, dialTimeout)
}

func sessionReachable(sock string) bool {
	conn, err := dialSession(sock)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// requestSwitch asks the terminals attached to session to show target
// instead. It reports false when no current client is attached there.
func requestSwitch(session, target string) bool {
	conn, err := dialSession(session)
	if err != nil {
		return false
	}
	defer conn.Close()
	if writeFrame(conn, frameSwitchReq, []byte(target)) != nil {
		return false
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	typ, payload, err := readFrame(conn)
	var reply struct{ Switched int }
	return err == nil && typ == frameReply && json.Unmarshal(payload, &reply) == nil && reply.Switched > 0
}

// insideSession returns the session this process runs in, if it is alive.
func insideSession() string {
	if self := os.Getenv("DI_SESSION"); self != "" && sessionReachable(self) {
		return self
	}
	return ""
}

// detachSession asks a session to detach all of its clients.
func detachSession(sock string) error {
	conn, err := dialSession(sock)
	if err != nil {
		removeSessionFiles(sock)
		return err
	}
	defer conn.Close()
	return writeFrame(conn, frameDetachAll, nil)
}
