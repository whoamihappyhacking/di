package main

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// vterm is a faithful terminal emulator used by the session server. It never
// renders while a client is attached (output is passed through untouched); it
// only exists so a newly attached client, a preview, or a status query can see
// exactly what a real terminal would be showing right now.

const (
	defaultVTermCols       = 120
	defaultVTermRows       = 36
	defaultVTermScrollback = 10000
	maxVTermRows           = 500
	maxVTermCols           = 1000
	maxCSIParamValue       = 65535
	maxCSIParamsLen        = 1024
	maxIntermLen           = 16
	maxOSCBufLen           = 256 << 10
	maxCombiningBytes      = 32
	maxScrollbackLineBytes = 64 << 10
	maxScrollbackBytes     = 8 << 20 // keeps attach snapshots small enough to replay quickly
	maxPendingSequence     = 1 << 20
)

type vtermColorType uint8

const (
	vtermColorDefault vtermColorType = iota
	vtermColorIndexed
	vtermColorRGB
)

type vtermColor struct {
	kind    vtermColorType
	index   uint8
	r, g, b uint8
}

const (
	vtermAttrBold uint16 = 1 << iota
	vtermAttrDim
	vtermAttrItalic
	vtermAttrUnderline
	vtermAttrBlink
	vtermAttrReverse
	vtermAttrInvisible
	vtermAttrStrikethrough
	vtermAttrDoubleUnderline
	vtermAttrCurlyUnderline
	vtermAttrDottedUnderline
	vtermAttrDashedUnderline
	vtermAttrOverline
)

const vtermUnderlineMask = vtermAttrUnderline | vtermAttrDoubleUnderline | vtermAttrCurlyUnderline | vtermAttrDottedUnderline | vtermAttrDashedUnderline

type vtermStyle struct {
	fg        vtermColor
	bg        vtermColor
	ul        vtermColor
	flags     uint16
	hyperlink string
}

type vtermCell struct {
	ch        rune
	combining string
	width     uint8 // 1 = normal/blank, 2 = wide leader, 0 = wide continuation
	style     vtermStyle
}

func blankVTermCell(style vtermStyle) vtermCell {
	// Erase operations use the background color of the current style only.
	var eraseStyle vtermStyle
	eraseStyle.bg = style.bg
	return vtermCell{ch: ' ', width: 1, style: eraseStyle}
}

func (c vtermCell) isDefaultBlank() bool {
	return c.width == 1 && (c.ch == ' ' || c.ch == 0) && c.combining == "" && c.style == vtermStyle{}
}

type vtermRow struct {
	cells   []vtermCell
	wrapped bool // the logical line continues on the next row (soft wrap)
}

func newVTermRow(cols int, style vtermStyle) vtermRow {
	if cols < 1 {
		cols = 1
	}
	cells := make([]vtermCell, cols)
	blank := blankVTermCell(style)
	for i := range cells {
		cells[i] = blank
	}
	return vtermRow{cells: cells}
}

func (r vtermRow) plainText() string {
	return cellsPlainText(r.cells)
}

func cellsPlainText(cells []vtermCell) string {
	var b strings.Builder
	for _, c := range cells {
		if c.width == 0 {
			continue
		}
		if c.ch == 0 {
			b.WriteByte(' ')
		} else {
			b.WriteRune(c.ch)
		}
		b.WriteString(c.combining)
	}
	return strings.TrimRight(b.String(), " ")
}

// sbLine is one logical scrollback line, stored as ANSI so long histories stay
// small and can be replayed (and re-wrapped by the host terminal) verbatim.
type sbLine struct {
	ansi  string
	width int  // display columns
	open  bool // the last pushed row soft-wrapped into the following row
}

type vtermScreen struct {
	rows         int
	cols         int
	grid         []vtermRow
	tabs         []bool
	curR         int
	curC         int
	pendingWrap  bool
	scrollTop    int
	scrollBottom int
	originMode   bool
	autoWrap     bool
	cursorVis    bool
	cursorShape  uint8 // DECSCUSR: 0..6

	savedCurR        int
	savedCurC        int
	savedStyle       vtermStyle
	savedOriginMode  bool
	savedPendingWrap bool

	kittyStack [8]uint8
	kittyLen   int
}

func newDefaultTabs(cols int) []bool {
	if cols < 1 {
		cols = 1
	}
	tabs := make([]bool, cols)
	for c := 8; c < cols; c += 8 {
		tabs[c] = true
	}
	return tabs
}

func newVTermScreen(rows, cols int) vtermScreen {
	rows = clampVTermRows(rows)
	cols = clampVTermCols(cols)
	grid := make([]vtermRow, rows)
	for i := range grid {
		grid[i] = newVTermRow(cols, vtermStyle{})
	}
	return vtermScreen{
		rows:         rows,
		cols:         cols,
		grid:         grid,
		tabs:         newDefaultTabs(cols),
		scrollBottom: rows - 1,
		autoWrap:     true,
		cursorVis:    true,
	}
}

func clampVTermRows(rows int) int {
	if rows < 1 {
		return defaultVTermRows
	}
	return min(rows, maxVTermRows)
}

func clampVTermCols(cols int) int {
	if cols < 1 {
		return defaultVTermCols
	}
	return min(cols, maxVTermCols)
}

func (s *vtermScreen) currentKittyFlags() uint8 {
	if s.kittyLen <= 0 {
		return 0
	}
	return s.kittyStack[s.kittyLen-1]
}

func (s *vtermScreen) pushKittyFlags(flags uint8) {
	if s.kittyLen < len(s.kittyStack) {
		s.kittyStack[s.kittyLen] = flags
		s.kittyLen++
	} else {
		copy(s.kittyStack[:], s.kittyStack[1:])
		s.kittyStack[len(s.kittyStack)-1] = flags
	}
}

func (s *vtermScreen) popKittyFlags(n int) {
	if n <= 0 {
		n = 1
	}
	s.kittyLen = max(s.kittyLen-n, 0)
}

func (s *vtermScreen) setKittyFlags(flags uint8, mode int) {
	if s.kittyLen == 0 {
		s.kittyLen = 1
		s.kittyStack[0] = 0
	}
	cur := s.kittyStack[s.kittyLen-1]
	switch mode {
	case 2:
		cur |= flags
	case 3:
		cur &^= flags
	default:
		cur = flags
	}
	s.kittyStack[s.kittyLen-1] = cur
}

type vtermParserState uint8

const (
	vtStateGround vtermParserState = iota
	vtStateEsc
	vtStateEscIntermediate
	vtStateCSI
	vtStateOSC
	vtStateDCS
)

type vterm struct {
	primary         vtermScreen
	alt             vtermScreen
	inAltScreen     bool
	style           vtermStyle
	scrollback      []sbLine
	scrollbackBytes int
	maxScrollback   int

	// Terminal modes.
	appCursorKeys   bool // ?1
	appKeypad       bool // DECKPAM / DECKPNM
	insertMode      bool // IRM
	newlineMode     bool // LNM
	bracketedPaste  bool // ?2004
	focusEvents     bool // ?1004
	mouseMode       int  // 0, 1000, 1002, 1003
	mouseSGR        bool // ?1006
	syncOutput      bool // ?2026
	modifyOtherKeys int

	g0Charset     byte
	g1Charset     byte
	activeCharset int

	// Metadata.
	title       string
	cwd         string
	agentState  string // body of the last OSC 7501 state report
	progressSeq string // OSC 9;4 progress
	bells       int
	notices     int
	lastNotice  string

	lastPrintedCh        rune
	lastPrintedCombining string
	pendingReplies       bytes.Buffer
	statusReplies        bytes.Buffer // replies only di can give (OSC 7501 capability)

	// Legacy di snapshot stream markers (OSC 7502), used to read old servers.
	sawSnapshotEnd bool

	// Parser state.
	state       vtermParserState
	escInterm   []byte
	csiPrefix   byte
	csiParams   bytes.Buffer
	csiInterm   []byte
	csiCorrupt  bool
	oscBuf      bytes.Buffer
	oscOverflow bool
	oscEscSeen  bool
	utf8Buf     [4]byte
	utf8Need    int
	utf8Len     int

	pendingSeq      []byte // raw bytes of an unfinished sequence
	pendingOverflow bool
}

func newVTerm(rows, cols int) *vterm {
	rows = clampVTermRows(rows)
	cols = clampVTermCols(cols)
	return &vterm{
		primary:       newVTermScreen(rows, cols),
		alt:           newVTermScreen(rows, cols),
		maxScrollback: defaultVTermScrollback,
		g0Charset:     'B',
		g1Charset:     'B',
	}
}

func (vt *vterm) activeScreen() *vtermScreen {
	if vt.inAltScreen {
		return &vt.alt
	}
	return &vt.primary
}

func (vt *vterm) Size() (rows, cols int) {
	scr := vt.activeScreen()
	return scr.rows, scr.cols
}

func (vt *vterm) DrainReplies() []byte {
	if vt.pendingReplies.Len() == 0 {
		return nil
	}
	out := append([]byte(nil), vt.pendingReplies.Bytes()...)
	vt.pendingReplies.Reset()
	return out
}

func (vt *vterm) DrainStatusReplies() []byte {
	if vt.statusReplies.Len() == 0 {
		return nil
	}
	out := append([]byte(nil), vt.statusReplies.Bytes()...)
	vt.statusReplies.Reset()
	return out
}

func (vt *vterm) pushScrollback(row vtermRow) {
	if vt.maxScrollback <= 0 {
		return
	}
	cells := row.cells
	if !row.wrapped {
		cells = trimBlankTail(cells, 0)
	}
	ansi := encodeCellsANSI(cells)
	vt.scrollbackBytes += len(ansi)
	if n := len(vt.scrollback); n > 0 && vt.scrollback[n-1].open && len(vt.scrollback[n-1].ansi) < maxScrollbackLineBytes {
		last := &vt.scrollback[n-1]
		last.ansi += ansi
		last.width += len(cells)
		last.open = row.wrapped
	} else {
		vt.scrollback = append(vt.scrollback, sbLine{ansi: ansi, width: len(cells), open: row.wrapped})
	}
	if len(vt.scrollback) >= vt.maxScrollback+256 || vt.scrollbackBytes > maxScrollbackBytes+maxScrollbackBytes/8 {
		drop := max(len(vt.scrollback)-vt.maxScrollback, 0)
		for _, line := range vt.scrollback[:drop] {
			vt.scrollbackBytes -= len(line.ansi)
		}
		for drop < len(vt.scrollback)-1 && vt.scrollbackBytes > maxScrollbackBytes {
			vt.scrollbackBytes -= len(vt.scrollback[drop].ansi)
			drop++
		}
		vt.scrollback = append([]sbLine(nil), vt.scrollback[drop:]...)
	}
}

func (vt *vterm) closeScrollbackLine() {
	if n := len(vt.scrollback); n > 0 {
		vt.scrollback[n-1].open = false
	}
}

// Write feeds raw PTY output bytes into the terminal state machine.
func (vt *vterm) Write(data []byte) {
	for _, b := range data {
		prev := vt.state
		vt.step(b)
		vt.trackPendingSequence(prev, b)
	}
}

// trackPendingSequence keeps the raw bytes of a sequence that has started but
// not finished. PTY reads split output anywhere, so a snapshot can fall in
// the middle of an escape sequence or a UTF-8 character; replaying these
// bytes after the snapshot puts the real terminal's parser in the same spot,
// and the rest of the sequence arriving through passthrough completes it.
func (vt *vterm) trackPendingSequence(prev vtermParserState, b byte) {
	if vt.state == vtStateGround && vt.utf8Need == 0 {
		vt.pendingSeq = vt.pendingSeq[:0]
		vt.pendingOverflow = false
		return
	}
	inString := prev == vtStateOSC || prev == vtStateDCS
	switch {
	case b == 0x1b && !inString:
		vt.pendingSeq = append(vt.pendingSeq[:0], b)
		vt.pendingOverflow = false
	case b < 0x20 && !inString:
		// A control executed in the middle of a sequence is already applied.
	case len(vt.pendingSeq) < maxPendingSequence:
		vt.pendingSeq = append(vt.pendingSeq, b)
	default:
		vt.pendingOverflow = true
	}
}

// PendingSequence returns the bytes to replay after a snapshot. An oversized
// string sequence (say, an inline image) is reduced to its introducer, so the
// rest of it is swallowed by the terminal instead of printed.
func (vt *vterm) PendingSequence() []byte {
	if vt.pendingOverflow && len(vt.pendingSeq) >= 2 {
		return append([]byte(nil), vt.pendingSeq[:2]...)
	}
	return append([]byte(nil), vt.pendingSeq...)
}

func (vt *vterm) step(b byte) {
	if vt.utf8Need > 0 {
		if (b & 0xC0) == 0x80 {
			vt.utf8Buf[vt.utf8Len] = b
			vt.utf8Len++
			if vt.utf8Len == vt.utf8Need {
				r, size := utf8.DecodeRune(vt.utf8Buf[:vt.utf8Len])
				vt.utf8Need = 0
				vt.utf8Len = 0
				if r == utf8.RuneError && size <= 1 {
					vt.putRune(utf8.RuneError)
				} else {
					vt.putRune(r)
				}
			}
			return
		}
		// Invalid continuation byte: emit a replacement character and re-process b.
		vt.utf8Need = 0
		vt.utf8Len = 0
		vt.putRune(utf8.RuneError)
	}

	// CAN and SUB cancel any active escape or control sequence.
	if b == 0x18 || b == 0x1a {
		vt.state = vtStateGround
		vt.oscEscSeen = false
		vt.csiCorrupt = false
		vt.oscOverflow = false
		return
	}

	switch vt.state {
	case vtStateOSC:
		vt.stepOSC(b)
		return
	case vtStateDCS:
		vt.stepDCS(b)
		return
	}

	// C0 controls can appear inside ESC/CSI sequences.
	if b < 0x20 || b == 0x7f {
		if b == 0x1b {
			vt.state = vtStateEsc
			vt.escInterm = vt.escInterm[:0]
			return
		}
		vt.handleControl(b)
		return
	}

	switch vt.state {
	case vtStateGround:
		switch {
		case b < 0x80:
			vt.putRune(rune(b))
		case b >= 0xC2 && b <= 0xDF:
			vt.utf8Buf[0], vt.utf8Len, vt.utf8Need = b, 1, 2
		case b >= 0xE0 && b <= 0xEF:
			vt.utf8Buf[0], vt.utf8Len, vt.utf8Need = b, 1, 3
		case b >= 0xF0 && b <= 0xF4:
			vt.utf8Buf[0], vt.utf8Len, vt.utf8Need = b, 1, 4
		default:
			vt.putRune(utf8.RuneError)
		}
	case vtStateEsc:
		vt.stepEsc(b)
	case vtStateEscIntermediate:
		vt.stepEscIntermediate(b)
	case vtStateCSI:
		vt.stepCSI(b)
	}
}

func (vt *vterm) handleControl(b byte) {
	scr := vt.activeScreen()
	switch b {
	case 0x07: // BEL
		vt.bells++
	case 0x08: // BS
		scr.pendingWrap = false
		if scr.curC > 0 {
			scr.curC--
		}
	case 0x09: // HT
		if scr.pendingWrap && scr.curC == scr.cols-1 {
			return
		}
		scr.pendingWrap = false
		vt.advanceTabForward(scr, 1)
	case 0x0a, 0x0b, 0x0c: // LF, VT, FF
		scr.pendingWrap = false
		if vt.newlineMode {
			scr.curC = 0
		}
		vt.lineFeed()
	case 0x0d: // CR
		scr.pendingWrap = false
		scr.curC = 0
	case 0x0e: // SO
		vt.activeCharset = 1
	case 0x0f: // SI
		vt.activeCharset = 0
	}
}

func (vt *vterm) advanceTabForward(scr *vtermScreen, n int) {
	for step := 0; step < max(n, 1); step++ {
		found := scr.cols - 1
		for c := scr.curC + 1; c < scr.cols; c++ {
			if c < len(scr.tabs) && scr.tabs[c] {
				found = c
				break
			}
		}
		scr.curC = found
	}
}

func (vt *vterm) advanceTabBackward(scr *vtermScreen, n int) {
	for step := 0; step < max(n, 1); step++ {
		found := 0
		for c := scr.curC - 1; c >= 0; c-- {
			if c < len(scr.tabs) && scr.tabs[c] {
				found = c
				break
			}
		}
		scr.curC = found
	}
}

func (vt *vterm) lineFeed() {
	scr := vt.activeScreen()
	if scr.curR == scr.scrollBottom {
		vt.scrollUpRegion(scr, scr.scrollTop, scr.scrollBottom, 1, !vt.inAltScreen && scr.scrollTop == 0)
	} else if scr.curR < scr.rows-1 {
		scr.curR++
	}
}

func (vt *vterm) reverseIndex() {
	scr := vt.activeScreen()
	scr.pendingWrap = false
	if scr.curR == scr.scrollTop {
		vt.scrollDownRegion(scr, scr.scrollTop, scr.scrollBottom, 1)
	} else if scr.curR > 0 {
		scr.curR--
	}
}

func (vt *vterm) scrollUpRegion(scr *vtermScreen, top, bottom, n int, recordScrollback bool) {
	top = max(top, 0)
	bottom = min(bottom, scr.rows-1)
	if top > bottom || n <= 0 {
		return
	}
	n = min(n, bottom-top+1)
	if recordScrollback {
		for i := 0; i < n; i++ {
			vt.pushScrollback(scr.grid[top+i])
		}
	}
	copy(scr.grid[top:], scr.grid[top+n:bottom+1])
	for r := bottom - n + 1; r <= bottom; r++ {
		scr.grid[r] = newVTermRow(scr.cols, vt.style)
	}
}

func (vt *vterm) scrollDownRegion(scr *vtermScreen, top, bottom, n int) {
	top = max(top, 0)
	bottom = min(bottom, scr.rows-1)
	if top > bottom || n <= 0 {
		return
	}
	n = min(n, bottom-top+1)
	copy(scr.grid[top+n:bottom+1], scr.grid[top:bottom+1-n])
	for r := top; r < top+n; r++ {
		scr.grid[r] = newVTermRow(scr.cols, vt.style)
	}
}

// clearWideOverlap clears the partner cell of a wide character at (row, col).
func (vt *vterm) clearWideOverlap(scr *vtermScreen, row, col int) {
	if row < 0 || row >= scr.rows || col < 0 || col >= scr.cols {
		return
	}
	cells := scr.grid[row].cells
	blank := blankVTermCell(vt.style)
	if cells[col].width == 2 && col+1 < scr.cols {
		cells[col+1] = blank
	} else if cells[col].width == 0 && col > 0 {
		cells[col-1] = blank
	}
}

// clearWidePairAt clears both halves of a wide character if col is either half.
func (vt *vterm) clearWidePairAt(scr *vtermScreen, row, col int) {
	if row < 0 || row >= scr.rows || col < 0 || col >= scr.cols {
		return
	}
	cells := scr.grid[row].cells
	blank := blankVTermCell(vt.style)
	switch cells[col].width {
	case 2:
		cells[col] = blank
		if col+1 < scr.cols {
			cells[col+1] = blank
		}
	case 0:
		cells[col] = blank
		if col > 0 {
			cells[col-1] = blank
		}
	}
}

var decSpecialGraphics = map[rune]rune{
	'`': '◆', 'a': '▒', 'b': '␉', 'c': '␌', 'd': '␍', 'e': '␊', 'f': '°', 'g': '±',
	'h': '␤', 'i': '␋', 'j': '┘', 'k': '┐', 'l': '┌', 'm': '└', 'n': '┼', 'o': '⎺',
	'p': '⎻', 'q': '─', 'r': '⎼', 's': '⎽', 't': '├', 'u': '┤', 'v': '┴', 'w': '┬',
	'x': '│', 'y': '≤', 'z': '≥', '{': 'π', '|': '≠', '}': '£', '~': '·',
}

func (vt *vterm) insertBlankCharsAt(scr *vtermScreen, row, col, n int) {
	if row < 0 || row >= scr.rows || col < 0 || col >= scr.cols || n <= 0 {
		return
	}
	n = min(n, scr.cols-col)
	cells := scr.grid[row].cells
	if cells[col].width == 0 {
		vt.clearWidePairAt(scr, row, col)
	}
	if scr.cols-n-1 >= col && cells[scr.cols-n-1].width == 2 {
		vt.clearWidePairAt(scr, row, scr.cols-n-1)
	}
	copy(cells[col+n:], cells[col:scr.cols-n])
	blank := blankVTermCell(vt.style)
	for c := col; c < col+n; c++ {
		cells[c] = blank
	}
}

func (vt *vterm) deleteCharsAt(scr *vtermScreen, row, col, n int) {
	if row < 0 || row >= scr.rows || col < 0 || col >= scr.cols || n <= 0 {
		return
	}
	n = min(n, scr.cols-col)
	cells := scr.grid[row].cells
	if cells[col].width == 0 {
		vt.clearWidePairAt(scr, row, col)
	}
	if col+n < scr.cols && cells[col+n].width == 0 {
		vt.clearWidePairAt(scr, row, col+n)
	}
	copy(cells[col:], cells[col+n:])
	blank := blankVTermCell(vt.style)
	for c := scr.cols - n; c < scr.cols; c++ {
		cells[c] = blank
	}
}

func (vt *vterm) putRune(r rune) {
	scr := vt.activeScreen()
	charset := vt.g0Charset
	if vt.activeCharset == 1 {
		charset = vt.g1Charset
	}
	if charset == '0' {
		if mapped, ok := decSpecialGraphics[r]; ok {
			r = mapped
		}
	}

	w := singleRuneWidth(r)
	if w <= 0 {
		if r < 0x20 {
			return
		}
		// Zero-width combining character: attach to the previous cell.
		targetC := scr.curC - 1
		if scr.pendingWrap {
			targetC = scr.cols - 1
		}
		if targetC >= 0 && targetC < scr.cols {
			cells := scr.grid[scr.curR].cells
			if cells[targetC].width == 0 && targetC > 0 {
				targetC--
			}
			if len(cells[targetC].combining)+utf8.RuneLen(r) <= maxCombiningBytes {
				cells[targetC].combining += string(r)
				if vt.lastPrintedCh != 0 {
					vt.lastPrintedCombining = cells[targetC].combining
				}
			}
		}
		return
	}

	scr.curR = clampInt(scr.curR, 0, scr.rows-1)
	scr.curC = clampInt(scr.curC, 0, scr.cols-1)
	if w == 2 && scr.cols < 2 {
		return
	}

	if scr.pendingWrap && scr.autoWrap {
		scr.grid[scr.curR].wrapped = true
		scr.pendingWrap = false
		scr.curC = 0
		vt.lineFeed()
	}

	if w == 2 && scr.curC == scr.cols-1 {
		if !scr.autoWrap {
			return
		}
		vt.clearWideOverlap(scr, scr.curR, scr.curC)
		scr.grid[scr.curR].cells[scr.curC] = blankVTermCell(vt.style)
		scr.grid[scr.curR].wrapped = true
		scr.curC = 0
		vt.lineFeed()
	}

	if vt.insertMode {
		vt.insertBlankCharsAt(scr, scr.curR, scr.curC, w)
	}

	cells := scr.grid[scr.curR].cells
	vt.clearWideOverlap(scr, scr.curR, scr.curC)
	if w == 2 {
		vt.clearWideOverlap(scr, scr.curR, scr.curC+1)
		cells[scr.curC] = vtermCell{ch: r, width: 2, style: vt.style}
		cells[scr.curC+1] = vtermCell{width: 0, style: vt.style}
	} else {
		cells[scr.curC] = vtermCell{ch: r, width: 1, style: vt.style}
	}
	if scr.curC+w >= scr.cols {
		scr.curC = scr.cols - 1
		scr.pendingWrap = scr.autoWrap
	} else {
		scr.curC += w
	}
	vt.lastPrintedCh = r
	vt.lastPrintedCombining = ""
}

func (vt *vterm) stepEsc(b byte) {
	vt.state = vtStateGround
	switch b {
	case '[':
		vt.state = vtStateCSI
		vt.csiPrefix = 0
		vt.csiParams.Reset()
		vt.csiInterm = vt.csiInterm[:0]
		vt.csiCorrupt = false
	case ']':
		vt.state = vtStateOSC
		vt.oscBuf.Reset()
		vt.oscOverflow = false
		vt.oscEscSeen = false
	case 'P', 'X', '^', '_':
		vt.state = vtStateDCS
		vt.oscEscSeen = false
	case '7': // DECSC
		vt.saveCursorOn(vt.activeScreen())
	case '8': // DECRC
		vt.restoreCursorOn(vt.activeScreen())
	case 'D': // IND
		vt.activeScreen().pendingWrap = false
		vt.lineFeed()
	case 'M': // RI
		vt.reverseIndex()
	case 'E': // NEL
		scr := vt.activeScreen()
		scr.pendingWrap = false
		scr.curC = 0
		vt.lineFeed()
	case 'H': // HTS
		scr := vt.activeScreen()
		if scr.curC >= 0 && scr.curC < len(scr.tabs) {
			scr.tabs[scr.curC] = true
		}
	case 'c': // RIS
		vt.resetFull()
	case '=': // DECKPAM
		vt.appKeypad = true
	case '>': // DECKPNM
		vt.appKeypad = false
	default:
		if b >= 0x20 && b <= 0x2f {
			if len(vt.escInterm) < maxIntermLen {
				vt.escInterm = append(vt.escInterm, b)
			}
			vt.state = vtStateEscIntermediate
		}
	}
}

func (vt *vterm) stepEscIntermediate(b byte) {
	if b >= 0x20 && b <= 0x2f {
		if len(vt.escInterm) < maxIntermLen {
			vt.escInterm = append(vt.escInterm, b)
		}
		return
	}
	if b >= 0x30 && b <= 0x7e && len(vt.escInterm) == 1 {
		switch vt.escInterm[0] {
		case '(':
			if b == '0' || b == 'B' {
				vt.g0Charset = b
			}
		case ')':
			if b == '0' || b == 'B' {
				vt.g1Charset = b
			}
		case '#':
			if b == '8' { // DECALN
				scr := vt.activeScreen()
				scr.scrollTop, scr.scrollBottom = 0, scr.rows-1
				scr.originMode, scr.pendingWrap = false, false
				scr.curR, scr.curC = 0, 0
				for r := range scr.grid {
					for c := range scr.grid[r].cells {
						scr.grid[r].cells[c] = vtermCell{ch: 'E', width: 1}
					}
					scr.grid[r].wrapped = false
				}
			}
		}
	}
	vt.state = vtStateGround
}

func (vt *vterm) stepOSC(b byte) {
	if vt.oscEscSeen {
		vt.oscEscSeen = false
		if b == '\\' {
			if !vt.oscOverflow {
				vt.dispatchOSC(vt.oscBuf.String())
			}
			vt.state = vtStateGround
			return
		}
		// Any other ESC sequence aborts the unterminated OSC.
		vt.state = vtStateEsc
		vt.escInterm = vt.escInterm[:0]
		vt.stepEsc(b)
		return
	}
	switch b {
	case 0x07:
		if !vt.oscOverflow {
			vt.dispatchOSC(vt.oscBuf.String())
		}
		vt.state = vtStateGround
	case 0x1b:
		vt.oscEscSeen = true
	default:
		if vt.oscBuf.Len() < maxOSCBufLen {
			vt.oscBuf.WriteByte(b)
		} else {
			vt.oscOverflow = true
		}
	}
}

func (vt *vterm) stepDCS(b byte) {
	if vt.oscEscSeen {
		vt.oscEscSeen = false
		if b == '\\' {
			vt.state = vtStateGround
			return
		}
		vt.state = vtStateEsc
		vt.escInterm = vt.escInterm[:0]
		vt.stepEsc(b)
		return
	}
	if b == 0x1b {
		vt.oscEscSeen = true
	}
}

func (vt *vterm) dispatchOSC(payload string) {
	code, rest, ok := strings.Cut(payload, ";")
	if !ok {
		return
	}
	switch code {
	case "0", "2":
		vt.title = strings.TrimSpace(rest)
	case "7":
		vt.cwd = strings.TrimSpace(rest)
	case "8":
		// OSC 8 ; params ; URI ST
		if _, uri, ok := strings.Cut(rest, ";"); ok {
			vt.style.hyperlink = uri
		}
	case "9":
		if strings.HasPrefix(rest, "4;") {
			// ConEmu / Ghostty progress: 9;4;state;progress
			vt.progressSeq = rest
			if strings.HasPrefix(rest, "4;0") {
				vt.progressSeq = ""
			}
		} else {
			// iTerm2-style desktop notification.
			vt.notices++
			vt.lastNotice = rest
		}
	case "777":
		// urxvt / Ghostty notification: 777;notify;title;body
		if strings.HasPrefix(rest, "notify;") {
			vt.notices++
			parts := strings.SplitN(rest, ";", 3)
			vt.lastNotice = strings.TrimSpace(strings.Join(parts[1:], " "))
		}
	case "99":
		// Kitty desktop notification: 99;metadata;payload
		if _, body, ok := strings.Cut(rest, ";"); ok && body != "" {
			vt.notices++
			vt.lastNotice = body
		}
	case "7501":
		// Agent status reports (state=working|blocked|idle|done|clear).
		s := strings.TrimSpace(rest)
		if strings.HasPrefix(s, "?") {
			vt.statusReplies.WriteString("\x1b]7501;?\x1b\\")
			return
		}
		switch {
		case strings.Contains(s, "state=done"):
			vt.agentState = strings.Replace(s, "state=done", "state=idle", 1)
		case strings.Contains(s, "state=clear"):
			vt.agentState = ""
		default:
			vt.agentState = s
		}
	case "7502":
		vt.handleLegacySnapshotOSC(rest)
	}
}

// handleLegacySnapshotOSC understands the markers older di servers embed in
// their attach snapshots so previews of those sessions still render correctly.
func (vt *vterm) handleLegacySnapshotOSC(rest string) {
	var rows, cols int
	begin := false
	for _, field := range strings.Split(rest, ";") {
		switch {
		case field == "snap=begin":
			begin = true
		case field == "snap=end":
			vt.sawSnapshotEnd = true
		case strings.HasPrefix(field, "rows="):
			rows, _ = strconv.Atoi(field[5:])
		case strings.HasPrefix(field, "cols="):
			cols, _ = strconv.Atoi(field[5:])
		case strings.HasPrefix(field, "wrap="):
			scr := vt.activeScreen()
			for _, idx := range strings.Split(field[5:], ",") {
				if r, err := strconv.Atoi(idx); err == nil && r >= 0 && r < len(scr.grid) {
					scr.grid[r].wrapped = true
				}
			}
		case field == "pw=1":
			scr := vt.activeScreen()
			if scr.curC == scr.cols-1 && scr.autoWrap {
				scr.pendingWrap = true
			}
		}
	}
	if begin && rows > 0 && cols > 0 {
		vt.Resize(rows, cols)
	}
}

func (vt *vterm) stepCSI(b byte) {
	switch {
	case b >= 0x30 && b <= 0x3f:
		if len(vt.csiInterm) > 0 {
			vt.csiCorrupt = true
			return
		}
		if b == '?' || b == '>' || b == '<' || b == '=' {
			if vt.csiPrefix == 0 && vt.csiParams.Len() == 0 {
				vt.csiPrefix = b
			} else {
				vt.csiCorrupt = true
			}
			return
		}
		if vt.csiParams.Len() < maxCSIParamsLen {
			vt.csiParams.WriteByte(b)
		} else {
			vt.csiCorrupt = true
		}
	case b >= 0x20 && b <= 0x2f:
		if len(vt.csiInterm) < maxIntermLen {
			vt.csiInterm = append(vt.csiInterm, b)
		} else {
			vt.csiCorrupt = true
		}
	case b >= 0x40 && b <= 0x7e:
		if !vt.csiCorrupt {
			vt.dispatchCSI(vt.csiPrefix, vt.csiInterm, vt.csiParams.String(), b)
		}
		vt.state = vtStateGround
		vt.csiCorrupt = false
	}
}

func parseCSIParams(raw string) []int {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ";")
	out := make([]int, len(parts))
	for i, p := range parts {
		if idx := strings.IndexByte(p, ':'); idx >= 0 {
			p = p[:idx]
		}
		if p == "" {
			continue
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(p, "-") {
				out[i] = maxCSIParamValue
			}
			continue
		}
		out[i] = clampInt(v, 0, maxCSIParamValue)
	}
	return out
}

func csiParam(params []int, idx, def int) int {
	if idx < len(params) && params[idx] > 0 {
		return params[idx]
	}
	return def
}

func (vt *vterm) dispatchCSI(prefix byte, interm []byte, rawParams string, final byte) {
	scr := vt.activeScreen()
	params := parseCSIParams(rawParams)

	// Kitty keyboard protocol: CSI > flags u, CSI < n u, CSI = flags ; mode u, CSI ? u
	if final == 'u' && len(interm) == 0 && prefix != 0 {
		switch prefix {
		case '>':
			scr.pushKittyFlags(uint8(csiParam(params, 0, 0)))
		case '<':
			scr.popKittyFlags(csiParam(params, 0, 1))
		case '=':
			scr.setKittyFlags(uint8(csiParam(params, 0, 0)), csiParam(params, 1, 1))
		case '?':
			fmt.Fprintf(&vt.pendingReplies, "\x1b[?%du", scr.currentKittyFlags())
		}
		return
	}

	if len(interm) == 1 {
		switch {
		case final == 'q' && prefix == 0 && interm[0] == ' ': // DECSCUSR
			shape := csiParam(params, 0, 0)
			if shape <= 6 {
				scr.cursorShape = uint8(shape)
			}
		case final == 'p' && prefix == 0 && interm[0] == '!': // DECSTR
			vt.softReset()
		case final == 'p' && prefix == '?' && interm[0] == '$': // DECRQM
			if len(params) > 0 {
				fmt.Fprintf(&vt.pendingReplies, "\x1b[?%d;%d$y", params[0], vt.queryDECPrivateMode(params[0]))
			}
		}
		return
	}

	switch {
	case final == 'c' && prefix == '>' && len(interm) == 0: // DA2
		if csiParam(params, 0, 0) == 0 {
			vt.pendingReplies.WriteString("\x1b[>1;10;0c")
		}
		return
	case final == 'm' && prefix == '>' && len(interm) == 0: // modifyOtherKeys
		if len(params) >= 2 && params[0] == 4 {
			vt.modifyOtherKeys = clampInt(params[1], 0, 2)
		} else if len(params) == 0 || params[0] == 4 {
			vt.modifyOtherKeys = 0
		}
		return
	case final == 'n' && prefix == '>' && len(interm) == 0:
		if len(params) == 1 && params[0] == 4 {
			vt.modifyOtherKeys = 0
		}
		return
	case prefix == '?':
		if len(interm) == 0 {
			vt.dispatchDECPrivate(params, final)
		}
		return
	case prefix != 0 || len(interm) != 0:
		return
	}

	// Non-SGR standard CSI commands do not use colon subparameters.
	if final != 'm' && strings.IndexByte(rawParams, ':') >= 0 {
		return
	}

	switch final {
	case 'A': // CUU
		minRow := 0
		if scr.originMode || scr.curR >= scr.scrollTop {
			minRow = scr.scrollTop
		}
		scr.pendingWrap = false
		scr.curR = max(scr.curR-csiParam(params, 0, 1), minRow)
	case 'B', 'e': // CUD
		maxRow := scr.rows - 1
		if scr.originMode || scr.curR <= scr.scrollBottom {
			maxRow = scr.scrollBottom
		}
		scr.pendingWrap = false
		scr.curR = min(scr.curR+csiParam(params, 0, 1), maxRow)
	case 'C', 'a': // CUF
		scr.pendingWrap = false
		scr.curC = min(scr.curC+csiParam(params, 0, 1), scr.cols-1)
	case 'D': // CUB
		scr.pendingWrap = false
		scr.curC = max(scr.curC-csiParam(params, 0, 1), 0)
	case 'E': // CNL
		maxRow := scr.rows - 1
		if scr.originMode || scr.curR <= scr.scrollBottom {
			maxRow = scr.scrollBottom
		}
		scr.pendingWrap = false
		scr.curC = 0
		scr.curR = min(scr.curR+csiParam(params, 0, 1), maxRow)
	case 'F': // CPL
		minRow := 0
		if scr.originMode || scr.curR >= scr.scrollTop {
			minRow = scr.scrollTop
		}
		scr.pendingWrap = false
		scr.curC = 0
		scr.curR = max(scr.curR-csiParam(params, 0, 1), minRow)
	case 'G', '`': // CHA
		scr.pendingWrap = false
		scr.curC = clampInt(csiParam(params, 0, 1)-1, 0, scr.cols-1)
	case 'd': // VPA
		row := csiParam(params, 0, 1) - 1
		minRow, maxRow := 0, scr.rows-1
		if scr.originMode {
			row += scr.scrollTop
			minRow, maxRow = scr.scrollTop, scr.scrollBottom
		}
		scr.pendingWrap = false
		scr.curR = clampInt(row, minRow, maxRow)
	case 'H', 'f': // CUP / HVP
		row := csiParam(params, 0, 1) - 1
		minRow, maxRow := 0, scr.rows-1
		if scr.originMode {
			row += scr.scrollTop
			minRow, maxRow = scr.scrollTop, scr.scrollBottom
		}
		scr.pendingWrap = false
		scr.curR = clampInt(row, minRow, maxRow)
		scr.curC = clampInt(csiParam(params, 1, 1)-1, 0, scr.cols-1)
	case 'I': // CHT
		scr.pendingWrap = false
		vt.advanceTabForward(scr, csiParam(params, 0, 1))
	case 'Z': // CBT
		scr.pendingWrap = false
		vt.advanceTabBackward(scr, csiParam(params, 0, 1))
	case 'g': // TBC
		switch csiParam(params, 0, 0) {
		case 0:
			if scr.curC >= 0 && scr.curC < len(scr.tabs) {
				scr.tabs[scr.curC] = false
			}
		case 3:
			clear(scr.tabs)
		}
	case 'J': // ED
		vt.eraseInDisplay(scr, csiParam(params, 0, 0))
	case 'K': // EL
		vt.eraseInLine(scr, csiParam(params, 0, 0))
	case 'X': // ECH
		scr.pendingWrap = false
		blank := blankVTermCell(vt.style)
		end := min(scr.curC+csiParam(params, 0, 1), scr.cols)
		for c := scr.curC; c < end; c++ {
			vt.clearWideOverlap(scr, scr.curR, c)
			scr.grid[scr.curR].cells[c] = blank
		}
	case 'L': // IL
		if scr.curR >= scr.scrollTop && scr.curR <= scr.scrollBottom {
			scr.pendingWrap = false
			scr.curC = 0
			vt.scrollDownRegion(scr, scr.curR, scr.scrollBottom, csiParam(params, 0, 1))
		}
	case 'M': // DL
		if scr.curR >= scr.scrollTop && scr.curR <= scr.scrollBottom {
			scr.pendingWrap = false
			scr.curC = 0
			vt.scrollUpRegion(scr, scr.curR, scr.scrollBottom, csiParam(params, 0, 1), false)
		}
	case '@': // ICH
		scr.pendingWrap = false
		vt.insertBlankCharsAt(scr, scr.curR, scr.curC, csiParam(params, 0, 1))
	case 'P': // DCH
		scr.pendingWrap = false
		vt.deleteCharsAt(scr, scr.curR, scr.curC, csiParam(params, 0, 1))
	case 'S': // SU
		vt.scrollUpRegion(scr, scr.scrollTop, scr.scrollBottom, csiParam(params, 0, 1), !vt.inAltScreen && scr.scrollTop == 0)
	case 'T': // SD
		vt.scrollDownRegion(scr, scr.scrollTop, scr.scrollBottom, csiParam(params, 0, 1))
	case 'b': // REP
		if vt.lastPrintedCh != 0 {
			ch, comb := vt.lastPrintedCh, vt.lastPrintedCombining
			for i := 0; i < min(csiParam(params, 0, 1), 4096); i++ {
				vt.putRune(ch)
				for _, cr := range comb {
					vt.putRune(cr)
				}
			}
		}
	case 'c': // DA1
		if csiParam(params, 0, 0) == 0 {
			vt.pendingReplies.WriteString("\x1b[?62;22c")
		}
	case 'n': // DSR
		switch csiParam(params, 0, 0) {
		case 5:
			vt.pendingReplies.WriteString("\x1b[0n")
		case 6:
			fmt.Fprintf(&vt.pendingReplies, "\x1b[%d;%dR", vt.reportRow(scr), scr.curC+1)
		}
	case 'h', 'l': // SM / RM
		for _, p := range params {
			switch p {
			case 4:
				vt.insertMode = final == 'h'
			case 20:
				vt.newlineMode = final == 'h'
			}
		}
	case 'r': // DECSTBM
		top := max(csiParam(params, 0, 1)-1, 0)
		bottom := min(csiParam(params, 1, scr.rows)-1, scr.rows-1)
		if top < bottom {
			scr.scrollTop, scr.scrollBottom = top, bottom
			scr.curC = 0
			scr.curR = 0
			if scr.originMode {
				scr.curR = top
			}
			scr.pendingWrap = false
		}
	case 's': // SCOSC
		if len(params) == 0 {
			vt.saveCursorOn(scr)
		}
	case 'u': // SCORC
		if len(params) == 0 {
			vt.restoreCursorOn(scr)
		}
	case 'm': // SGR
		vt.handleSGR(rawParams)
	}
}

func (vt *vterm) reportRow(scr *vtermScreen) int {
	if scr.originMode {
		return max(scr.curR-scr.scrollTop+1, 1)
	}
	return scr.curR + 1
}

func (vt *vterm) queryDECPrivateMode(mode int) int {
	scr := vt.activeScreen()
	var set bool
	switch mode {
	case 1:
		set = vt.appCursorKeys
	case 6:
		set = scr.originMode
	case 7:
		set = scr.autoWrap
	case 25:
		set = scr.cursorVis
	case 47, 1047, 1049:
		set = vt.inAltScreen
	case 1000, 1002, 1003:
		set = vt.mouseMode == mode
	case 1004:
		set = vt.focusEvents
	case 1006:
		set = vt.mouseSGR
	case 1048:
		set = true
	case 2004:
		set = vt.bracketedPaste
	case 2026:
		set = vt.syncOutput
	default:
		return 0
	}
	if set {
		return 1
	}
	return 2
}

func (vt *vterm) eraseInDisplay(scr *vtermScreen, mode int) {
	switch mode {
	case 0: // cursor to end of screen
		vt.eraseInLine(scr, 0)
		for r := scr.curR + 1; r < scr.rows; r++ {
			scr.grid[r] = newVTermRow(scr.cols, vt.style)
		}
	case 1: // start of screen to cursor
		for r := 0; r < scr.curR; r++ {
			scr.grid[r] = newVTermRow(scr.cols, vt.style)
		}
		vt.eraseInLine(scr, 1)
	case 2: // entire screen
		scr.pendingWrap = false
		for r := range scr.grid {
			scr.grid[r] = newVTermRow(scr.cols, vt.style)
		}
	case 3: // scrollback
		if !vt.inAltScreen {
			vt.scrollback = nil
			vt.scrollbackBytes = 0
		}
	}
}

func (vt *vterm) eraseInLine(scr *vtermScreen, mode int) {
	if scr.curR < 0 || scr.curR >= scr.rows {
		return
	}
	scr.pendingWrap = false
	blank := blankVTermCell(vt.style)
	row := &scr.grid[scr.curR]
	switch mode {
	case 0:
		vt.clearWideOverlap(scr, scr.curR, scr.curC)
		for c := scr.curC; c < scr.cols; c++ {
			row.cells[c] = blank
		}
		row.wrapped = false
	case 1:
		vt.clearWideOverlap(scr, scr.curR, scr.curC)
		for c := 0; c <= scr.curC && c < scr.cols; c++ {
			row.cells[c] = blank
		}
	case 2:
		for c := range row.cells {
			row.cells[c] = blank
		}
		row.wrapped = false
	}
}

func (vt *vterm) dispatchDECPrivate(params []int, final byte) {
	if final == 'n' {
		if csiParam(params, 0, 0) == 6 { // DECXCPR
			scr := vt.activeScreen()
			fmt.Fprintf(&vt.pendingReplies, "\x1b[?%d;%dR", vt.reportRow(scr), scr.curC+1)
		}
		return
	}
	if final != 'h' && final != 'l' {
		return
	}
	set := final == 'h'
	for _, p := range params {
		// Re-resolve the active screen so CSI ?1049;25l applies 25 to the new screen.
		scr := vt.activeScreen()
		switch p {
		case 1:
			vt.appCursorKeys = set
		case 6:
			scr.originMode = set
			scr.curC = 0
			scr.curR = 0
			if set {
				scr.curR = scr.scrollTop
			}
			scr.pendingWrap = false
		case 7:
			scr.autoWrap = set
			if !set {
				scr.pendingWrap = false
			}
		case 25:
			scr.cursorVis = set
		case 47, 1047, 1049:
			vt.switchAltScreen(p, set)
		case 1048:
			if set {
				vt.saveCursorOn(scr)
			} else {
				vt.restoreCursorOn(scr)
			}
		case 1000, 1002, 1003:
			if set {
				vt.mouseMode = p
			} else if vt.mouseMode == p {
				vt.mouseMode = 0
			}
		case 1004:
			vt.focusEvents = set
		case 1006:
			vt.mouseSGR = set
		case 2004:
			vt.bracketedPaste = set
		case 2026:
			vt.syncOutput = set
		}
	}
}

func (vt *vterm) switchAltScreen(mode int, enter bool) {
	if enter {
		if mode == 1049 && !vt.inAltScreen {
			vt.saveCursorOn(&vt.primary)
		}
		if mode != 47 || !vt.inAltScreen {
			alt := newVTermScreen(vt.primary.rows, vt.primary.cols)
			if mode == 47 {
				alt = vt.alt
				alt.pendingWrap = false
			}
			if !vt.inAltScreen {
				alt.curR = clampInt(vt.primary.curR, 0, alt.rows-1)
				alt.curC = clampInt(vt.primary.curC, 0, alt.cols-1)
				shareGlobalModes(&alt, &vt.primary)
			}
			alt.kittyStack, alt.kittyLen = vt.alt.kittyStack, vt.alt.kittyLen
			vt.alt = alt
		}
		vt.inAltScreen = true
		return
	}
	if !vt.inAltScreen {
		if mode == 1049 {
			vt.restoreCursorOn(&vt.primary)
		}
		return
	}
	vt.inAltScreen = false
	shareGlobalModes(&vt.primary, &vt.alt)
	if mode == 1049 {
		vt.restoreCursorOn(&vt.primary)
	} else {
		// 47 and 1047 keep the one cursor terminals have for both buffers.
		vt.primary.curR = clampInt(vt.alt.curR, 0, vt.primary.rows-1)
		vt.primary.curC = clampInt(vt.alt.curC, 0, vt.primary.cols-1)
	}
	vt.primary.pendingWrap = false
}

// shareGlobalModes copies modes that terminals keep once for both buffers.
func shareGlobalModes(dst, src *vtermScreen) {
	dst.autoWrap = src.autoWrap
	dst.cursorVis = src.cursorVis
	dst.cursorShape = src.cursorShape
	dst.originMode = src.originMode
}

func (vt *vterm) saveCursorOn(scr *vtermScreen) {
	scr.savedCurR = scr.curR
	scr.savedCurC = scr.curC
	scr.savedStyle = vt.style
	scr.savedOriginMode = scr.originMode
	scr.savedPendingWrap = scr.pendingWrap
}

func (vt *vterm) restoreCursorOn(scr *vtermScreen) {
	scr.curR = clampInt(scr.savedCurR, 0, scr.rows-1)
	scr.curC = clampInt(scr.savedCurC, 0, scr.cols-1)
	vt.style = scr.savedStyle
	scr.originMode = scr.savedOriginMode
	if scr.originMode {
		scr.curR = clampInt(scr.curR, scr.scrollTop, scr.scrollBottom)
	}
	scr.pendingWrap = scr.savedPendingWrap && scr.autoWrap && scr.curC == scr.cols-1
}

func (vt *vterm) softReset() {
	scr := vt.activeScreen()
	scr.originMode = false
	scr.autoWrap = true
	scr.cursorVis = true
	scr.cursorShape = 0
	scr.scrollTop = 0
	scr.scrollBottom = scr.rows - 1
	scr.pendingWrap = false
	scr.savedCurR, scr.savedCurC = 0, 0
	scr.savedStyle = vtermStyle{}
	scr.savedOriginMode = false
	scr.savedPendingWrap = false
	vt.style = vtermStyle{}
	vt.appCursorKeys = false
	vt.appKeypad = false
	vt.insertMode = false
	vt.newlineMode = false
	vt.g0Charset, vt.g1Charset, vt.activeCharset = 'B', 'B', 0
}

func (vt *vterm) resetFull() {
	rows, cols := vt.primary.rows, vt.primary.cols
	scrollback, scrollbackBytes, maxScrollback := vt.scrollback, vt.scrollbackBytes, vt.maxScrollback
	bells, notices, lastNotice := vt.bells, vt.notices, vt.lastNotice
	replies, statusReplies := vt.DrainReplies(), vt.DrainStatusReplies()
	*vt = vterm{
		primary:         newVTermScreen(rows, cols),
		alt:             newVTermScreen(rows, cols),
		scrollback:      scrollback,
		scrollbackBytes: scrollbackBytes,
		maxScrollback:   maxScrollback,
		g0Charset:       'B',
		g1Charset:       'B',
		bells:           bells,
		notices:         notices,
		lastNotice:      lastNotice,
	}
	vt.pendingReplies.Write(replies)
	vt.statusReplies.Write(statusReplies)
}

func (vt *vterm) handleSGR(raw string) {
	if raw == "" {
		vt.style = vtermStyle{hyperlink: vt.style.hyperlink}
		return
	}
	setColor := func(code int, c vtermColor) {
		switch code {
		case 38:
			vt.style.fg = c
		case 48:
			vt.style.bg = c
		default:
			vt.style.ul = c
		}
	}
	tokens := strings.Split(raw, ";")
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		// Colon subparameters: 4:3 (curly), 38:2::r:g:b, 38:5:n
		if strings.Contains(tok, ":") {
			sub := strings.Split(tok, ":")
			code, _ := strconv.Atoi(sub[0])
			switch code {
			case 4:
				vt.style.flags &^= vtermUnderlineMask
				kind := 1
				if len(sub) > 1 && sub[1] != "" {
					kind, _ = strconv.Atoi(sub[1])
				}
				switch kind {
				case 0:
				case 2:
					vt.style.flags |= vtermAttrDoubleUnderline
				case 3:
					vt.style.flags |= vtermAttrCurlyUnderline
				case 4:
					vt.style.flags |= vtermAttrDottedUnderline
				case 5:
					vt.style.flags |= vtermAttrDashedUnderline
				default:
					vt.style.flags |= vtermAttrUnderline
				}
			case 38, 48, 58:
				if len(sub) >= 3 && sub[1] == "5" {
					if idx, err := strconv.Atoi(sub[2]); err == nil && idx >= 0 && idx <= 255 {
						setColor(code, vtermColor{kind: vtermColorIndexed, index: uint8(idx)})
					}
				} else if len(sub) >= 5 && sub[1] == "2" {
					off := 2
					if len(sub) >= 6 {
						off = 3
					}
					if c, ok := parseRGB(sub[off], sub[off+1], sub[off+2]); ok {
						setColor(code, c)
					}
				}
			}
			continue
		}

		n, _ := strconv.Atoi(tok)
		switch {
		case n == 0:
			vt.style = vtermStyle{hyperlink: vt.style.hyperlink}
		case n == 1:
			vt.style.flags |= vtermAttrBold
		case n == 2:
			vt.style.flags |= vtermAttrDim
		case n == 3:
			vt.style.flags |= vtermAttrItalic
		case n == 4:
			vt.style.flags = vt.style.flags&^vtermUnderlineMask | vtermAttrUnderline
		case n == 5, n == 6:
			vt.style.flags |= vtermAttrBlink
		case n == 7:
			vt.style.flags |= vtermAttrReverse
		case n == 8:
			vt.style.flags |= vtermAttrInvisible
		case n == 9:
			vt.style.flags |= vtermAttrStrikethrough
		case n == 21:
			vt.style.flags = vt.style.flags&^vtermUnderlineMask | vtermAttrDoubleUnderline
		case n == 22:
			vt.style.flags &^= vtermAttrBold | vtermAttrDim
		case n == 23:
			vt.style.flags &^= vtermAttrItalic
		case n == 24:
			vt.style.flags &^= vtermUnderlineMask
		case n == 25:
			vt.style.flags &^= vtermAttrBlink
		case n == 27:
			vt.style.flags &^= vtermAttrReverse
		case n == 28:
			vt.style.flags &^= vtermAttrInvisible
		case n == 29:
			vt.style.flags &^= vtermAttrStrikethrough
		case n >= 30 && n <= 37:
			vt.style.fg = vtermColor{kind: vtermColorIndexed, index: uint8(n - 30)}
		case n == 38 || n == 48 || n == 58:
			if i+1 >= len(tokens) {
				break
			}
			mode, _ := strconv.Atoi(tokens[i+1])
			if mode == 5 && i+2 < len(tokens) {
				if idx, err := strconv.Atoi(tokens[i+2]); err == nil && idx >= 0 && idx <= 255 {
					setColor(n, vtermColor{kind: vtermColorIndexed, index: uint8(idx)})
				}
				i += 2
			} else if mode == 2 && i+4 < len(tokens) {
				if c, ok := parseRGB(tokens[i+2], tokens[i+3], tokens[i+4]); ok {
					setColor(n, c)
				}
				i += 4
			}
		case n == 39:
			vt.style.fg = vtermColor{}
		case n >= 40 && n <= 47:
			vt.style.bg = vtermColor{kind: vtermColorIndexed, index: uint8(n - 40)}
		case n == 49:
			vt.style.bg = vtermColor{}
		case n == 53:
			vt.style.flags |= vtermAttrOverline
		case n == 55:
			vt.style.flags &^= vtermAttrOverline
		case n == 59:
			vt.style.ul = vtermColor{}
		case n >= 90 && n <= 97:
			vt.style.fg = vtermColor{kind: vtermColorIndexed, index: uint8(n - 90 + 8)}
		case n >= 100 && n <= 107:
			vt.style.bg = vtermColor{kind: vtermColorIndexed, index: uint8(n - 100 + 8)}
		}
	}
}

func parseRGB(rs, gs, bs string) (vtermColor, bool) {
	r, err1 := strconv.Atoi(rs)
	g, err2 := strconv.Atoi(gs)
	b, err3 := strconv.Atoi(bs)
	if err1 != nil || err2 != nil || err3 != nil || r < 0 || r > 255 || g < 0 || g > 255 || b < 0 || b > 255 {
		return vtermColor{}, false
	}
	return vtermColor{kind: vtermColorRGB, r: uint8(r), g: uint8(g), b: uint8(b)}, true
}

// trimBlankTail drops trailing default-styled blank cells, keeping at least minKeep.
func trimBlankTail(cells []vtermCell, minKeep int) []vtermCell {
	end := len(cells)
	for end > minKeep && cells[end-1].isDefaultBlank() {
		end--
	}
	return cells[:end]
}

// reflowRows re-wraps rows to newCols, joining soft-wrapped logical lines and
// tracking one cursor coordinate (trackR, trackC) to its new position.
func reflowRows(rows []vtermRow, newCols int, trackR, trackC int) (out []vtermRow, mappedR, mappedC int) {
	newCols = max(newCols, 1)
	mappedR, mappedC = -1, -1
	for i := 0; i < len(rows); {
		start := i
		for i < len(rows)-1 && rows[i].wrapped {
			i++
		}
		end := i
		i++

		var logical []vtermCell
		cursorIdx := -1
		for r := start; r <= end; r++ {
			minKeep := 0
			if r == trackR {
				minKeep = min(trackC+1, len(rows[r].cells))
			}
			cells := rows[r].cells
			if !rows[r].wrapped {
				cells = trimBlankTail(cells, minKeep)
			}
			for c, cell := range cells {
				if r == trackR && c == trackC {
					cursorIdx = len(logical)
				}
				if cell.width != 0 {
					logical = append(logical, cell)
				}
			}
			if r == trackR && cursorIdx < 0 {
				cursorIdx = len(logical)
			}
		}

		cur := newVTermRow(newCols, vtermStyle{})
		col := 0
		for idx, cell := range logical {
			w := max(int(cell.width), 1)
			if w == 2 && newCols == 1 {
				w, cell.width = 1, 1
			}
			if col+w > newCols {
				cur.wrapped = true
				out = append(out, cur)
				cur = newVTermRow(newCols, vtermStyle{})
				col = 0
			}
			if idx == cursorIdx {
				mappedR, mappedC = len(out), col
			}
			cur.cells[col] = cell
			if w == 2 {
				cur.cells[col+1] = vtermCell{width: 0, style: cell.style}
			}
			col += w
		}
		if cursorIdx >= len(logical) {
			mappedR, mappedC = len(out), clampInt(col, 0, newCols-1)
		}
		cur.wrapped = rows[end].wrapped
		out = append(out, cur)
	}
	return out, mappedR, mappedC
}

// Resize changes both screens, reflowing soft-wrapped primary rows the way
// modern terminals do so the snapshot matches what the host will show.
func (vt *vterm) Resize(rows, cols int) {
	rows = clampInt(rows, 1, maxVTermRows)
	cols = clampInt(cols, 1, maxVTermCols)
	if cols != vt.primary.cols {
		vt.closeScrollbackLine()
	}
	vt.resizeScreen(&vt.primary, rows, cols, true)
	vt.resizeScreen(&vt.alt, rows, cols, false)
}

func (vt *vterm) resizeScreen(scr *vtermScreen, newRows, newCols int, isPrimary bool) {
	if scr.rows == newRows && scr.cols == newCols {
		return
	}
	if isPrimary && newCols != scr.cols {
		last := scr.curR
		for r := len(scr.grid) - 1; r > last; r-- {
			if scr.grid[r].plainText() != "" || scr.grid[r].wrapped {
				last = r
				break
			}
		}
		reflowed, newR, newC := reflowRows(scr.grid[:last+1], newCols, scr.curR, scr.curC)
		if newR < 0 {
			newR, newC = 0, 0
		}
		if extra := len(reflowed) - newRows; extra > 0 {
			for i := 0; i < extra; i++ {
				vt.pushScrollback(reflowed[i])
			}
			reflowed = reflowed[extra:]
			newR = max(0, newR-extra)
		}
		for len(reflowed) < newRows {
			reflowed = append(reflowed, newVTermRow(newCols, vtermStyle{}))
		}
		scr.grid = reflowed
		scr.curR, scr.curC = newR, newC
	} else {
		if newRows < scr.rows {
			drop := scr.rows - newRows
			blankBottom := 0
			for r := scr.rows - 1; r > scr.curR && blankBottom < drop; r-- {
				if scr.grid[r].plainText() != "" {
					break
				}
				blankBottom++
			}
			if pushTop := drop - blankBottom; pushTop > 0 {
				if isPrimary {
					for i := 0; i < pushTop; i++ {
						vt.pushScrollback(scr.grid[i])
					}
				}
				scr.grid = append([]vtermRow(nil), scr.grid[pushTop:scr.rows-blankBottom]...)
				scr.curR = max(scr.curR-pushTop, 0)
				scr.savedCurR = max(scr.savedCurR-pushTop, 0)
			} else {
				scr.grid = scr.grid[:newRows]
			}
		} else {
			for i := scr.rows; i < newRows; i++ {
				scr.grid = append(scr.grid, newVTermRow(scr.cols, vtermStyle{}))
			}
		}
		if newCols != scr.cols {
			blank := blankVTermCell(vtermStyle{})
			for r := range scr.grid {
				old := scr.grid[r].cells
				next := make([]vtermCell, newCols)
				copy(next, old)
				for c := len(old); c < newCols; c++ {
					next[c] = blank
				}
				if next[newCols-1].width == 2 {
					next[newCols-1] = blank
				}
				scr.grid[r].cells = next
			}
		}
	}
	scr.rows, scr.cols = newRows, newCols
	scr.tabs = newDefaultTabs(newCols)
	scr.scrollTop, scr.scrollBottom = 0, newRows-1
	scr.curR = clampInt(scr.curR, 0, newRows-1)
	scr.curC = clampInt(scr.curC, 0, newCols-1)
	scr.savedCurR = clampInt(scr.savedCurR, 0, newRows-1)
	scr.savedCurC = clampInt(scr.savedCurC, 0, newCols-1)
	if scr.savedCurC != newCols-1 {
		scr.savedPendingWrap = false
	}
	scr.pendingWrap = false
}

// decodeANSICells parses a stored scrollback line back into cells.
func decodeANSICells(ansi string, width int) []vtermCell {
	tmp := newVTerm(1, clampInt(width, 1, maxVTermCols))
	tmp.maxScrollback = 0
	tmp.primary.autoWrap = false
	tmp.Write([]byte(ansi))
	return trimBlankTail(tmp.primary.grid[0].cells, 0)
}

// encodeCellsANSI writes cells with SGR/OSC 8 transitions, starting from and
// returning to the default style so the result can be concatenated freely.
func encodeCellsANSI(cells []vtermCell) string {
	if len(cells) == 0 {
		return ""
	}
	var buf bytes.Buffer
	var cur vtermStyle
	for _, c := range cells {
		if c.width == 0 {
			continue
		}
		if c.style != cur {
			writeVTermStyleTransition(&buf, cur, c.style)
			cur = c.style
		}
		if c.ch == 0 {
			buf.WriteByte(' ')
		} else {
			buf.WriteRune(c.ch)
		}
		buf.WriteString(c.combining)
	}
	writeVTermStyleTransition(&buf, cur, vtermStyle{})
	return buf.String()
}

func writeVTermStyleTransition(buf *bytes.Buffer, prev, next vtermStyle) {
	if prev.hyperlink != next.hyperlink {
		buf.WriteString("\x1b]8;;")
		buf.WriteString(next.hyperlink)
		buf.WriteString("\x1b\\")
	}
	if prev.fg == next.fg && prev.bg == next.bg && prev.ul == next.ul && prev.flags == next.flags {
		return
	}
	buf.WriteString("\x1b[0")
	for _, attr := range []struct {
		flag uint16
		code string
	}{
		{vtermAttrBold, ";1"}, {vtermAttrDim, ";2"}, {vtermAttrItalic, ";3"},
	} {
		if next.flags&attr.flag != 0 {
			buf.WriteString(attr.code)
		}
	}
	switch {
	case next.flags&vtermAttrDashedUnderline != 0:
		buf.WriteString(";4:5")
	case next.flags&vtermAttrDottedUnderline != 0:
		buf.WriteString(";4:4")
	case next.flags&vtermAttrCurlyUnderline != 0:
		buf.WriteString(";4:3")
	case next.flags&vtermAttrDoubleUnderline != 0:
		buf.WriteString(";21")
	case next.flags&vtermAttrUnderline != 0:
		buf.WriteString(";4")
	}
	for _, attr := range []struct {
		flag uint16
		code string
	}{
		{vtermAttrBlink, ";5"}, {vtermAttrReverse, ";7"}, {vtermAttrInvisible, ";8"},
		{vtermAttrStrikethrough, ";9"}, {vtermAttrOverline, ";53"},
	} {
		if next.flags&attr.flag != 0 {
			buf.WriteString(attr.code)
		}
	}
	writeSGRColor(buf, next.fg, 30, 90, "38")
	writeSGRColor(buf, next.bg, 40, 100, "48")
	// Underline color uses the colon form so terminals without SGR 58 skip it
	// whole instead of reading its arguments as other attributes.
	switch next.ul.kind {
	case vtermColorIndexed:
		fmt.Fprintf(buf, ";58:5:%d", next.ul.index)
	case vtermColorRGB:
		fmt.Fprintf(buf, ";58:2::%d:%d:%d", next.ul.r, next.ul.g, next.ul.b)
	}
	buf.WriteByte('m')
}

func writeSGRColor(buf *bytes.Buffer, c vtermColor, base, brightBase int, extended string) {
	switch c.kind {
	case vtermColorIndexed:
		switch {
		case c.index < 8:
			fmt.Fprintf(buf, ";%d", base+int(c.index))
		case c.index < 16:
			fmt.Fprintf(buf, ";%d", brightBase+int(c.index)-8)
		default:
			fmt.Fprintf(buf, ";%s;5;%d", extended, c.index)
		}
	case vtermColorRGB:
		fmt.Fprintf(buf, ";%s;2;%d;%d;%d", extended, c.r, c.g, c.b)
	}
}

func clampInt(value, lo, hi int) int {
	if value < lo {
		return lo
	}
	if value > hi {
		return hi
	}
	return value
}
