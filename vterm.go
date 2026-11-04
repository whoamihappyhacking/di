package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

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

	cursorSettleDelay     = 20 * time.Millisecond
	cursorJumpHoldDelay   = 100 * time.Millisecond
	cursorHideSettleDelay = 100 * time.Millisecond
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
	// Erase operations use background color of current style but no text decorations.
	var eraseStyle vtermStyle
	eraseStyle.bg = style.bg
	return vtermCell{
		ch:    ' ',
		width: 1,
		style: eraseStyle,
	}
}

type vtermRow struct {
	cells   []vtermCell
	wrapped bool
	dirty   bool
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
	return vtermRow{
		cells: cells,
		dirty: true,
	}
}

func (r vtermRow) clone() vtermRow {
	cells := make([]vtermCell, len(r.cells))
	copy(cells, r.cells)
	return vtermRow{
		cells:   cells,
		wrapped: r.wrapped,
		dirty:   r.dirty,
	}
}

func (r vtermRow) plainText() string {
	var b strings.Builder
	for _, c := range r.cells {
		if c.width == 0 {
			continue
		}
		if c.ch == 0 {
			b.WriteByte(' ')
		} else {
			b.WriteRune(c.ch)
		}
		if c.combining != "" {
			b.WriteString(c.combining)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

func (r vtermRow) equalContent(other vtermRow) bool {
	return r.plainText() == other.plainText()
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
	var defStyle vtermStyle
	grid := make([]vtermRow, rows)
	for i := range grid {
		grid[i] = newVTermRow(cols, defStyle)
	}
	return vtermScreen{
		rows:         rows,
		cols:         cols,
		grid:         grid,
		tabs:         newDefaultTabs(cols),
		scrollTop:    0,
		scrollBottom: rows - 1,
		autoWrap:     true,
		cursorVis:    true,
	}
}

func clampVTermRows(rows int) int {
	if rows < 1 {
		return defaultVTermRows
	}
	if rows > maxVTermRows {
		return maxVTermRows
	}
	return rows
}

func clampVTermCols(cols int) int {
	if cols < 1 {
		return defaultVTermCols
	}
	if cols > maxVTermCols {
		return maxVTermCols
	}
	return cols
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
	if n >= s.kittyLen {
		s.kittyLen = 0
	} else {
		s.kittyLen -= n
	}
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

// CursorSettleState debounces transient cursor movements and temporary cursor hides
// during full-frame TUI redraws, modeled after herdr's CursorPositionSettleState.
type CursorSettleState struct {
	lastPos         [2]int
	hasLastPos      bool
	settledPos      [2]int
	settledVisible  bool
	hasSettled      bool
	pendingPos      [2]int
	pendingVisible  bool
	pendingDeadline time.Time
	holdStartedAt   time.Time
	hasPending      bool
	lastRowChangeAt time.Time
	hasRowChange    bool
	preferredRow    int
	hasPreferredRow bool
	inputGraceUntil time.Time
}

func newCursorSettleState() CursorSettleState {
	return CursorSettleState{
		settledVisible: true,
	}
}

func (c *CursorSettleState) OnInput() {
	if c.hasPending && c.pendingVisible {
		c.settledPos = c.pendingPos
		c.settledVisible = true
		c.hasSettled = true
	}
	c.hasPending = false
	c.hasRowChange = false
	if c.hasSettled && c.settledVisible {
		c.preferredRow = c.settledPos[0]
		c.hasPreferredRow = true
	}
	c.inputGraceUntil = time.Now().Add(150 * time.Millisecond)
}

func (c *CursorSettleState) NextDeadline() (time.Time, bool) {
	if !c.hasPending {
		return time.Time{}, false
	}
	return c.pendingDeadline, true
}

func (c *CursorSettleState) Advance(now time.Time) bool {
	if !c.hasPending || now.Before(c.pendingDeadline) {
		return false
	}
	c.hasPending = false
	changed := !c.hasSettled || c.settledPos != c.pendingPos || c.settledVisible != c.pendingVisible
	c.settledPos = c.pendingPos
	c.settledVisible = c.pendingVisible
	c.hasSettled = true
	if c.settledVisible {
		c.preferredRow = c.settledPos[0]
		c.hasPreferredRow = true
	}
	return changed
}

func (c *CursorSettleState) ForceHidden(row, col int) (int, int, bool) {
	pos := [2]int{row, col}
	c.hasPending = false
	c.hasLastPos = false
	c.settledPos = pos
	c.settledVisible = false
	c.hasSettled = true
	return row, col, false
}

func (c *CursorSettleState) Observe(row, col int, visible bool, now time.Time) (settledRow, settledCol int, settledVis bool) {
	pos := [2]int{row, col}
	if !visible {
		c.hasLastPos = false
		if !c.hasSettled {
			c.settledPos = pos
			c.settledVisible = false
			c.hasSettled = true
			return row, col, false
		}
		if c.settledVisible {
			// Hold the settled visible cursor briefly so transient ?25l hides during redraws don't flicker.
			if !c.hasPending || c.pendingVisible {
				c.pendingPos = pos
				c.pendingVisible = false
				c.pendingDeadline = now.Add(cursorHideSettleDelay)
				c.holdStartedAt = now
				c.hasPending = true
			} else {
				c.pendingPos = pos
			}
			c.Advance(now)
			return c.settledPos[0], c.settledPos[1], c.settledVisible
		}
		// Already hidden: clear any stale pending visible candidate so Advance cannot resurrect it.
		c.hasPending = false
		c.settledPos = pos
		return c.settledPos[0], c.settledPos[1], false
	}

	if c.hasLastPos && c.lastPos[0] != pos[0] {
		c.lastRowChangeAt = now
		c.hasRowChange = true
	}
	c.lastPos = pos
	c.hasLastPos = true

	if !c.hasSettled {
		c.settledPos = pos
		c.settledVisible = true
		c.hasSettled = true
		c.preferredRow = pos[0]
		c.hasPreferredRow = true
		return pos[0], pos[1], true
	}

	if c.settledVisible && c.settledPos == pos {
		c.hasPending = false
		return pos[0], pos[1], true
	}

	// During active user input (including held-key repeat), settle cursor movement immediately with zero lag.
	if !c.inputGraceUntil.IsZero() && !now.After(c.inputGraceUntil) {
		c.settledPos = pos
		c.settledVisible = true
		c.hasSettled = true
		c.preferredRow = pos[0]
		c.hasPreferredRow = true
		c.hasPending = false
		c.hasRowChange = false
		return pos[0], pos[1], true
	}

	// Small horizontal steps on the preferred row settle immediately; large jumps still debounce.
	if c.hasPreferredRow && pos[0] == c.preferredRow && c.settledVisible {
		colDelta := pos[1] - c.settledPos[1]
		if colDelta < 0 {
			colDelta = -colDelta
		}
		if colDelta <= 4 {
			c.settledPos = pos
			c.settledVisible = true
			c.hasPending = false
			return pos[0], pos[1], true
		}
	}

	delay := cursorSettleDelay
	if c.hasRowChange && now.Sub(c.lastRowChangeAt) < cursorJumpHoldDelay {
		delay = cursorJumpHoldDelay
	}
	if !c.hasPending {
		c.pendingPos = pos
		c.pendingVisible = true
		c.pendingDeadline = now.Add(delay)
		c.holdStartedAt = now
		c.hasPending = true
	} else {
		c.pendingPos = pos
		c.pendingVisible = true
		// Cap total hold duration so continuous 10-50ms cursor movement still settles periodically.
		maxDeadline := c.holdStartedAt.Add(cursorJumpHoldDelay)
		nextDeadline := now.Add(delay)
		if nextDeadline.After(maxDeadline) {
			nextDeadline = maxDeadline
		}
		c.pendingDeadline = nextDeadline
	}
	c.Advance(now)
	return c.settledPos[0], c.settledPos[1], c.settledVisible
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

type vtermScrollMetrics struct {
	TotalRows    int
	ScrollOffset int
	ViewportRows int
}

// vterm implements an authoritative virtual terminal state machine modeled after
// herdr's GhosttyPaneTerminal. It maintains primary & alternate screens, a 10,000-line
// scrollback buffer, full SGR & wide-character layout, and in-place scrollback recovery.
type vterm struct {
	primary       vtermScreen
	alt           vtermScreen
	inAltScreen   bool
	style         vtermStyle
	scrollback    []vtermRow
	savedScroll   []vtermRow
	inClearDump   bool
	maxScrollback int
	scrollOffset  int

	// Rows pushed to scrollback since the last BlitEncoder consume.
	pushedScrollback       []vtermRow
	fullScrollbackReplaced bool

	// Snapshot of primary screen taken before an upward cursor jump / in-place redraw
	// so scrolled-off top lines can be recovered even when the TUI redraws in-place.
	preRedrawGrid        []vtermRow
	hasPreRedraw         bool
	sawUpwardRedraw      bool
	wroteSincePreRedraw  bool

	// Terminal modes forwarded to attached clients.
	appCursorKeys   bool // ?1
	appKeypad       bool // DECKPAM (ESC =) / DECKPNM (ESC >)
	insertMode      bool // IRM (CSI 4h / 4l)
	newlineMode     bool // LNM (CSI 20h / 20l)
	bracketedPaste  bool // ?2004
	focusEvents     bool // ?1004
	mouseMode       int  // 0, 1000, 1002, 1003
	mouseSGR        bool // ?1006
	syncOutput      bool // ?2026
	modifyOtherKeys int

	// Character sets (G0 / G1: 'B' = ASCII, '0' = DEC Special Graphics)
	g0Charset     byte
	g1Charset     byte
	activeCharset int // 0 = G0 (SI), 1 = G1 (SO)

	// Metadata & passthrough
	title                 string
	cwd                   string
	agentState            string
	sawOSC7501            bool
	sawWorkingSinceEncode bool
	lastSpinnerAt         time.Time
	progressSeq           string
	lastDECRPM           string
	lastPrintedCh        rune
	lastPrintedCombining string
	lastPrintedWidth     uint8
	pendingPassthrough   bytes.Buffer
	pendingReplies       bytes.Buffer
	sawServerVTerm       bool
	sawSnapshotEnd       bool
	inSnapshot           bool
	preSnapRows          int
	preSnapCols          int

	// VT parser state
	state       vtermParserState
	escInterm   []byte
	csiPrefix   byte
	csiParams   bytes.Buffer
	csiInterm   []byte
	csiCorrupt  bool
	oscBuf      bytes.Buffer
	oscOverflow bool
	dcsBuf      bytes.Buffer
	utf8Buf     [4]byte
	utf8Need    int
	utf8Len     int
	oscEscSeen  bool

	cursorSettle CursorSettleState
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
		cursorSettle:  newCursorSettleState(),
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

func (vt *vterm) InAltScreen() bool {
	return vt.inAltScreen
}

func (vt *vterm) trimScrollbackSlice(rows []vtermRow) []vtermRow {
	if len(rows) > vt.maxScrollback {
		return rows[len(rows)-vt.maxScrollback:]
	}
	return rows
}

func (vt *vterm) finalizeClearDump() {
	if !vt.inClearDump {
		return
	}
	vt.inClearDump = false
	if len(vt.savedScroll) == 0 {
		if len(vt.scrollback) > 0 {
			vt.fullScrollbackReplaced = true
		}
		return
	}
	if len(vt.scrollback) > 0 {
		// A redraw after \x1b[3J that scrolled lines into scrollback is a history re-dump
		// (e.g. agy/claude on SIGWINCH); replace savedScroll cleanly without duplication.
		vt.savedScroll = nil
		vt.fullScrollbackReplaced = true
		if vt.scrollOffset > len(vt.scrollback) {
			vt.scrollOffset = len(vt.scrollback)
		}
		return
	}
	// Redraw after \x1b[3J did not re-dump history; preserve savedScroll.
	vt.scrollback = vt.trimScrollbackSlice(vt.savedScroll)
	vt.savedScroll = nil
	if vt.scrollOffset > len(vt.scrollback) {
		vt.scrollOffset = len(vt.scrollback)
	}
}

func (vt *vterm) effectiveScrollback() []vtermRow {
	sb := vt.trimScrollbackSlice(vt.scrollback)
	if len(vt.savedScroll) == 0 {
		return sb
	}
	saved := vt.trimScrollbackSlice(vt.savedScroll)
	if len(sb) > 0 && len(sb) >= max(1, len(saved)/2) {
		return sb
	}
	if len(sb) == 0 {
		return saved
	}
	combined := make([]vtermRow, 0, len(saved)+len(sb))
	combined = append(combined, saved...)
	combined = append(combined, sb...)
	return vt.trimScrollbackSlice(combined)
}

func (vt *vterm) ConsumeFullScrollbackReplaced() bool {
	if vt.inClearDump && len(vt.scrollback) > 0 && (len(vt.savedScroll) == 0 || len(vt.scrollback) >= max(1, len(vt.savedScroll)/2)) {
		vt.finalizeClearDump()
	} else if !vt.inClearDump && len(vt.savedScroll) > 0 {
		vt.finalizeClearDump()
	}
	v := vt.fullScrollbackReplaced
	vt.fullScrollbackReplaced = false
	return v
}

func (vt *vterm) ScrollbackLen() int {
	return len(vt.effectiveScrollback())
}

func (vt *vterm) ScrollOffset() int {
	if vt.inAltScreen {
		return 0
	}
	sbLen := vt.ScrollbackLen()
	if vt.scrollOffset > sbLen {
		return sbLen
	}
	return vt.scrollOffset
}

func (vt *vterm) ScrollMetrics() vtermScrollMetrics {
	scr := vt.activeScreen()
	if vt.inAltScreen {
		return vtermScrollMetrics{
			TotalRows:    scr.rows,
			ScrollOffset: 0,
			ViewportRows: scr.rows,
		}
	}
	sbLen := vt.ScrollbackLen()
	off := vt.scrollOffset
	if off > sbLen {
		off = sbLen
	}
	return vtermScrollMetrics{
		TotalRows:    sbLen + scr.rows,
		ScrollOffset: off,
		ViewportRows: scr.rows,
	}
}

func (vt *vterm) IsScrolledBack() bool {
	return !vt.inAltScreen && vt.ScrollOffset() > 0
}

func (vt *vterm) ScrollUp(lines int) bool {
	sbLen := vt.ScrollbackLen()
	if vt.inAltScreen || sbLen == 0 {
		return false
	}
	if lines < 1 {
		lines = 1
	}
	prev := vt.ScrollOffset()
	vt.scrollOffset = prev + lines
	if vt.scrollOffset > sbLen {
		vt.scrollOffset = sbLen
	}
	return vt.scrollOffset != prev
}

func (vt *vterm) ScrollDown(lines int) bool {
	if vt.inAltScreen || vt.scrollOffset == 0 {
		return false
	}
	if lines < 1 {
		lines = 1
	}
	prev := vt.ScrollOffset()
	vt.scrollOffset = prev - lines
	if vt.scrollOffset < 0 {
		vt.scrollOffset = 0
	}
	return vt.scrollOffset != prev
}

func (vt *vterm) ScrollReset() bool {
	if vt.scrollOffset == 0 {
		return false
	}
	vt.scrollOffset = 0
	return true
}

func (vt *vterm) DrainPushedScrollback() []vtermRow {
	if len(vt.pushedScrollback) == 0 {
		return nil
	}
	out := vt.trimScrollbackSlice(vt.pushedScrollback)
	vt.pushedScrollback = nil
	return out
}

func (vt *vterm) DrainPassthrough() []byte {
	if vt.pendingPassthrough.Len() == 0 {
		return nil
	}
	out := append([]byte(nil), vt.pendingPassthrough.Bytes()...)
	vt.pendingPassthrough.Reset()
	return out
}

func (vt *vterm) DrainReplies() []byte {
	if vt.pendingReplies.Len() == 0 {
		return nil
	}
	out := append([]byte(nil), vt.pendingReplies.Bytes()...)
	vt.pendingReplies.Reset()
	return out
}

func (vt *vterm) SawServerVTerm() bool {
	return vt.sawServerVTerm
}

func (vt *vterm) SawServerQueries() bool {
	return vt.sawSnapshotEnd
}

func (vt *vterm) appendScrollbackRow(row vtermRow) {
	cloned := row.clone()
	cloned.dirty = false
	vt.scrollback = append(vt.scrollback, cloned)
	if len(vt.scrollback) >= vt.maxScrollback+1024 {
		drop := len(vt.scrollback) - vt.maxScrollback
		vt.scrollback = append([]vtermRow(nil), vt.scrollback[drop:]...)
	}
	vt.pushedScrollback = append(vt.pushedScrollback, cloned)
	if len(vt.pushedScrollback) >= vt.maxScrollback+1024 {
		drop := len(vt.pushedScrollback) - vt.maxScrollback
		vt.pushedScrollback = append([]vtermRow(nil), vt.pushedScrollback[drop:]...)
	}
	if vt.inClearDump && len(vt.savedScroll) > 0 && len(vt.scrollback) >= max(1, len(vt.savedScroll)/2) {
		vt.savedScroll = nil
		vt.inClearDump = false
		vt.fullScrollbackReplaced = true
	}
	if vt.scrollOffset > 0 {
		vt.scrollOffset++
		maxOff := vt.ScrollbackLen()
		if vt.scrollOffset > maxOff {
			vt.scrollOffset = maxOff
		}
	}
}

func (vt *vterm) snapshotBeforeUpwardRedraw() {
	if vt.inAltScreen || vt.hasPreRedraw {
		return
	}
	vt.preRedrawGrid = make([]vtermRow, len(vt.primary.grid))
	for i := range vt.primary.grid {
		vt.preRedrawGrid[i] = vt.primary.grid[i].clone()
	}
	vt.hasPreRedraw = true
	vt.wroteSincePreRedraw = false
}

// commitInPlaceScrollback checks whether an in-place full-screen redraw shifted
// the visible primary screen upward by k rows without emitting bottom linefeeds,
// and if so captures the k scrolled-off top rows into scrollback.
func (vt *vterm) commitInPlaceScrollback() {
	if !vt.hasPreRedraw {
		return
	}
	oldGrid := vt.preRedrawGrid
	vt.preRedrawGrid = nil
	vt.hasPreRedraw = false
	vt.sawUpwardRedraw = false
	vt.wroteSincePreRedraw = false

	if vt.inAltScreen || len(oldGrid) != len(vt.primary.grid) {
		return
	}
	rows := len(oldGrid)
	if rows < 4 {
		return
	}

	oldText := make([]string, rows)
	newText := make([]string, rows)
	anyChanged := false
	for i := 0; i < rows; i++ {
		oldText[i] = oldGrid[i].plainText()
		newText[i] = vt.primary.grid[i].plainText()
		if oldText[i] != newText[i] {
			anyChanged = true
		}
	}
	if !anyChanged {
		return
	}

	// Find the first non-empty anchor row in newText (within the top half) and check
	// if a contiguous block of rows in oldText shifted upward by k >= 1 rows.
	bestShift := 0
	maxTop := rows / 2
	if maxTop > 12 {
		maxTop = 12
	}
	for dstStart := 0; dstStart < maxTop && bestShift == 0; dstStart++ {
		if len(strings.TrimSpace(newText[dstStart])) < 2 {
			continue
		}
		for k := 1; dstStart+k < rows-1; k++ {
			if oldText[dstStart+k] != newText[dstStart] {
				continue
			}
			// Verify at least 2 non-empty lines and 3 total lines match at this shift k,
			// and at least one matched row actually changed relative to k=0.
			matched := 0
			substantive := 0
			changedInWindow := false
			for r := dstStart; r+k < rows && r < dstStart+10; r++ {
				if oldText[r+k] == newText[r] {
					matched++
					if len(strings.TrimSpace(newText[r])) >= 2 {
						substantive++
					}
					if oldText[r] != newText[r] {
						changedInWindow = true
					}
				} else {
					break
				}
			}
			if substantive >= 2 && matched >= 3 && changedInWindow {
				bestShift = k
				for r := dstStart; r < dstStart+k; r++ {
					txt := oldText[r]
					if txt == "" && len(vt.scrollback) == 0 {
						continue
					}
					vt.appendScrollbackRow(oldGrid[r])
				}
				break
			}
		}
	}
}

// Write feeds raw PTY output bytes into the virtual terminal state machine.
func (vt *vterm) Write(data []byte) {
	for i := 0; i < len(data); i++ {
		b := data[i]
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
				continue
			}
			// Invalid continuation byte: emit replacement character and re-process b.
			vt.utf8Need = 0
			vt.utf8Len = 0
			vt.putRune(utf8.RuneError)
		}

		// CAN (0x18) and SUB (0x1a) immediately cancel any active escape/control sequence.
		if b == 0x18 || b == 0x1a {
			vt.state = vtStateGround
			vt.oscEscSeen = false
			vt.csiCorrupt = false
			vt.oscOverflow = false
			continue
		}

		switch vt.state {
		case vtStateOSC:
			vt.stepOSC(b)
			continue
		case vtStateDCS:
			vt.stepDCS(b)
			continue
		}

		// C0 controls can appear inside ESC/CSI sequences.
		if b < 0x20 || b == 0x7f {
			if b == 0x1b {
				vt.state = vtStateEsc
				vt.escInterm = vt.escInterm[:0]
				continue
			}
			vt.handleControl(b)
			continue
		}

		switch vt.state {
		case vtStateGround:
			if b < 0x80 {
				vt.putRune(rune(b))
			} else if b >= 0xC2 && b <= 0xDF {
				vt.utf8Buf[0] = b
				vt.utf8Len = 1
				vt.utf8Need = 2
			} else if b >= 0xE0 && b <= 0xEF {
				vt.utf8Buf[0] = b
				vt.utf8Len = 1
				vt.utf8Need = 3
			} else if b >= 0xF0 && b <= 0xF4 {
				vt.utf8Buf[0] = b
				vt.utf8Len = 1
				vt.utf8Need = 4
			} else {
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

	if !vt.syncOutput {
		if vt.sawUpwardRedraw && vt.wroteSincePreRedraw && vt.primary.curR >= 2 {
			vt.commitInPlaceScrollback()
		}
	}
}

func (vt *vterm) handleControl(b byte) {
	scr := vt.activeScreen()
	switch b {
	case 0x07: // BEL
		vt.pendingPassthrough.WriteByte(0x07)
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
	case 0x0e: // SO (Shift Out -> G1 charset)
		vt.activeCharset = 1
	case 0x0f: // SI (Shift In -> G0 charset)
		vt.activeCharset = 0
	}
}

func (vt *vterm) advanceTabForward(scr *vtermScreen, n int) {
	if n < 1 {
		n = 1
	}
	for step := 0; step < n; step++ {
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
	if n < 1 {
		n = 1
	}
	for step := 0; step < n; step++ {
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
	if top < 0 {
		top = 0
	}
	if bottom >= scr.rows {
		bottom = scr.rows - 1
	}
	if top > bottom || n <= 0 {
		return
	}
	regionHeight := bottom - top + 1
	if n > regionHeight {
		n = regionHeight
	}
	if recordScrollback {
		vt.hasPreRedraw = false
		vt.sawUpwardRedraw = false
		vt.wroteSincePreRedraw = false
		for i := 0; i < n; i++ {
			vt.appendScrollbackRow(scr.grid[top+i])
		}
	}
	for r := top; r <= bottom-n; r++ {
		scr.grid[r] = scr.grid[r+n]
		scr.grid[r].dirty = true
	}
	for r := bottom - n + 1; r <= bottom; r++ {
		scr.grid[r] = newVTermRow(scr.cols, vt.style)
	}
}

func (vt *vterm) scrollDownRegion(scr *vtermScreen, top, bottom, n int) {
	if top < 0 {
		top = 0
	}
	if bottom >= scr.rows {
		bottom = scr.rows - 1
	}
	if top > bottom || n <= 0 {
		return
	}
	regionHeight := bottom - top + 1
	if n > regionHeight {
		n = regionHeight
	}
	for r := bottom; r >= top+n; r-- {
		scr.grid[r] = scr.grid[r-n]
		scr.grid[r].dirty = true
	}
	for r := top; r < top+n; r++ {
		scr.grid[r] = newVTermRow(scr.cols, vt.style)
	}
}

// clearWideOverlap clears the partner cell of a wide character intersected at (row, col)
// using the current SGR erase background style (BCE).
func (vt *vterm) clearWideOverlap(scr *vtermScreen, row, col int) {
	if row < 0 || row >= scr.rows || col < 0 || col >= scr.cols {
		return
	}
	cells := scr.grid[row].cells
	c := cells[col]
	blank := blankVTermCell(vt.style)
	if c.width == 2 && col+1 < scr.cols {
		cells[col+1] = blank
	} else if c.width == 0 && col > 0 {
		cells[col-1] = blank
	}
}

// clearWidePairAt clears both halves of a wide character if col is either half.
func (vt *vterm) clearWidePairAt(scr *vtermScreen, row, col int) {
	if row < 0 || row >= scr.rows || col < 0 || col >= scr.cols {
		return
	}
	cells := scr.grid[row].cells
	c := cells[col]
	blank := blankVTermCell(vt.style)
	if c.width == 2 {
		cells[col] = blank
		if col+1 < scr.cols {
			cells[col+1] = blank
		}
	} else if c.width == 0 {
		cells[col] = blank
		if col > 0 {
			cells[col-1] = blank
		}
	}
}

func mapDECSpecialGraphics(r rune) rune {
	switch r {
	case '`':
		return '◆'
	case 'a':
		return '▒'
	case 'b':
		return '␉'
	case 'c':
		return '␌'
	case 'd':
		return '␍'
	case 'e':
		return '␊'
	case 'f':
		return '°'
	case 'g':
		return '±'
	case 'h':
		return '␤'
	case 'i':
		return '␋'
	case 'j':
		return '┘'
	case 'k':
		return '┐'
	case 'l':
		return '┌'
	case 'm':
		return '└'
	case 'n':
		return '┼'
	case 'o':
		return '⎺'
	case 'p':
		return '⎻'
	case 'q':
		return '─'
	case 'r':
		return '⎼'
	case 's':
		return '⎽'
	case 't':
		return '├'
	case 'u':
		return '┤'
	case 'v':
		return '┴'
	case 'w':
		return '┬'
	case 'x':
		return '│'
	case 'y':
		return '≤'
	case 'z':
		return '≥'
	case '{':
		return 'π'
	case '|':
		return '≠'
	case '}':
		return '£'
	case '~':
		return '·'
	default:
		return r
	}
}

func (vt *vterm) insertBlankCharsAt(scr *vtermScreen, row, col, n int) {
	if row < 0 || row >= scr.rows || col < 0 || col >= scr.cols || n <= 0 {
		return
	}
	if n > scr.cols-col {
		n = scr.cols - col
	}
	cells := scr.grid[row].cells
	// 1. If insertion point bisects a wide character (col is the right half), clear both halves.
	if cells[col].width == 0 {
		vt.clearWidePairAt(scr, row, col)
	}
	// 2. If the shifted region's right edge cuts a wide leader at cols-n-1, clear that leader.
	if scr.cols-n-1 >= col && cells[scr.cols-n-1].width == 2 {
		vt.clearWidePairAt(scr, row, scr.cols-n-1)
	}
	copy(cells[col+n:], cells[col:scr.cols-n])
	blank := blankVTermCell(vt.style)
	for c := col; c < col+n; c++ {
		cells[c] = blank
	}
	scr.grid[row].dirty = true
}

func (vt *vterm) deleteCharsAt(scr *vtermScreen, row, col, n int) {
	if row < 0 || row >= scr.rows || col < 0 || col >= scr.cols || n <= 0 {
		return
	}
	if n > scr.cols-col {
		n = scr.cols - col
	}
	cells := scr.grid[row].cells
	// 1. If col bisects a wide character (col is the right half), clear its leader at col-1.
	if cells[col].width == 0 {
		vt.clearWidePairAt(scr, row, col)
	}
	// 2. If col+n bisects a wide character (col+n is the right half), clear both halves
	// so an orphan continuation cell is never shifted into col.
	if col+n < scr.cols && cells[col+n].width == 0 {
		vt.clearWidePairAt(scr, row, col+n)
	}
	copy(cells[col:], cells[col+n:])
	blank := blankVTermCell(vt.style)
	for c := scr.cols - n; c < scr.cols; c++ {
		cells[c] = blank
	}
	scr.grid[row].dirty = true
}

func (vt *vterm) putRune(r rune) {
	scr := vt.activeScreen()
	charset := vt.g0Charset
	if vt.activeCharset == 1 {
		charset = vt.g1Charset
	}
	if charset == '0' && r >= 0x5f && r <= 0x7e {
		r = mapDECSpecialGraphics(r)
	}

	w := singleRuneWidth(r)
	if w <= 0 {
		if r < 0x20 {
			return
		}
		// Zero-width combining/variation character: attach to previous cell on this row.
		if scr.curR >= 0 && scr.curR < scr.rows {
			targetC := scr.curC - 1
			if scr.pendingWrap {
				targetC = scr.cols - 1
			}
			if targetC >= 0 && targetC < scr.cols {
				if scr.grid[scr.curR].cells[targetC].width == 0 && targetC > 0 {
					targetC--
				}
				// Variation selector-16 (U+FE0F) widens certain narrow emoji symbols if space permits.
				if len(scr.grid[scr.curR].cells[targetC].combining)+utf8.RuneLen(r) <= maxCombiningBytes {
					scr.grid[scr.curR].cells[targetC].combining += string(r)
					scr.grid[scr.curR].dirty = true
					if vt.lastPrintedCh != 0 {
						vt.lastPrintedCombining = scr.grid[scr.curR].cells[targetC].combining
					}
				}
			}
		}
		return
	}

	if scr.curR < 0 {
		scr.curR = 0
	} else if scr.curR >= scr.rows {
		scr.curR = scr.rows - 1
	}
	if scr.curC < 0 {
		scr.curC = 0
	} else if scr.curC >= scr.cols {
		scr.curC = scr.cols - 1
	}

	// If inserting a 2-column glyph on a 1-column screen, it cannot fit.
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
		if scr.autoWrap {
			vt.clearWideOverlap(scr, scr.curR, scr.curC)
			scr.grid[scr.curR].cells[scr.curC] = blankVTermCell(vt.style)
			scr.grid[scr.curR].wrapped = true
			scr.grid[scr.curR].dirty = true
			scr.curC = 0
			vt.lineFeed()
		} else {
			// When autowrap is disabled (?7l), a wide glyph at the final column cannot fit.
			return
		}
	}

	if vt.hasPreRedraw {
		vt.wroteSincePreRedraw = true
	}

	if vt.insertMode {
		vt.insertBlankCharsAt(scr, scr.curR, scr.curC, w)
	}

	vt.clearWideOverlap(scr, scr.curR, scr.curC)
	if w == 2 && scr.curC+1 < scr.cols {
		vt.clearWideOverlap(scr, scr.curR, scr.curC+1)
		scr.grid[scr.curR].cells[scr.curC] = vtermCell{
			ch:    r,
			width: 2,
			style: vt.style,
		}
		scr.grid[scr.curR].cells[scr.curC+1] = vtermCell{
			ch:    0,
			width: 0,
			style: vt.style,
		}
		scr.grid[scr.curR].dirty = true
		if scr.curC+2 >= scr.cols {
			scr.curC = scr.cols - 1
			scr.pendingWrap = scr.autoWrap
		} else {
			scr.curC += 2
		}
	} else {
		scr.grid[scr.curR].cells[scr.curC] = vtermCell{
			ch:    r,
			width: 1,
			style: vt.style,
		}
		scr.grid[scr.curR].dirty = true
		if scr.curC+1 >= scr.cols {
			scr.curC = scr.cols - 1
			scr.pendingWrap = scr.autoWrap
		} else {
			scr.curC++
		}
	}

	vt.lastPrintedCh = r
	vt.lastPrintedCombining = ""
	vt.lastPrintedWidth = uint8(w)
}

func (vt *vterm) stepEsc(b byte) {
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
		vt.dcsBuf.Reset()
		vt.oscEscSeen = false
	case '7': // DECSC
		vt.saveCursor()
		vt.state = vtStateGround
	case '8': // DECRC
		vt.restoreCursor()
		vt.state = vtStateGround
	case 'D': // IND
		scr := vt.activeScreen()
		scr.pendingWrap = false
		vt.lineFeed()
		vt.state = vtStateGround
	case 'M': // RI
		vt.reverseIndex()
		vt.state = vtStateGround
	case 'E': // NEL
		scr := vt.activeScreen()
		scr.pendingWrap = false
		scr.curC = 0
		vt.lineFeed()
		vt.state = vtStateGround
	case 'H': // HTS - Horizontal Tab Set
		scr := vt.activeScreen()
		if scr.curC >= 0 && scr.curC < len(scr.tabs) {
			scr.tabs[scr.curC] = true
		}
		vt.state = vtStateGround
	case 'c': // RIS
		vt.resetFull()
		vt.state = vtStateGround
	case '=': // DECKPAM
		vt.appKeypad = true
		vt.state = vtStateGround
	case '>': // DECKPNM
		vt.appKeypad = false
		vt.state = vtStateGround
	default:
		if b >= 0x20 && b <= 0x2f {
			if len(vt.escInterm) < maxIntermLen {
				vt.escInterm = append(vt.escInterm, b)
			}
			vt.state = vtStateEscIntermediate
		} else {
			vt.state = vtStateGround
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
	if b >= 0x30 && b <= 0x7e {
		if len(vt.escInterm) == 1 {
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
				if b == '8' { // DECALN - Screen Alignment Pattern
					scr := vt.activeScreen()
					scr.scrollTop = 0
					scr.scrollBottom = scr.rows - 1
					scr.originMode = false
					scr.pendingWrap = false
					scr.curR = 0
					scr.curC = 0
					var defStyle vtermStyle
					for r := 0; r < scr.rows; r++ {
						for c := 0; c < scr.cols; c++ {
							scr.grid[r].cells[c] = vtermCell{ch: 'E', width: 1, style: defStyle}
						}
						scr.grid[r].wrapped = false
						scr.grid[r].dirty = true
					}
				}
			}
		}
		vt.state = vtStateGround
	}
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
		// Any other ESC sequence inside OSC aborts the unterminated OSC and starts the new ESC sequence.
		vt.state = vtStateEsc
		vt.escInterm = vt.escInterm[:0]
		vt.stepEsc(b)
		return
	}
	if b == 0x07 {
		if !vt.oscOverflow {
			vt.dispatchOSC(vt.oscBuf.String())
		}
		vt.state = vtStateGround
		return
	}
	if b == 0x1b {
		vt.oscEscSeen = true
		return
	}
	if vt.oscBuf.Len() < maxOSCBufLen {
		vt.oscBuf.WriteByte(b)
	} else {
		vt.oscOverflow = true
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
		return
	}
	if vt.dcsBuf.Len() < maxOSCBufLen {
		vt.dcsBuf.WriteByte(b)
	}
}

func (vt *vterm) dispatchOSC(payload string) {
	semi := strings.IndexByte(payload, ';')
	if semi < 0 {
		return
	}
	code := payload[:semi]
	rest := payload[semi+1:]
	switch code {
	case "0", "2":
		vt.title = strings.TrimSpace(rest)
	case "7":
		vt.cwd = strings.TrimSpace(rest)
	case "8":
		// OSC 8 ; params ; URI ST
		semi2 := strings.IndexByte(rest, ';')
		if semi2 >= 0 {
			vt.style.hyperlink = rest[semi2+1:]
		}
	case "9":
		// ConEmu / Ghostty progress: 9;4;state;progress
		if strings.HasPrefix(rest, "4;") {
			vt.progressSeq = rest
		}
	case "52":
		// Forward complete OSC 52 clipboard sequence to the attached terminal.
		vt.pendingPassthrough.WriteString("\x1b]52;")
		vt.pendingPassthrough.WriteString(rest)
		vt.pendingPassthrough.WriteByte(0x07)
	case "21337":
		if !vt.inSnapshot {
			vt.pendingPassthrough.WriteString("\x1b]21337;")
			vt.pendingPassthrough.WriteString(rest)
			vt.pendingPassthrough.WriteByte(0x07)
		}
	case "7501":
		s := strings.TrimSpace(rest)
		if s == "?" || strings.HasPrefix(s, "?") {
			vt.pendingReplies.WriteString("\x1b]7501;?\x1b\\")
			return
		}
		vt.sawOSC7501 = true
		vt.lastSpinnerAt = time.Time{}
		switch {
		case strings.Contains(s, "state=working"):
			vt.sawWorkingSinceEncode = true
			vt.agentState = s
		case strings.Contains(s, "state=done"):
			vt.agentState = strings.Replace(s, "state=done", "state=idle", 1)
		case strings.Contains(s, "state=clear"):
			vt.agentState = ""
		default:
			vt.agentState = s
		}
	case "7502":
		vt.handleInternalOSC7502(rest)
	}
}

func (vt *vterm) handleInternalOSC7502(rest string) {
	var snapRows, snapCols int
	sawBegin := false
	sawEnd := false
	for _, field := range strings.Split(rest, ";") {
		if field == "vterm=1" {
			vt.sawServerVTerm = true
		} else if field == "snap=begin" {
			sawBegin = true
		} else if field == "snap=end" {
			sawEnd = true
			vt.sawSnapshotEnd = true
		} else if v, ok := strings.CutPrefix(field, "rows="); ok {
			snapRows, _ = strconv.Atoi(v)
		} else if v, ok := strings.CutPrefix(field, "cols="); ok {
			snapCols, _ = strconv.Atoi(v)
		} else if v, ok := strings.CutPrefix(field, "sbrow="); ok {
			if raw, err := base64.StdEncoding.DecodeString(v); err == nil {
				row := vt.decodeSnapshotRow(raw, vt.primary.cols)
				vt.appendScrollbackRow(row)
			}
		} else if v, ok := strings.CutPrefix(field, "wrap="); ok {
			scr := vt.activeScreen()
			for _, idxStr := range strings.Split(v, ",") {
				if r, err := strconv.Atoi(idxStr); err == nil && r >= 0 && r < len(scr.grid) {
					scr.grid[r].wrapped = true
				}
			}
		} else if field == "pw=1" {
			scr := vt.activeScreen()
			if scr.curC == scr.cols-1 && scr.autoWrap {
				scr.pendingWrap = true
			}
		}
	}
	if sawBegin && snapRows > 0 && snapCols > 0 {
		vt.inSnapshot = true
		vt.preSnapRows = vt.primary.rows
		vt.preSnapCols = vt.primary.cols
		vt.Resize(snapRows, snapCols)
	}
	if sawEnd && vt.inSnapshot {
		vt.inSnapshot = false
		if vt.preSnapRows > 0 && vt.preSnapCols > 0 {
			vt.Resize(vt.preSnapRows, vt.preSnapCols)
		}
	}
}

func (vt *vterm) decodeSnapshotRow(raw []byte, cols int) vtermRow {
	tmp := newVTerm(1, max(cols, 1))
	tmp.primary.autoWrap = false
	wrapped := false
	if bytes.HasPrefix(raw, []byte("W:")) {
		wrapped = true
		raw = raw[2:]
	}
	tmp.Write(raw)
	row := tmp.primary.grid[0]
	row.wrapped = wrapped
	row.dirty = false
	return row
}

func (vt *vterm) stepCSI(b byte) {
	if b >= 0x30 && b <= 0x3f {
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
		return
	}
	if b >= 0x20 && b <= 0x2f {
		if len(vt.csiInterm) < maxIntermLen {
			vt.csiInterm = append(vt.csiInterm, b)
		} else {
			vt.csiCorrupt = true
		}
		return
	}
	if b >= 0x40 && b <= 0x7e {
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
			out[i] = 0
			continue
		}
		v, err := strconv.Atoi(p)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(p, "-") {
				out[i] = maxCSIParamValue
			}
			continue
		}
		if v < 0 {
			v = 0
		} else if v > maxCSIParamValue {
			v = maxCSIParamValue
		}
		out[i] = v
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
	if final == 'u' && len(interm) == 0 && (prefix == '>' || prefix == '<' || prefix == '=' || prefix == '?') {
		switch prefix {
		case '>':
			flags := uint8(csiParam(params, 0, 0))
			scr.pushKittyFlags(flags)
		case '<':
			n := csiParam(params, 0, 1)
			scr.popKittyFlags(n)
		case '=':
			flags := uint8(csiParam(params, 0, 0))
			mode := csiParam(params, 1, 1)
			scr.setKittyFlags(flags, mode)
		case '?':
			fmt.Fprintf(&vt.pendingReplies, "\x1b[?%du", scr.currentKittyFlags())
		}
		return
	}

	// DECSCUSR: CSI Ps SP q
	if final == 'q' && prefix == 0 && len(interm) == 1 && interm[0] == ' ' {
		shape := 0
		if len(params) > 0 {
			shape = params[0]
		}
		if shape >= 0 && shape <= 6 {
			scr.cursorShape = uint8(shape)
		}
		return
	}

	// DECSTR (Soft Terminal Reset): CSI ! p
	if final == 'p' && prefix == 0 && len(interm) == 1 && interm[0] == '!' {
		vt.softReset()
		return
	}

	// DECRQM (Request DEC Private Mode): CSI ? Ps $ p
	if final == 'p' && prefix == '?' && len(interm) == 1 && interm[0] == '$' {
		if len(params) > 0 {
			mode := params[0]
			pm := vt.queryDECPrivateMode(mode)
			fmt.Fprintf(&vt.pendingReplies, "\x1b[?%d;%d$y", mode, pm)
		}
		return
	}

	// DECRPM (Report DEC Private Mode): CSI ? Ps ; Pm $ y
	if final == 'y' && prefix == '?' && len(interm) == 1 && interm[0] == '$' {
		vt.lastDECRPM = fmt.Sprintf("\x1b[?%s$y", rawParams)
		return
	}

	// DA2 (Secondary Device Attributes): CSI > c
	if final == 'c' && prefix == '>' && len(interm) == 0 {
		if len(params) == 0 || params[0] == 0 {
			vt.pendingReplies.WriteString("\x1b[>1;10;0c")
		}
		return
	}

	// modifyOtherKeys: CSI > 4 ; Pv m or CSI > 4 n
	if final == 'm' && prefix == '>' && len(interm) == 0 {
		if len(params) == 0 {
			vt.modifyOtherKeys = 0
		} else if params[0] == 4 {
			if len(params) >= 2 {
				vt.modifyOtherKeys = clampInt(params[1], 0, 2)
			} else {
				vt.modifyOtherKeys = 0
			}
		}
		return
	}
	if final == 'n' && prefix == '>' && len(interm) == 0 {
		if len(params) == 1 && params[0] == 4 {
			vt.modifyOtherKeys = 0
		}
		return
	}

	if prefix == '?' {
		if len(interm) == 0 {
			vt.dispatchDECPrivate(params, final)
		}
		return
	}
	if prefix != 0 || len(interm) != 0 {
		return
	}

	// Non-SGR standard CSI commands do not use colon subparameters.
	if final != 'm' && strings.IndexByte(rawParams, ':') >= 0 {
		return
	}

	switch final {
	case 'A': // CUU - Cursor Up
		n := csiParam(params, 0, 1)
		if !vt.inAltScreen && n >= 2 {
			vt.snapshotBeforeUpwardRedraw()
			vt.sawUpwardRedraw = true
		}
		scr.pendingWrap = false
		minRow := 0
		if scr.originMode || scr.curR >= scr.scrollTop {
			minRow = scr.scrollTop
		}
		scr.curR -= n
		if scr.curR < minRow {
			scr.curR = minRow
		}
	case 'B', 'e': // CUD - Cursor Down
		n := csiParam(params, 0, 1)
		scr.pendingWrap = false
		maxRow := scr.rows - 1
		if scr.originMode || scr.curR <= scr.scrollBottom {
			maxRow = scr.scrollBottom
		}
		scr.curR += n
		if scr.curR > maxRow {
			scr.curR = maxRow
		}
	case 'C', 'a': // CUF - Cursor Forward
		n := csiParam(params, 0, 1)
		scr.pendingWrap = false
		scr.curC += n
		if scr.curC >= scr.cols {
			scr.curC = scr.cols - 1
		}
	case 'D': // CUB - Cursor Backward
		n := csiParam(params, 0, 1)
		scr.pendingWrap = false
		scr.curC -= n
		if scr.curC < 0 {
			scr.curC = 0
		}
	case 'E': // CNL - Cursor Next Line
		n := csiParam(params, 0, 1)
		scr.pendingWrap = false
		scr.curC = 0
		maxRow := scr.rows - 1
		if scr.originMode || scr.curR <= scr.scrollBottom {
			maxRow = scr.scrollBottom
		}
		scr.curR += n
		if scr.curR > maxRow {
			scr.curR = maxRow
		}
	case 'F': // CPL - Cursor Preceding Line
		n := csiParam(params, 0, 1)
		if !vt.inAltScreen && n >= 2 {
			vt.snapshotBeforeUpwardRedraw()
			vt.sawUpwardRedraw = true
		}
		scr.pendingWrap = false
		scr.curC = 0
		minRow := 0
		if scr.originMode || scr.curR >= scr.scrollTop {
			minRow = scr.scrollTop
		}
		scr.curR -= n
		if scr.curR < minRow {
			scr.curR = minRow
		}
	case 'G', '`': // CHA - Cursor Character Absolute
		col := csiParam(params, 0, 1) - 1
		scr.pendingWrap = false
		if col < 0 {
			col = 0
		} else if col >= scr.cols {
			col = scr.cols - 1
		}
		scr.curC = col
	case 'd': // VPA - Line Position Absolute
		row := csiParam(params, 0, 1) - 1
		minRow, maxRow := 0, scr.rows-1
		if scr.originMode {
			row += scr.scrollTop
			minRow = scr.scrollTop
			maxRow = scr.scrollBottom
		}
		if !vt.inAltScreen && row < scr.curR-1 {
			vt.snapshotBeforeUpwardRedraw()
			vt.sawUpwardRedraw = true
		}
		scr.pendingWrap = false
		if row < minRow {
			row = minRow
		} else if row > maxRow {
			row = maxRow
		}
		scr.curR = row
	case 'H', 'f': // CUP / HVP - Cursor Position
		row := csiParam(params, 0, 1) - 1
		col := csiParam(params, 1, 1) - 1
		minRow, maxRow := 0, scr.rows-1
		if scr.originMode {
			row += scr.scrollTop
			minRow = scr.scrollTop
			maxRow = scr.scrollBottom
		}
		if !vt.inAltScreen && row < scr.curR-1 {
			vt.snapshotBeforeUpwardRedraw()
			vt.sawUpwardRedraw = true
		}
		scr.pendingWrap = false
		if row < minRow {
			row = minRow
		} else if row > maxRow {
			row = maxRow
		}
		if col < 0 {
			col = 0
		} else if col >= scr.cols {
			col = scr.cols - 1
		}
		scr.curR = row
		scr.curC = col
	case 'I': // CHT - Cursor Forward Tabulation
		n := csiParam(params, 0, 1)
		scr.pendingWrap = false
		vt.advanceTabForward(scr, n)
	case 'Z': // CBT - Cursor Backward Tabulation
		n := csiParam(params, 0, 1)
		scr.pendingWrap = false
		vt.advanceTabBackward(scr, n)
	case 'g': // TBC - Tab Clear
		mode := 0
		if len(params) > 0 {
			mode = params[0]
		}
		switch mode {
		case 0:
			if scr.curC >= 0 && scr.curC < len(scr.tabs) {
				scr.tabs[scr.curC] = false
			}
		case 3:
			for i := range scr.tabs {
				scr.tabs[i] = false
			}
		}
	case 'J': // ED - Erase in Display
		mode := 0
		if len(params) > 0 {
			mode = params[0]
		}
		vt.eraseInDisplay(scr, mode)
	case 'K': // EL - Erase in Line
		mode := 0
		if len(params) > 0 {
			mode = params[0]
		}
		vt.eraseInLine(scr, mode)
	case 'X': // ECH - Erase Characters
		n := csiParam(params, 0, 1)
		scr.pendingWrap = false
		blank := blankVTermCell(vt.style)
		endC := scr.curC + n
		if endC > scr.cols || endC < scr.curC {
			endC = scr.cols
		}
		for c := scr.curC; c < endC; c++ {
			vt.clearWideOverlap(scr, scr.curR, c)
			scr.grid[scr.curR].cells[c] = blank
		}
		scr.grid[scr.curR].dirty = true
	case 'L': // IL - Insert Lines
		n := csiParam(params, 0, 1)
		if scr.curR >= scr.scrollTop && scr.curR <= scr.scrollBottom {
			scr.pendingWrap = false
			scr.curC = 0
			vt.scrollDownRegion(scr, scr.curR, scr.scrollBottom, n)
		}
	case 'M': // DL - Delete Lines
		n := csiParam(params, 0, 1)
		if scr.curR >= scr.scrollTop && scr.curR <= scr.scrollBottom {
			scr.pendingWrap = false
			scr.curC = 0
			vt.scrollUpRegion(scr, scr.curR, scr.scrollBottom, n, false)
		}
	case '@': // ICH - Insert Characters
		n := csiParam(params, 0, 1)
		scr.pendingWrap = false
		vt.insertBlankCharsAt(scr, scr.curR, scr.curC, n)
	case 'P': // DCH - Delete Characters
		n := csiParam(params, 0, 1)
		scr.pendingWrap = false
		vt.deleteCharsAt(scr, scr.curR, scr.curC, n)
	case 'S': // SU - Scroll Up
		n := csiParam(params, 0, 1)
		vt.scrollUpRegion(scr, scr.scrollTop, scr.scrollBottom, n, !vt.inAltScreen && scr.scrollTop == 0)
	case 'T': // SD - Scroll Down
		n := csiParam(params, 0, 1)
		vt.scrollDownRegion(scr, scr.scrollTop, scr.scrollBottom, n)
	case 'b': // REP - Repeat preceding graphic character
		n := csiParam(params, 0, 1)
		if n > 4096 {
			n = 4096
		}
		if vt.lastPrintedCh != 0 {
			comb := vt.lastPrintedCombining
			for i := 0; i < n; i++ {
				vt.putRune(vt.lastPrintedCh)
				for _, cr := range comb {
					vt.putRune(cr)
				}
			}
		}
	case 'c': // DA1 - Primary Device Attributes
		if len(params) == 0 || params[0] == 0 {
			vt.pendingReplies.WriteString("\x1b[?62;22c")
		}
	case 'n': // DSR - Device Status Report
		if len(params) > 0 {
			switch params[0] {
			case 5:
				vt.pendingReplies.WriteString("\x1b[0n")
			case 6:
				repR := scr.curR + 1
				if scr.originMode {
					repR = (scr.curR - scr.scrollTop) + 1
					if repR < 1 {
						repR = 1
					}
				}
				fmt.Fprintf(&vt.pendingReplies, "\x1b[%d;%dR", repR, scr.curC+1)
			}
		}
	case 'h', 'l': // SM / RM - ANSI Set/Reset Mode
		set := final == 'h'
		for _, p := range params {
			switch p {
			case 4: // IRM - Insert Replacement Mode
				vt.insertMode = set
			case 20: // LNM - Line Feed / New Line Mode
				vt.newlineMode = set
			}
		}
	case 'r': // DECSTBM - Set Top and Bottom Margins
		top := csiParam(params, 0, 1) - 1
		bottom := csiParam(params, 1, scr.rows) - 1
		if top < 0 {
			top = 0
		}
		if bottom >= scr.rows {
			bottom = scr.rows - 1
		}
		if top < bottom {
			scr.scrollTop = top
			scr.scrollBottom = bottom
			scr.curC = 0
			if scr.originMode {
				scr.curR = top
			} else {
				scr.curR = 0
			}
			scr.pendingWrap = false
		}
	case 's': // SCOSC - Save Cursor
		if len(params) == 0 {
			vt.saveCursor()
		}
	case 'u': // SCORC - Restore Cursor
		if len(params) == 0 {
			vt.restoreCursor()
		}
	case 'm': // SGR
		vt.handleSGR(rawParams)
	}
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
		return 0 // not recognized
	}
	if set {
		return 1
	}
	return 2
}

func (vt *vterm) eraseInDisplay(scr *vtermScreen, mode int) {
	blank := blankVTermCell(vt.style)
	switch mode {
	case 0: // Cursor to end of screen
		scr.pendingWrap = false
		vt.eraseInLine(scr, 0)
		for r := scr.curR + 1; r < scr.rows; r++ {
			scr.grid[r] = newVTermRow(scr.cols, vt.style)
		}
	case 1: // Start of screen to cursor
		scr.pendingWrap = false
		for r := 0; r < scr.curR; r++ {
			scr.grid[r] = newVTermRow(scr.cols, vt.style)
		}
		vt.eraseInLine(scr, 1)
	case 2: // Entire screen
		scr.pendingWrap = false
		if !vt.inAltScreen {
			vt.snapshotBeforeUpwardRedraw()
			vt.sawUpwardRedraw = true
		}
		for r := 0; r < scr.rows; r++ {
			for c := 0; c < scr.cols; c++ {
				scr.grid[r].cells[c] = blank
			}
			scr.grid[r].wrapped = false
			scr.grid[r].dirty = true
		}
	case 3:
		// Smart \x1b[3J protection on the primary screen:
		// Save current scrollback into savedScroll before resetting scrollback.
		// If the subsequent redraw is a full-history dump (e.g. agy on SIGWINCH) that
		// pushes fresh scrollback lines, savedScroll is replaced cleanly without duplication.
		// Otherwise savedScroll is preserved so a bare \x1b[3J screen clear never destroys
		// user scrollback history. Note: ED 3 does NOT clear pendingWrap.
		if vt.inAltScreen {
			return
		}
		eff := vt.effectiveScrollback()
		if len(eff) > 0 {
			vt.savedScroll = make([]vtermRow, len(eff))
			copy(vt.savedScroll, eff)
		}
		vt.scrollback = nil
		vt.pushedScrollback = nil
		vt.inClearDump = true
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
	case 0: // Cursor to end of line
		if scr.curC >= 0 && scr.curC < scr.cols {
			vt.clearWideOverlap(scr, scr.curR, scr.curC)
			for c := scr.curC; c < scr.cols; c++ {
				row.cells[c] = blank
			}
		}
		row.wrapped = false
	case 1: // Start of line to cursor
		if scr.curC >= 0 && scr.curC < scr.cols {
			vt.clearWideOverlap(scr, scr.curR, scr.curC)
		}
		for c := 0; c <= scr.curC && c < scr.cols; c++ {
			row.cells[c] = blank
		}
	case 2: // Entire line
		for c := 0; c < scr.cols; c++ {
			row.cells[c] = blank
		}
		row.wrapped = false
	}
	row.dirty = true
}

func (vt *vterm) dispatchDECPrivate(params []int, final byte) {
	if final == 'n' {
		// DECXCPR: CSI ? 6 n
		if len(params) > 0 && params[0] == 6 {
			scr := vt.activeScreen()
			repR := scr.curR + 1
			if scr.originMode {
				repR = (scr.curR - scr.scrollTop) + 1
				if repR < 1 {
					repR = 1
				}
			}
			fmt.Fprintf(&vt.pendingReplies, "\x1b[?%d;%dR", repR, scr.curC+1)
		}
		return
	}
	if final != 'h' && final != 'l' {
		return
	}
	set := final == 'h'
	for _, p := range params {
		// Re-resolve activeScreen on every iteration so multi-parameter sequences like
		// CSI ?1049;25l or CSI ?1049;6h apply subsequent parameters to the newly active screen!
		scr := vt.activeScreen()
		switch p {
		case 1: // DECCKM - Application Cursor Keys
			vt.appCursorKeys = set
		case 6: // DECOM - Origin Mode
			scr.originMode = set
			scr.curC = 0
			if set {
				scr.curR = scr.scrollTop
			} else {
				scr.curR = 0
			}
			scr.pendingWrap = false
		case 7: // DECAWM - Auto Wrap Mode
			scr.autoWrap = set
			if !set {
				scr.pendingWrap = false
			}
		case 25: // DECTCEM - Cursor Visible
			scr.cursorVis = set
			if set && vt.inClearDump {
				vt.finalizeClearDump()
			}
		case 47: // Legacy Alternate Screen Buffer (no clear, no cursor save)
			vt.switchAltScreenMode(47, set)
		case 1047: // Alternate Screen Buffer (clears alt screen)
			vt.switchAltScreenMode(1047, set)
		case 1048: // Save/Restore Cursor
			if set {
				vt.saveCursorOn(scr)
			} else {
				vt.restoreCursorOn(scr)
			}
		case 1049: // Save Cursor + Alternate Screen Buffer
			vt.switchAltScreenMode(1049, set)
		case 1000, 1002, 1003: // Mouse tracking modes
			if set {
				vt.mouseMode = p
			} else if vt.mouseMode == p {
				vt.mouseMode = 0
			}
		case 1004: // Focus events
			vt.focusEvents = set
		case 1006: // SGR mouse encoding
			vt.mouseSGR = set
		case 2004: // Bracketed paste
			vt.bracketedPaste = set
		case 2026: // Synchronized output
			vt.syncOutput = set
			if !set {
				if vt.sawUpwardRedraw {
					vt.commitInPlaceScrollback()
				}
				if vt.inClearDump {
					vt.finalizeClearDump()
				}
			}
		}
	}
}

func (vt *vterm) switchAltScreenMode(mode int, enter bool) {
	if enter {
		switch mode {
		case 47:
			if !vt.inAltScreen {
				vt.inAltScreen = true
				vt.alt.curR = clampInt(vt.primary.curR, 0, vt.alt.rows-1)
				vt.alt.curC = clampInt(vt.primary.curC, 0, vt.alt.cols-1)
				vt.alt.pendingWrap = false
			}
		case 1047:
			if !vt.inAltScreen {
				vt.inAltScreen = true
				vt.alt = newVTermScreen(vt.primary.rows, vt.primary.cols)
				vt.alt.curR = clampInt(vt.primary.curR, 0, vt.alt.rows-1)
				vt.alt.curC = clampInt(vt.primary.curC, 0, vt.alt.cols-1)
			} else {
				vt.alt = newVTermScreen(vt.primary.rows, vt.primary.cols)
			}
		case 1049:
			if !vt.inAltScreen {
				vt.saveCursorOn(&vt.primary)
				vt.inAltScreen = true
			}
			vt.alt = newVTermScreen(vt.primary.rows, vt.primary.cols)
		}
		return
	}

	switch mode {
	case 47:
		if vt.inAltScreen {
			vt.inAltScreen = false
			vt.primary.curR = clampInt(vt.alt.curR, 0, vt.primary.rows-1)
			vt.primary.curC = clampInt(vt.alt.curC, 0, vt.primary.cols-1)
			vt.primary.pendingWrap = false
			for r := range vt.primary.grid {
				vt.primary.grid[r].dirty = true
			}
		}
	case 1047:
		if vt.inAltScreen {
			vt.alt = newVTermScreen(vt.primary.rows, vt.primary.cols)
			vt.inAltScreen = false
			vt.primary.pendingWrap = false
			for r := range vt.primary.grid {
				vt.primary.grid[r].dirty = true
			}
		}
	case 1049:
		if vt.inAltScreen {
			vt.alt = newVTermScreen(vt.primary.rows, vt.primary.cols)
			vt.inAltScreen = false
			for r := range vt.primary.grid {
				vt.primary.grid[r].dirty = true
			}
		}
		vt.restoreCursorOn(&vt.primary)
	}
}

func (vt *vterm) saveCursor() {
	vt.saveCursorOn(vt.activeScreen())
}

func (vt *vterm) saveCursorOn(scr *vtermScreen) {
	scr.savedCurR = scr.curR
	scr.savedCurC = scr.curC
	scr.savedStyle = vt.style
	scr.savedOriginMode = scr.originMode
	scr.savedPendingWrap = scr.pendingWrap
}

func (vt *vterm) restoreCursor() {
	vt.restoreCursorOn(vt.activeScreen())
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
	scr.savedCurR = 0
	scr.savedCurC = 0
	scr.savedStyle = vtermStyle{}
	scr.savedOriginMode = false
	scr.savedPendingWrap = false
	vt.style = vtermStyle{}
	vt.appCursorKeys = false
	vt.appKeypad = false
	vt.insertMode = false
	vt.newlineMode = false
	vt.g0Charset = 'B'
	vt.g1Charset = 'B'
	vt.activeCharset = 0
}

func (vt *vterm) resetFull() {
	rows, cols := vt.primary.rows, vt.primary.cols
	vt.primary = newVTermScreen(rows, cols)
	vt.alt = newVTermScreen(rows, cols)
	vt.inAltScreen = false
	vt.style = vtermStyle{}
	vt.scrollOffset = 0
	vt.inClearDump = false
	vt.savedScroll = nil
	vt.hasPreRedraw = false
	vt.sawUpwardRedraw = false
	vt.wroteSincePreRedraw = false
	vt.appCursorKeys = false
	vt.appKeypad = false
	vt.insertMode = false
	vt.newlineMode = false
	vt.bracketedPaste = false
	vt.focusEvents = false
	vt.mouseMode = 0
	vt.mouseSGR = false
	vt.syncOutput = false
	vt.modifyOtherKeys = 0
	vt.g0Charset = 'B'
	vt.g1Charset = 'B'
	vt.activeCharset = 0
	vt.lastPrintedCh = 0
	vt.lastPrintedCombining = ""
	vt.lastPrintedWidth = 0
	vt.title = ""
	vt.cwd = ""
	vt.agentState = ""
	vt.sawOSC7501 = false
	vt.sawWorkingSinceEncode = false
	vt.lastSpinnerAt = time.Time{}
	vt.progressSeq = ""
}

func (vt *vterm) handleSGR(raw string) {
	if raw == "" {
		href := vt.style.hyperlink
		vt.style = vtermStyle{hyperlink: href}
		return
	}
	tokens := strings.Split(raw, ";")
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		// Support colon subparameters such as 4:3 (curly), 4:4 (dotted), 4:5 (dashed), 38:2::r:g:b, 38:5:n
		if strings.Contains(tok, ":") {
			sub := strings.Split(tok, ":")
			code, _ := strconv.Atoi(sub[0])
			switch code {
			case 4:
				vt.style.flags &^= vtermUnderlineMask
				subKind := 1
				if len(sub) > 1 && sub[1] != "" {
					subKind, _ = strconv.Atoi(sub[1])
				}
				switch subKind {
				case 0:
					// underline off
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
				var c vtermColor
				if len(sub) >= 3 && sub[1] == "5" {
					idx, _ := strconv.Atoi(sub[2])
					if idx >= 0 && idx <= 255 {
						c = vtermColor{kind: vtermColorIndexed, index: uint8(idx)}
					}
				} else if len(sub) >= 5 && sub[1] == "2" {
					offset := 2
					if len(sub) >= 6 {
						offset = 3
					}
					r, _ := strconv.Atoi(sub[offset])
					g, _ := strconv.Atoi(sub[offset+1])
					b, _ := strconv.Atoi(sub[offset+2])
					if r >= 0 && r <= 255 && g >= 0 && g <= 255 && b >= 0 && b <= 255 {
						c = vtermColor{kind: vtermColorRGB, r: uint8(r), g: uint8(g), b: uint8(b)}
					}
				}
				if c.kind != vtermColorDefault {
					if code == 38 {
						vt.style.fg = c
					} else if code == 48 {
						vt.style.bg = c
					} else {
						vt.style.ul = c
					}
				}
			}
			continue
		}

		n := 0
		if tok != "" {
			n, _ = strconv.Atoi(tok)
		}
		switch {
		case n == 0:
			href := vt.style.hyperlink
			vt.style = vtermStyle{hyperlink: href}
		case n == 1:
			vt.style.flags |= vtermAttrBold
		case n == 2:
			vt.style.flags |= vtermAttrDim
		case n == 3:
			vt.style.flags |= vtermAttrItalic
		case n == 4:
			vt.style.flags &^= vtermUnderlineMask
			vt.style.flags |= vtermAttrUnderline
		case n == 5, n == 6:
			vt.style.flags |= vtermAttrBlink
		case n == 7:
			vt.style.flags |= vtermAttrReverse
		case n == 8:
			vt.style.flags |= vtermAttrInvisible
		case n == 9:
			vt.style.flags |= vtermAttrStrikethrough
		case n == 21:
			vt.style.flags &^= vtermUnderlineMask
			vt.style.flags |= vtermAttrDoubleUnderline
		case n == 22:
			vt.style.flags &^= (vtermAttrBold | vtermAttrDim)
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
			if i+1 < len(tokens) {
				mode, _ := strconv.Atoi(tokens[i+1])
				if mode == 5 && i+2 < len(tokens) {
					idx, _ := strconv.Atoi(tokens[i+2])
					if idx >= 0 && idx <= 255 {
						c := vtermColor{kind: vtermColorIndexed, index: uint8(idx)}
						if n == 38 {
							vt.style.fg = c
						} else if n == 48 {
							vt.style.bg = c
						} else {
							vt.style.ul = c
						}
					}
					i += 2
				} else if mode == 2 && i+4 < len(tokens) {
					r, _ := strconv.Atoi(tokens[i+2])
					g, _ := strconv.Atoi(tokens[i+3])
					b, _ := strconv.Atoi(tokens[i+4])
					if r >= 0 && r <= 255 && g >= 0 && g <= 255 && b >= 0 && b <= 255 {
						c := vtermColor{kind: vtermColorRGB, r: uint8(r), g: uint8(g), b: uint8(b)}
						if n == 38 {
							vt.style.fg = c
						} else if n == 48 {
							vt.style.bg = c
						} else {
							vt.style.ul = c
						}
					}
					i += 4
				}
			}
		case n == 39:
			vt.style.fg = vtermColor{kind: vtermColorDefault}
		case n >= 40 && n <= 47:
			vt.style.bg = vtermColor{kind: vtermColorIndexed, index: uint8(n - 40)}
		case n == 49:
			vt.style.bg = vtermColor{kind: vtermColorDefault}
		case n == 53:
			vt.style.flags |= vtermAttrOverline
		case n == 55:
			vt.style.flags &^= vtermAttrOverline
		case n == 59:
			vt.style.ul = vtermColor{kind: vtermColorDefault}
		case n >= 90 && n <= 97:
			vt.style.fg = vtermColor{kind: vtermColorIndexed, index: uint8(n - 90 + 8)}
		case n >= 100 && n <= 107:
			vt.style.bg = vtermColor{kind: vtermColorIndexed, index: uint8(n - 100 + 8)}
		}
	}
}

func rowTrimmedCells(row vtermRow, minKeep int) []vtermCell {
	cells := row.cells
	if row.wrapped {
		// Trim only trailing padding cell if a wide char wrapped before the last column.
		end := len(cells)
		var defStyle vtermStyle
		if end > 0 && cells[end-1].width == 1 && (cells[end-1].ch == ' ' || cells[end-1].ch == 0) && cells[end-1].combining == "" && cells[end-1].style == defStyle {
			// Check if this was a wide-wrap spacer at the end of a wrapped row; keep normal spaces unless at end.
		}
		return cells[:end]
	}
	end := len(cells)
	var defStyle vtermStyle
	for end > minKeep {
		c := cells[end-1]
		if c.width == 1 && (c.ch == ' ' || c.ch == 0) && c.combining == "" && c.style == defStyle {
			end--
		} else {
			break
		}
	}
	return cells[:end]
}

// reflowRows re-wraps a slice of vtermRows to newCols, preserving soft-wrapped logical lines
// and optionally tracking a cursor coordinate (trackR, trackC) -> (newR, newC).
func reflowRows(rows []vtermRow, newCols int, trackR, trackC int) (out []vtermRow, mappedR, mappedC int) {
	if newCols < 1 {
		newCols = 1
	}
	var defStyle vtermStyle
	blank := blankVTermCell(defStyle)
	mappedR, mappedC = -1, -1

	i := 0
	for i < len(rows) {
		// Group consecutive rows belonging to the same soft-wrapped logical line.
		startRow := i
		for i < len(rows)-1 && rows[i].wrapped {
			i++
		}
		endRow := i
		i++

		var logical []vtermCell
		cursorLogicalIdx := -1
		for r := startRow; r <= endRow; r++ {
			minKeep := 0
			if r == trackR {
				minKeep = trackC + 1
				if minKeep > len(rows[r].cells) {
					minKeep = len(rows[r].cells)
				}
			}
			part := rowTrimmedCells(rows[r], minKeep)
			for c := 0; c < len(part); c++ {
				if r == trackR && c == trackC {
					cursorLogicalIdx = len(logical)
				}
				cell := part[c]
				if cell.width == 0 {
					continue
				}
				logical = append(logical, cell)
			}
			if r == trackR && cursorLogicalIdx < 0 {
				cursorLogicalIdx = len(logical)
			}
		}

		if len(logical) == 0 {
			newRow := newVTermRow(newCols, defStyle)
			newRow.wrapped = rows[endRow].wrapped
			if cursorLogicalIdx >= 0 {
				mappedR = len(out)
				mappedC = clampInt(trackC, 0, newCols-1)
			}
			out = append(out, newRow)
			continue
		}

		curRow := newVTermRow(newCols, defStyle)
		col := 0
		for idx, cell := range logical {
			w := int(cell.width)
			if w < 1 {
				w = 1
			}
			if w == 2 && newCols == 1 {
				w = 1
				cell.width = 1
			}
			if col+w > newCols {
				curRow.wrapped = true
				out = append(out, curRow)
				curRow = newVTermRow(newCols, defStyle)
				col = 0
			}
			if idx == cursorLogicalIdx {
				mappedR = len(out)
				mappedC = col
			}
			curRow.cells[col] = cell
			if w == 2 && col+1 < newCols {
				curRow.cells[col+1] = vtermCell{ch: 0, width: 0, style: cell.style}
			} else if w == 2 {
				curRow.cells[col] = blank
			}
			col += w
		}
		if cursorLogicalIdx >= len(logical) {
			mappedR = len(out)
			mappedC = clampInt(col, 0, newCols-1)
		}
		curRow.wrapped = rows[endRow].wrapped
		out = append(out, curRow)
	}
	return out, mappedR, mappedC
}

// Resize updates the dimensions of both primary and alternate screens while
// preserving scrollback, reflowing soft-wrapped primary rows, and clamping saved cursors.
func (vt *vterm) Resize(rows, cols int) {
	if rows < 1 {
		rows = 1
	} else if rows > maxVTermRows {
		rows = maxVTermRows
	}
	if cols < 1 {
		cols = 1
	} else if cols > maxVTermCols {
		cols = maxVTermCols
	}
	vt.hasPreRedraw = false
	vt.sawUpwardRedraw = false
	vt.wroteSincePreRedraw = false
	vt.resizeScreen(&vt.primary, rows, cols, true)
	vt.resizeScreen(&vt.alt, rows, cols, false)
}

func (vt *vterm) resizeScreen(scr *vtermScreen, newRows, newCols int, isPrimary bool) {
	if scr.rows == newRows && scr.cols == newCols {
		return
	}

	var defStyle vtermStyle
	if isPrimary && newCols != scr.cols {
		if len(vt.scrollback) > 0 {
			vt.scrollback, _, _ = reflowRows(vt.scrollback, newCols, -1, -1)
			vt.scrollback = vt.trimScrollbackSlice(vt.scrollback)
		}
		if len(vt.savedScroll) > 0 {
			vt.savedScroll, _, _ = reflowRows(vt.savedScroll, newCols, -1, -1)
			vt.savedScroll = vt.trimScrollbackSlice(vt.savedScroll)
		}
		lastActive := scr.curR
		for r := len(scr.grid) - 1; r > lastActive; r-- {
			if scr.grid[r].plainText() != "" || scr.grid[r].wrapped {
				lastActive = r
				break
			}
		}
		if lastActive >= len(scr.grid) {
			lastActive = len(scr.grid) - 1
		}
		activeReflowed, newCurR, newCurC := reflowRows(scr.grid[:lastActive+1], newCols, scr.curR, scr.curC)
		if newCurR < 0 {
			newCurR = 0
			newCurC = 0
		}
		if len(activeReflowed) > newRows {
			pushTop := len(activeReflowed) - newRows
			for i := 0; i < pushTop; i++ {
				vt.appendScrollbackRow(activeReflowed[i])
			}
			activeReflowed = activeReflowed[pushTop:]
			newCurR = max(0, newCurR-pushTop)
		}
		for len(activeReflowed) < newRows {
			activeReflowed = append(activeReflowed, newVTermRow(newCols, defStyle))
		}
		scr.grid = activeReflowed
		scr.curR = clampInt(newCurR, 0, newRows-1)
		scr.curC = clampInt(newCurC, 0, newCols-1)
	} else {
		// Adjust row count first.
		if newRows < scr.rows {
			drop := scr.rows - newRows
			blankBottom := 0
			for r := scr.rows - 1; r > scr.curR && blankBottom < drop; r-- {
				if scr.grid[r].plainText() == "" {
					blankBottom++
				} else {
					break
				}
			}
			pushTop := drop - blankBottom
			if pushTop > 0 {
				if isPrimary {
					for i := 0; i < pushTop; i++ {
						vt.appendScrollbackRow(scr.grid[i])
					}
				}
				scr.grid = append([]vtermRow(nil), scr.grid[pushTop:scr.rows-blankBottom]...)
				scr.curR -= pushTop
				if scr.curR < 0 {
					scr.curR = 0
				}
			} else {
				scr.grid = scr.grid[:newRows]
			}
		} else if newRows > scr.rows {
			add := newRows - scr.rows
			for i := 0; i < add; i++ {
				scr.grid = append(scr.grid, newVTermRow(scr.cols, defStyle))
			}
		}

		// Adjust column count on each row (alternate screen).
		if newCols != scr.cols {
			blank := blankVTermCell(defStyle)
			for r := range scr.grid {
				oldCells := scr.grid[r].cells
				if newCols > len(oldCells) {
					next := make([]vtermCell, newCols)
					copy(next, oldCells)
					for c := len(oldCells); c < newCols; c++ {
						next[c] = blank
					}
					scr.grid[r].cells = next
				} else if newCols < len(oldCells) {
					next := make([]vtermCell, newCols)
					copy(next, oldCells[:newCols])
					if newCols > 0 && next[newCols-1].width == 2 {
						next[newCols-1] = blank
					}
					scr.grid[r].cells = next
				}
				scr.grid[r].dirty = true
			}
		}
	}

	scr.rows = newRows
	scr.cols = newCols
	scr.tabs = newDefaultTabs(newCols)
	scr.scrollTop = 0
	scr.scrollBottom = newRows - 1
	scr.curR = clampInt(scr.curR, 0, newRows-1)
	scr.curC = clampInt(scr.curC, 0, newCols-1)
	scr.savedCurR = clampInt(scr.savedCurR, 0, newRows-1)
	scr.savedCurC = clampInt(scr.savedCurC, 0, newCols-1)
	if scr.savedCurC != newCols-1 {
		scr.savedPendingWrap = false
	}
	scr.pendingWrap = false
}

// ViewportRow returns the resolved vtermRow for viewport row r (0..rows-1),
// taking vt.scrollOffset into account when scrolled back on the primary screen.
func (vt *vterm) ViewportRow(r int) vtermRow {
	return vt.viewportRowWithScrollback(r, vt.effectiveScrollback())
}

func (vt *vterm) viewportRowWithScrollback(r int, sb []vtermRow) vtermRow {
	scr := vt.activeScreen()
	if r < 0 || r >= scr.rows {
		return newVTermRow(scr.cols, vtermStyle{})
	}
	off := vt.ScrollOffset()
	if vt.inAltScreen || off <= 0 {
		return scr.grid[r]
	}
	sbLen := len(sb)
	idx := sbLen - off + r
	if idx < 0 {
		return newVTermRow(scr.cols, vtermStyle{})
	}
	if idx < sbLen {
		row := sb[idx]
		if len(row.cells) == scr.cols {
			return row
		}
		resized := newVTermRow(scr.cols, vtermStyle{})
		copy(resized.cells, row.cells)
		if scr.cols > 0 && resized.cells[scr.cols-1].width == 2 {
			resized.cells[scr.cols-1] = blankVTermCell(vtermStyle{})
		}
		resized.wrapped = row.wrapped
		return resized
	}
	gridIdx := idx - sbLen
	if gridIdx >= 0 && gridIdx < len(scr.grid) {
		return scr.grid[gridIdx]
	}
	return newVTermRow(scr.cols, vtermStyle{})
}

// ScreenPlainLines returns plain-text lines from scrollback and the active screen,
// preserving single blank lines for cleanRenderedLines.
func (vt *vterm) ScreenPlainLines() []string {
	scr := vt.activeScreen()
	var lines []string
	if !vt.inAltScreen {
		sb := vt.effectiveScrollback()
		for i := range sb {
			lines = append(lines, sb[i].plainText())
		}
	}
	for r := 0; r < scr.rows; r++ {
		lines = append(lines, scr.grid[r].plainText())
	}
	return lines
}

// DetectionLines returns the last maxLines non-empty plain-text lines from the
// virtual terminal (combining scrollback + active screen), modeled after herdr's detection_text.
func (vt *vterm) DetectionLines(maxLines int) []string {
	scr := vt.activeScreen()
	var lines []string
	if !vt.inAltScreen {
		sb := vt.effectiveScrollback()
		start := 0
		if maxLines > 0 && len(sb) > maxLines*2 {
			start = len(sb) - maxLines*2
		}
		for i := start; i < len(sb); i++ {
			if txt := strings.TrimSpace(sb[i].plainText()); txt != "" {
				lines = append(lines, txt)
			}
		}
	}
	for r := 0; r < scr.rows; r++ {
		if txt := strings.TrimSpace(scr.grid[r].plainText()); txt != "" {
			lines = append(lines, txt)
		}
	}
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
}

const spinnerSettleDelay = 300 * time.Millisecond

// HasActiveSpinnerLine checks whether the bottom region of the active screen
// currently displays an active Braille spinner status line (e.g. "⠸ Working").
func (vt *vterm) HasActiveSpinnerLine() bool {
	scr := vt.activeScreen()
	if scr == nil {
		return false
	}
	substantiveCount := 0
	for r := scr.rows - 1; r >= 0; r-- {
		line := strings.TrimSpace(scr.grid[r].plainText())
		if line == "" || isPureHorizontalRule(line) {
			continue
		}
		if _, ok := parseSpinnerLine(line); ok {
			return true
		}
		if isUIChromeLine(line) {
			continue
		}
		if cleanSummaryText(line) != "" {
			substantiveCount++
			if substantiveCount >= 4 {
				break
			}
		}
	}
	return false
}

func (vt *vterm) NextSpinnerSettleDeadline(currentAgentState string) (time.Time, bool) {
	if vt.sawOSC7501 || currentAgentState != "state=working" || vt.lastSpinnerAt.IsZero() {
		return time.Time{}, false
	}
	if vt.HasActiveSpinnerLine() {
		return time.Time{}, false
	}
	return vt.lastSpinnerAt.Add(spinnerSettleDelay), true
}

func (vt *vterm) SpinnerSettleReady(currentAgentState string, now time.Time) bool {
	deadline, ok := vt.NextSpinnerSettleDeadline(currentAgentState)
	return ok && !now.Before(deadline)
}

// RenderPreviewANSI renders the bottom targetRows x targetCols of the active screen
// (and scrollback if needed) with full SGR styles for `di --preview`.
func (vt *vterm) RenderPreviewANSI(targetCols, targetRows int) string {
	if targetCols <= 0 {
		targetCols = 80
	}
	if targetRows <= 0 {
		targetRows = 24
	}
	scr := vt.activeScreen()
	var allRows []vtermRow
	if !vt.inAltScreen {
		sb := vt.effectiveScrollback()
		if len(sb) > 0 {
			start := 0
			if len(sb) > targetRows {
				start = len(sb) - targetRows
			}
			allRows = append(allRows, sb[start:]...)
		}
	}
	lastNonBlank := -1
	for i := range scr.grid {
		allRows = append(allRows, scr.grid[i])
	}
	for i := len(allRows) - 1; i >= 0; i-- {
		if strings.TrimSpace(allRows[i].plainText()) != "" {
			lastNonBlank = i
			break
		}
	}
	if lastNonBlank < 0 {
		return ""
	}
	end := lastNonBlank + 1
	start := 0
	if end > targetRows {
		start = end - targetRows
	}
	selected := allRows[start:end]

	var b strings.Builder
	for i, row := range selected {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(encodeVTermRowANSI(row, targetCols))
	}
	return b.String()
}

// ScrollbackHeaderANSI returns an ANSI prefix containing the OSC 7502 vterm=1
// marker and all scrollback rows so a newly attached client receives the full
// scrollback history before the server's active viewport history.
func (vt *vterm) ScrollbackHeaderANSI() []byte {
	sb := vt.effectiveScrollback()
	if len(sb) == 0 {
		return []byte(fmt.Sprintf("\x1b]7502;vterm=1;rows=%d;cols=%d\x07", vt.primary.rows, vt.primary.cols))
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "\x1b]7502;vterm=1;rows=%d;cols=%d\x07\x1b[0m\x1b[H", vt.primary.rows, vt.primary.cols)
	for _, row := range sb {
		buf.WriteString(encodeVTermRowANSI(row, vt.primary.cols))
		buf.WriteString("\r\n")
	}
	for r := 0; r < vt.primary.rows-1; r++ {
		buf.WriteString("\r\n")
	}
	buf.WriteString("\x1b[2J\x1b[H")
	return buf.Bytes()
}

func writeScreenSavedCursorAndState(buf *bytes.Buffer, scr *vtermScreen) {
	// 1. Restore saved cursor state onto scr using DECSC (\x1b7).
	if scr.savedOriginMode {
		buf.WriteString("\x1b[?6h")
	} else {
		buf.WriteString("\x1b[?6l")
	}
	writeVTermStyleTransition(buf, vtermStyle{}, scr.savedStyle, true)
	fmt.Fprintf(buf, "\x1b[%d;%dH\x1b7", scr.savedCurR+1, scr.savedCurC+1)

	// 2. Restore scroll margins (DECSTBM).
	if scr.scrollTop != 0 || scr.scrollBottom != scr.rows-1 {
		fmt.Fprintf(buf, "\x1b[%d;%dr", scr.scrollTop+1, scr.scrollBottom+1)
	} else {
		buf.WriteString("\x1b[r")
	}

	// 3. Restore active originMode (?6) and autoWrap (?7).
	if scr.originMode {
		buf.WriteString("\x1b[?6h")
	} else {
		buf.WriteString("\x1b[?6l")
	}
	if scr.autoWrap {
		buf.WriteString("\x1b[?7h")
	} else {
		buf.WriteString("\x1b[?7l")
	}

	// 4. Restore wrapped flags and active cursor coordinates.
	var wrappedIndices []string
	for r := 0; r < scr.rows; r++ {
		if scr.grid[r].wrapped {
			wrappedIndices = append(wrappedIndices, strconv.Itoa(r))
		}
	}
	if len(wrappedIndices) > 0 {
		fmt.Fprintf(buf, "\x1b]7502;wrap=%s\x07", strings.Join(wrappedIndices, ","))
	}

	// When originMode is active, CUP uses coordinates relative to scrollTop; position with ?6l first
	// or adjust row, then restore ?6h if needed.
	if scr.originMode {
		buf.WriteString("\x1b[?6l")
		fmt.Fprintf(buf, "\x1b[%d;%dH", scr.curR+1, scr.curC+1)
		buf.WriteString("\x1b[?6h")
		// Note: ?6h homes the cursor, so position relative to scrollTop when within margins.
		if scr.curR >= scr.scrollTop && scr.curR <= scr.scrollBottom {
			fmt.Fprintf(buf, "\x1b[%d;%dH", (scr.curR-scr.scrollTop)+1, scr.curC+1)
		}
	} else {
		fmt.Fprintf(buf, "\x1b[%d;%dH", scr.curR+1, scr.curC+1)
	}
	if scr.pendingWrap && scr.autoWrap && scr.curC == scr.cols-1 {
		buf.WriteString("\x1b]7502;pw=1\x07")
	}

	// 5. Restore Kitty keyboard stack for this screen.
	for i := 0; i < scr.kittyLen; i++ {
		fmt.Fprintf(buf, "\x1b[>%du", scr.kittyStack[i])
	}
}

// SnapshotANSI serializes the complete virtual terminal state (scrollback + primary
// and alternate screens + saved cursors + SGR + margins + modes + OSC 7502 vterm=1
// marker) into a single self-contained ANSI stream so newly attached clients and
// previews can reconstruct the exact vterm state without duplicating scrollback or
// replaying historical bells/clipboard side effects.
func (vt *vterm) SnapshotANSI() []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "\x1b]7502;vterm=1;snap=begin;rows=%d;cols=%d\x07", vt.primary.rows, vt.primary.cols)
	if vt.title != "" {
		buf.WriteString("\x1b]2;")
		buf.WriteString(vt.title)
		buf.WriteByte(0x07)
	}
	if vt.cwd != "" {
		buf.WriteString("\x1b]7;")
		buf.WriteString(vt.cwd)
		buf.WriteByte(0x07)
	}
	if vt.agentState != "" {
		buf.WriteString("\x1b]7501;")
		buf.WriteString(vt.agentState)
		buf.WriteByte(0x07)
	}
	if vt.progressSeq != "" {
		buf.WriteString("\x1b]9;")
		buf.WriteString(vt.progressSeq)
		buf.WriteByte(0x07)
	}
	if vt.lastDECRPM != "" {
		buf.WriteString(vt.lastDECRPM)
	}

	buf.WriteString("\x1b[?2026h\x1b[?25l\x1b]8;;\x1b\\\x1b[0m\x1b[r\x1b[?6l\x1b[?7l\x1b[H\x1b[2J")

	// 1. Emit scrollback + primary screen on the primary buffer first.
	sb := vt.effectiveScrollback()
	if len(sb) > 0 {
		for _, row := range sb {
			buf.WriteString(encodeVTermRowANSI(row, vt.primary.cols))
			buf.WriteString("\r\n")
		}
		for r := 0; r < vt.primary.rows-1; r++ {
			buf.WriteString("\r\n")
		}
	}
	buf.WriteString("\x1b[H")
	for r := 0; r < vt.primary.rows; r++ {
		buf.WriteString("\x1b[2K")
		buf.WriteString(encodeVTermRowANSI(vt.primary.grid[r], vt.primary.cols))
		if r < vt.primary.rows-1 {
			buf.WriteString("\r\n")
		}
	}
	writeScreenSavedCursorAndState(&buf, &vt.primary)

	// 2. Paint alternate screen state (and leave in ?1047h if currently in alt screen).
	if vt.inAltScreen {
		buf.WriteString("\x1b[?1047h\x1b[0m\x1b[r\x1b[?6l\x1b[?7l\x1b[H\x1b[2J")
		for r := 0; r < vt.alt.rows; r++ {
			fmt.Fprintf(&buf, "\x1b[%d;1H\x1b[2K", r+1)
			buf.WriteString(encodeVTermRowANSI(vt.alt.grid[r], vt.alt.cols))
		}
		writeScreenSavedCursorAndState(&buf, &vt.alt)
	}

	// 3. Restore active character sets, SGR style, and terminal modes.
	if vt.g0Charset == '0' {
		buf.WriteString("\x1b(0")
	} else {
		buf.WriteString("\x1b(B")
	}
	if vt.g1Charset == '0' {
		buf.WriteString("\x1b)0")
	} else {
		buf.WriteString("\x1b)B")
	}
	if vt.activeCharset == 1 {
		buf.WriteByte(0x0e)
	} else {
		buf.WriteByte(0x0f)
	}

	writeVTermStyleTransition(&buf, vtermStyle{}, vt.style, true)

	scr := vt.activeScreen()
	if scr.cursorShape > 0 {
		fmt.Fprintf(&buf, "\x1b[%d q", scr.cursorShape)
	}
	if vt.appCursorKeys {
		buf.WriteString("\x1b[?1h")
	}
	if vt.appKeypad {
		buf.WriteString("\x1b=")
	}
	if vt.insertMode {
		buf.WriteString("\x1b[4h")
	}
	if vt.newlineMode {
		buf.WriteString("\x1b[20h")
	}
	if vt.bracketedPaste {
		buf.WriteString("\x1b[?2004h")
	}
	if vt.focusEvents {
		buf.WriteString("\x1b[?1004h")
	}
	if vt.mouseMode > 0 {
		fmt.Fprintf(&buf, "\x1b[?%dh", vt.mouseMode)
	}
	if vt.mouseSGR {
		buf.WriteString("\x1b[?1006h")
	}
	if vt.modifyOtherKeys > 0 {
		fmt.Fprintf(&buf, "\x1b[>4;%dm", vt.modifyOtherKeys)
	}
	if scr.cursorVis {
		buf.WriteString("\x1b[?25h")
	}
	buf.WriteString("\x1b[?2026l\x1b]7502;snap=end\x07")
	return buf.Bytes()
}

func encodeVTermRowANSI(row vtermRow, maxCols int) string {
	limit := len(row.cells)
	if maxCols > 0 && maxCols < limit {
		limit = maxCols
	}
	// Trim trailing default-styled blank spaces so rows are compact.
	end := limit
	var defStyle vtermStyle
	for end > 0 {
		c := row.cells[end-1]
		if c.width == 1 && (c.ch == ' ' || c.ch == 0) && c.combining == "" && c.style == defStyle {
			end--
		} else {
			break
		}
	}
	if end == 0 {
		return ""
	}

	var buf bytes.Buffer
	var curStyle vtermStyle
	hasStyle := false
	for col := 0; col < end; col++ {
		c := row.cells[col]
		if c.width == 0 {
			continue
		}
		if c.width == 2 && col+1 >= limit {
			break
		}
		if !hasStyle || c.style != curStyle {
			writeVTermStyleTransition(&buf, curStyle, c.style, !hasStyle)
			curStyle = c.style
			hasStyle = true
		}
		if c.ch == 0 {
			buf.WriteByte(' ')
		} else {
			buf.WriteRune(c.ch)
		}
		if c.combining != "" {
			buf.WriteString(c.combining)
		}
	}
	if hasStyle && curStyle != defStyle {
		if curStyle.hyperlink != "" {
			buf.WriteString("\x1b]8;;\x1b\\")
		}
		buf.WriteString("\x1b[0m")
	}
	return buf.String()
}

func writeVTermStyleTransition(buf *bytes.Buffer, prev, next vtermStyle, forceReset bool) {
	if prev.hyperlink != next.hyperlink || (forceReset && next.hyperlink != "") {
		buf.WriteString("\x1b]8;;")
		buf.WriteString(next.hyperlink)
		buf.WriteString("\x1b\\")
	}
	if !forceReset && prev.fg == next.fg && prev.bg == next.bg && prev.ul == next.ul && prev.flags == next.flags {
		return
	}

	buf.WriteString("\x1b[0")
	if next.flags&vtermAttrBold != 0 {
		buf.WriteString(";1")
	}
	if next.flags&vtermAttrDim != 0 {
		buf.WriteString(";2")
	}
	if next.flags&vtermAttrItalic != 0 {
		buf.WriteString(";3")
	}
	if next.flags&vtermAttrDashedUnderline != 0 {
		buf.WriteString(";4:5")
	} else if next.flags&vtermAttrDottedUnderline != 0 {
		buf.WriteString(";4:4")
	} else if next.flags&vtermAttrCurlyUnderline != 0 {
		buf.WriteString(";4:3")
	} else if next.flags&vtermAttrDoubleUnderline != 0 {
		buf.WriteString(";21")
	} else if next.flags&vtermAttrUnderline != 0 {
		buf.WriteString(";4")
	}
	if next.flags&vtermAttrBlink != 0 {
		buf.WriteString(";5")
	}
	if next.flags&vtermAttrReverse != 0 {
		buf.WriteString(";7")
	}
	if next.flags&vtermAttrInvisible != 0 {
		buf.WriteString(";8")
	}
	if next.flags&vtermAttrStrikethrough != 0 {
		buf.WriteString(";9")
	}
	if next.flags&vtermAttrOverline != 0 {
		buf.WriteString(";53")
	}

	switch next.fg.kind {
	case vtermColorIndexed:
		if next.fg.index < 8 {
			fmt.Fprintf(buf, ";%d", 30+int(next.fg.index))
		} else if next.fg.index < 16 {
			fmt.Fprintf(buf, ";%d", 90+int(next.fg.index)-8)
		} else {
			fmt.Fprintf(buf, ";38;5;%d", next.fg.index)
		}
	case vtermColorRGB:
		fmt.Fprintf(buf, ";38;2;%d;%d;%d", next.fg.r, next.fg.g, next.fg.b)
	}

	switch next.bg.kind {
	case vtermColorIndexed:
		if next.bg.index < 8 {
			fmt.Fprintf(buf, ";%d", 40+int(next.bg.index))
		} else if next.bg.index < 16 {
			fmt.Fprintf(buf, ";%d", 100+int(next.bg.index)-8)
		} else {
			fmt.Fprintf(buf, ";48;5;%d", next.bg.index)
		}
	case vtermColorRGB:
		fmt.Fprintf(buf, ";48;2;%d;%d;%d", next.bg.r, next.bg.g, next.bg.b)
	}

	switch next.ul.kind {
	case vtermColorIndexed:
		fmt.Fprintf(buf, ";58;5;%d", next.ul.index)
	case vtermColorRGB:
		fmt.Fprintf(buf, ";58;2;%d;%d;%d", next.ul.r, next.ul.g, next.ul.b)
	}

	buf.WriteByte('m')
}
