package main

import (
	"bytes"
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
)

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

func newVTermScreen(rows, cols int) vtermScreen {
	if rows < 1 {
		rows = defaultVTermRows
	}
	if cols < 1 {
		cols = defaultVTermCols
	}
	var defStyle vtermStyle
	grid := make([]vtermRow, rows)
	for i := range grid {
		grid[i] = newVTermRow(cols, defStyle)
	}
	return vtermScreen{
		rows:         rows,
		cols:         cols,
		grid:         grid,
		scrollTop:    0,
		scrollBottom: rows - 1,
		autoWrap:     true,
		cursorVis:    true,
	}
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
	hasPending      bool
	lastRowChangeAt time.Time
	hasRowChange    bool
	preferredRow    int
	hasPreferredRow bool
}

func newCursorSettleState() CursorSettleState {
	return CursorSettleState{
		settledVisible: true,
	}
}

func (c *CursorSettleState) OnInput() {
	c.hasPending = false
	c.hasRowChange = false
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
				c.hasPending = true
			}
			c.Advance(now)
			return c.settledPos[0], c.settledPos[1], c.settledVisible
		}
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

	if c.hasPreferredRow && pos[0] == c.preferredRow {
		c.settledPos = pos
		c.settledVisible = true
		c.hasPending = false
		return pos[0], pos[1], true
	}

	delay := cursorSettleDelay
	if c.hasRowChange && now.Sub(c.lastRowChangeAt) < cursorJumpHoldDelay {
		delay = cursorJumpHoldDelay
	}
	if !c.hasPending || c.pendingPos != pos || !c.pendingVisible {
		c.pendingPos = pos
		c.pendingVisible = true
		c.pendingDeadline = now.Add(delay)
		c.hasPending = true
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
// scrollback ring, full SGR & wide-character layout, and in-place scrollback recovery.
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
	preRedrawGrid   []vtermRow
	hasPreRedraw    bool
	sawUpwardRedraw bool

	// Terminal modes forwarded to attached clients.
	appCursorKeys   bool // ?1
	bracketedPaste  bool // ?2004
	focusEvents     bool // ?1004
	mouseMode       int  // 0, 1000, 1002, 1003
	mouseSGR        bool // ?1006
	syncOutput      bool // ?2026
	modifyOtherKeys int

	// Metadata & passthrough
	title              string
	cwd                string
	agentState         string
	lastPrintedCh      rune
	lastPrintedWidth   uint8
	pendingPassthrough bytes.Buffer
	sawServerVTerm     bool

	// VT parser state
	state      vtermParserState
	escInterm  []byte
	csiPrefix  byte
	csiParams  bytes.Buffer
	csiInterm  []byte
	oscBuf     bytes.Buffer
	dcsBuf     bytes.Buffer
	utf8Buf    [4]byte
	utf8Need   int
	utf8Len    int
	oscEscSeen bool

	cursorSettle CursorSettleState
}

func newVTerm(rows, cols int) *vterm {
	if rows < 1 {
		rows = defaultVTermRows
	}
	if cols < 1 {
		cols = defaultVTermCols
	}
	return &vterm{
		primary:       newVTermScreen(rows, cols),
		alt:           newVTermScreen(rows, cols),
		maxScrollback: defaultVTermScrollback,
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

func (vt *vterm) finalizeClearDump() {
	if !vt.inClearDump {
		return
	}
	vt.inClearDump = false
	if len(vt.savedScroll) == 0 {
		return
	}
	if len(vt.scrollback) > 0 && len(vt.scrollback) >= len(vt.savedScroll)/2 {
		// Full-history dump (e.g. agy on SIGWINCH) replaced scrollback at new width.
		vt.savedScroll = nil
		vt.fullScrollbackReplaced = true
		return
	}
	// Redraw after \x1b[3J did not re-dump history; preserve savedScroll.
	combined := make([]vtermRow, 0, len(vt.savedScroll)+len(vt.scrollback))
	combined = append(combined, vt.savedScroll...)
	combined = append(combined, vt.scrollback...)
	if len(combined) > vt.maxScrollback {
		combined = combined[len(combined)-vt.maxScrollback:]
	}
	vt.scrollback = combined
	vt.savedScroll = nil
}

func (vt *vterm) effectiveScrollback() []vtermRow {
	if len(vt.savedScroll) == 0 {
		return vt.scrollback
	}
	if len(vt.scrollback) > 0 && len(vt.scrollback) >= len(vt.savedScroll)/2 {
		return vt.scrollback
	}
	if len(vt.scrollback) == 0 {
		return vt.savedScroll
	}
	combined := make([]vtermRow, 0, len(vt.savedScroll)+len(vt.scrollback))
	combined = append(combined, vt.savedScroll...)
	combined = append(combined, vt.scrollback...)
	if len(combined) > vt.maxScrollback {
		combined = combined[len(combined)-vt.maxScrollback:]
	}
	return combined
}

func (vt *vterm) ConsumeFullScrollbackReplaced() bool {
	if !vt.inClearDump && len(vt.savedScroll) > 0 {
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
	return vtermScrollMetrics{
		TotalRows:    sbLen + scr.rows,
		ScrollOffset: vt.scrollOffset,
		ViewportRows: scr.rows,
	}
}

func (vt *vterm) IsScrolledBack() bool {
	return !vt.inAltScreen && vt.scrollOffset > 0
}

func (vt *vterm) ScrollUp(lines int) bool {
	sbLen := vt.ScrollbackLen()
	if vt.inAltScreen || sbLen == 0 {
		return false
	}
	if lines < 1 {
		lines = 1
	}
	prev := vt.scrollOffset
	vt.scrollOffset += lines
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
	prev := vt.scrollOffset
	vt.scrollOffset -= lines
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
	out := vt.pushedScrollback
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

func (vt *vterm) SawServerVTerm() bool {
	return vt.sawServerVTerm
}

func (vt *vterm) appendScrollbackRow(row vtermRow) {
	cloned := row.clone()
	cloned.dirty = false
	if len(vt.scrollback) > 0 && vt.scrollback[len(vt.scrollback)-1].equalContent(cloned) {
		// Still allow blank lines if the previous row wasn't identical or if pushed via real LF,
		// wait: only deduplicate during synthetic recovery, not real LF.
	}
	vt.scrollback = append(vt.scrollback, cloned)
	if len(vt.scrollback) > vt.maxScrollback {
		drop := len(vt.scrollback) - vt.maxScrollback
		vt.scrollback = append([]vtermRow(nil), vt.scrollback[drop:]...)
	}
	vt.pushedScrollback = append(vt.pushedScrollback, cloned)
	if len(vt.pushedScrollback) > vt.maxScrollback {
		drop := len(vt.pushedScrollback) - vt.maxScrollback
		vt.pushedScrollback = append([]vtermRow(nil), vt.pushedScrollback[drop:]...)
	}
	if vt.scrollOffset > 0 {
		vt.scrollOffset++
		if vt.scrollOffset > len(vt.scrollback) {
			vt.scrollOffset = len(vt.scrollback)
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

	if vt.inAltScreen || len(oldGrid) != len(vt.primary.grid) {
		return
	}
	rows := len(oldGrid)
	if rows < 4 {
		return
	}

	oldText := make([]string, rows)
	newText := make([]string, rows)
	for i := 0; i < rows; i++ {
		oldText[i] = oldGrid[i].plainText()
		newText[i] = vt.primary.grid[i].plainText()
	}

	// Find the first non-empty anchor row in newText (within the top half) and check
	// if a contiguous block of rows in oldText shifted upward by k >= 1 rows.
	bestShift := 0
	maxTop := rows / 2
	if maxTop > 12 {
		maxTop = 12
	}
	for dstStart := 0; dstStart < maxTop && bestShift == 0; dstStart++ {
		if len(strings.TrimSpace(newText[dstStart])) < 3 {
			continue
		}
		for k := 1; dstStart+k < rows-1; k++ {
			if oldText[dstStart+k] != newText[dstStart] {
				continue
			}
			// Verify at least 2 non-empty lines and 3 total lines match at this shift k.
			matched := 0
			substantive := 0
			for r := dstStart; r+k < rows && r < dstStart+10; r++ {
				if oldText[r+k] == newText[r] {
					matched++
					if len(strings.TrimSpace(newText[r])) >= 3 {
						substantive++
					}
				} else {
					break
				}
			}
			if substantive >= 2 && matched >= 3 {
				bestShift = k
				// Push the scrolled-off rows oldGrid[dstStart .. dstStart+k-1]
				for r := dstStart; r < dstStart+k; r++ {
					txt := oldText[r]
					if txt == "" && len(vt.scrollback) == 0 {
						continue
					}
					if len(vt.scrollback) > 0 && vt.scrollback[len(vt.scrollback)-1].plainText() == txt {
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
					r, _ := utf8.DecodeRune(vt.utf8Buf[:vt.utf8Len])
					vt.utf8Need = 0
					vt.utf8Len = 0
					vt.putRune(r)
				}
				continue
			}
			// Invalid continuation byte: flush replacement and re-process b.
			vt.utf8Need = 0
			vt.utf8Len = 0
		}

		switch vt.state {
		case vtStateOSC:
			vt.stepOSC(b)
			continue
		case vtStateDCS:
			vt.stepDCS(b)
			continue
		}

		// C0 controls can appear inside ESC/CSI sequences!
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
			} else if (b & 0xE0) == 0xC0 {
				vt.utf8Buf[0] = b
				vt.utf8Len = 1
				vt.utf8Need = 2
			} else if (b & 0xF0) == 0xE0 {
				vt.utf8Buf[0] = b
				vt.utf8Len = 1
				vt.utf8Need = 3
			} else if (b & 0xF8) == 0xF0 {
				vt.utf8Buf[0] = b
				vt.utf8Len = 1
				vt.utf8Need = 4
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
		if vt.sawUpwardRedraw {
			vt.commitInPlaceScrollback()
		}
		if vt.inClearDump && vt.primary.cursorVis {
			vt.finalizeClearDump()
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
		scr.pendingWrap = false
		nextTab := (scr.curC/8 + 1) * 8
		if nextTab >= scr.cols {
			nextTab = scr.cols - 1
		}
		scr.curC = nextTab
	case 0x0a, 0x0b, 0x0c: // LF, VT, FF
		scr.pendingWrap = false
		vt.lineFeed()
	case 0x0d: // CR
		scr.pendingWrap = false
		scr.curC = 0
	}
}

func (vt *vterm) lineFeed() {
	scr := vt.activeScreen()
	if scr.curR == scr.scrollBottom {
		vt.scrollUpRegion(scr, scr.scrollTop, scr.scrollBottom, 1, !vt.inAltScreen && scr.scrollTop == 0 && scr.scrollBottom == scr.rows-1)
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
		// If an in-place redraw was in progress and now emitted real scroll lines, clear the pre-redraw snapshot.
		vt.hasPreRedraw = false
		vt.sawUpwardRedraw = false
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

func (vt *vterm) clearWideOverlap(scr *vtermScreen, row, col int) {
	if row < 0 || row >= scr.rows || col < 0 || col >= scr.cols {
		return
	}
	cells := scr.grid[row].cells
	c := cells[col]
	blank := blankVTermCell(c.style)
	if c.width == 2 && col+1 < scr.cols {
		cells[col+1] = blank
	} else if c.width == 0 && col > 0 {
		cells[col-1] = blank
	}
}

func (vt *vterm) putRune(r rune) {
	scr := vt.activeScreen()
	w := singleRuneWidth(r)
	if w <= 0 {
		// Zero-width combining character: attach to previous cell on this row if possible.
		if scr.curR >= 0 && scr.curR < scr.rows {
			targetC := scr.curC - 1
			if scr.pendingWrap {
				targetC = scr.cols - 1
			}
			if targetC >= 0 && targetC < scr.cols {
				if scr.grid[scr.curR].cells[targetC].width == 0 && targetC > 0 {
					targetC--
				}
				scr.grid[scr.curR].cells[targetC].combining += string(r)
				scr.grid[scr.curR].dirty = true
			}
		}
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
			w = 1
		}
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
	vt.lastPrintedWidth = uint8(w)
}

func (vt *vterm) stepEsc(b byte) {
	switch b {
	case '[':
		vt.state = vtStateCSI
		vt.csiPrefix = 0
		vt.csiParams.Reset()
		vt.csiInterm = vt.csiInterm[:0]
	case ']':
		vt.state = vtStateOSC
		vt.oscBuf.Reset()
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
	case 'c': // RIS
		vt.resetFull()
		vt.state = vtStateGround
	case '=', '>': // DECKPAM / DECKPNM
		vt.state = vtStateGround
	default:
		if b >= 0x20 && b <= 0x2f {
			vt.escInterm = append(vt.escInterm, b)
			vt.state = vtStateEscIntermediate
		} else {
			vt.state = vtStateGround
		}
	}
}

func (vt *vterm) stepEscIntermediate(b byte) {
	if b >= 0x20 && b <= 0x2f {
		vt.escInterm = append(vt.escInterm, b)
		return
	}
	if b >= 0x30 && b <= 0x7e {
		// ESC # 8 (DECALN) etc.
		vt.state = vtStateGround
	}
}

func (vt *vterm) stepOSC(b byte) {
	if vt.oscEscSeen {
		vt.oscEscSeen = false
		if b == '\\' {
			vt.dispatchOSC(vt.oscBuf.String())
			vt.state = vtStateGround
			return
		}
		if b == '[' {
			vt.dispatchOSC(vt.oscBuf.String())
			vt.state = vtStateCSI
			vt.csiPrefix = 0
			vt.csiParams.Reset()
			vt.csiInterm = vt.csiInterm[:0]
			return
		}
		vt.oscBuf.WriteByte(0x1b)
	}
	if b == 0x07 {
		vt.dispatchOSC(vt.oscBuf.String())
		vt.state = vtStateGround
		return
	}
	if b == 0x1b {
		vt.oscEscSeen = true
		return
	}
	if vt.oscBuf.Len() < 65536 {
		vt.oscBuf.WriteByte(b)
	}
}

func (vt *vterm) stepDCS(b byte) {
	if vt.oscEscSeen {
		vt.oscEscSeen = false
		if b == '\\' {
			vt.state = vtStateGround
			return
		}
	}
	if b == 0x07 {
		vt.state = vtStateGround
		return
	}
	if b == 0x1b {
		vt.oscEscSeen = true
		return
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
			parts := strings.Split(rest[2:], ";")
			if len(parts) > 0 {
				switch parts[0] {
				case "0":
					vt.agentState = "idle"
				case "1", "3":
					vt.agentState = "running"
				case "2":
					vt.agentState = "error"
				case "4":
					vt.agentState = "attention"
				}
			}
		}
	case "52":
		// Forward OSC 52 clipboard sequence directly to the attached terminal.
		vt.pendingPassthrough.WriteString("\x1b]52;")
		vt.pendingPassthrough.WriteString(rest)
		vt.pendingPassthrough.WriteByte(0x07)
	case "7501":
		s := strings.TrimSpace(rest)
		switch {
		case strings.Contains(s, "state=done"):
			vt.agentState = "state=idle"
		case strings.Contains(s, "state=clear"), strings.Contains(s, "state=error"):
			vt.agentState = ""
		default:
			vt.agentState = s
		}
	case "7502":
		if strings.Contains(rest, "vterm=1") {
			vt.sawServerVTerm = true
		}
	}
}

func (vt *vterm) stepCSI(b byte) {
	if b >= 0x30 && b <= 0x3f {
		if vt.csiParams.Len() == 0 && len(vt.csiInterm) == 0 && (b == '?' || b == '>' || b == '<' || b == '=') {
			vt.csiPrefix = b
		} else {
			vt.csiParams.WriteByte(b)
		}
		return
	}
	if b >= 0x20 && b <= 0x2f {
		vt.csiInterm = append(vt.csiInterm, b)
		return
	}
	if b >= 0x40 && b <= 0x7e {
		vt.dispatchCSI(vt.csiPrefix, vt.csiInterm, vt.csiParams.String(), b)
		vt.state = vtStateGround
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
		} else if v, err := strconv.Atoi(p); err == nil {
			out[i] = v
		}
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

	// Kitty keyboard protocol: CSI > flags u, CSI < n u, CSI = flags ; mode u
	if final == 'u' && len(interm) == 0 && (prefix == '>' || prefix == '<' || prefix == '=') {
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
		}
		return
	}

	// DECSCUSR: CSI Ps SP q
	if final == 'q' && len(interm) == 1 && interm[0] == ' ' {
		shape := 0
		if len(params) > 0 {
			shape = params[0]
		}
		if shape >= 0 && shape <= 6 {
			scr.cursorShape = uint8(shape)
		}
		return
	}

	// modifyOtherKeys: CSI > 4 ; Pv m
	if final == 'm' && prefix == '>' {
		if len(params) >= 1 && params[0] == 4 {
			if len(params) >= 2 {
				vt.modifyOtherKeys = params[1]
			} else {
				vt.modifyOtherKeys = 0
			}
		}
		return
	}

	if prefix == '?' {
		vt.dispatchDECPrivate(params, final)
		return
	}
	if prefix != 0 {
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
		if scr.originMode {
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
		if scr.originMode {
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
		scr.curR += n
		if scr.curR >= scr.rows {
			scr.curR = scr.rows - 1
		}
	case 'F': // CPL - Cursor Preceding Line
		n := csiParam(params, 0, 1)
		if !vt.inAltScreen && n >= 2 {
			vt.snapshotBeforeUpwardRedraw()
			vt.sawUpwardRedraw = true
		}
		scr.pendingWrap = false
		scr.curC = 0
		scr.curR -= n
		if scr.curR < 0 {
			scr.curR = 0
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
		if scr.originMode {
			row += scr.scrollTop
		}
		if !vt.inAltScreen && row < scr.curR-1 {
			vt.snapshotBeforeUpwardRedraw()
			vt.sawUpwardRedraw = true
		}
		scr.pendingWrap = false
		if row < 0 {
			row = 0
		} else if row >= scr.rows {
			row = scr.rows - 1
		}
		scr.curR = row
	case 'H', 'f': // CUP / HVP - Cursor Position
		row := csiParam(params, 0, 1) - 1
		col := csiParam(params, 1, 1) - 1
		if scr.originMode {
			row += scr.scrollTop
		}
		if !vt.inAltScreen && row < scr.curR-1 {
			vt.snapshotBeforeUpwardRedraw()
			vt.sawUpwardRedraw = true
		}
		scr.pendingWrap = false
		if row < 0 {
			row = 0
		} else if row >= scr.rows {
			row = scr.rows - 1
		}
		if col < 0 {
			col = 0
		} else if col >= scr.cols {
			col = scr.cols - 1
		}
		scr.curR = row
		scr.curC = col
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
		if endC > scr.cols {
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
			vt.scrollDownRegion(scr, scr.curR, scr.scrollBottom, n)
		}
	case 'M': // DL - Delete Lines
		n := csiParam(params, 0, 1)
		if scr.curR >= scr.scrollTop && scr.curR <= scr.scrollBottom {
			scr.pendingWrap = false
			vt.scrollUpRegion(scr, scr.curR, scr.scrollBottom, n, false)
		}
	case '@': // ICH - Insert Characters
		n := csiParam(params, 0, 1)
		scr.pendingWrap = false
		if n > scr.cols-scr.curC {
			n = scr.cols - scr.curC
		}
		vt.clearWideOverlap(scr, scr.curR, scr.curC)
		cells := scr.grid[scr.curR].cells
		copy(cells[scr.curC+n:], cells[scr.curC:scr.cols-n])
		blank := blankVTermCell(vt.style)
		for c := scr.curC; c < scr.curC+n; c++ {
			cells[c] = blank
		}
		vt.clearWideOverlap(scr, scr.curR, scr.cols-1)
		scr.grid[scr.curR].dirty = true
	case 'P': // DCH - Delete Characters
		n := csiParam(params, 0, 1)
		scr.pendingWrap = false
		if n > scr.cols-scr.curC {
			n = scr.cols - scr.curC
		}
		vt.clearWideOverlap(scr, scr.curR, scr.curC)
		vt.clearWideOverlap(scr, scr.curR, scr.curC+n)
		cells := scr.grid[scr.curR].cells
		copy(cells[scr.curC:], cells[scr.curC+n:])
		blank := blankVTermCell(vt.style)
		for c := scr.cols - n; c < scr.cols; c++ {
			cells[c] = blank
		}
		scr.grid[scr.curR].dirty = true
	case 'S': // SU - Scroll Up
		n := csiParam(params, 0, 1)
		vt.scrollUpRegion(scr, scr.scrollTop, scr.scrollBottom, n, !vt.inAltScreen && scr.scrollTop == 0 && scr.scrollBottom == scr.rows-1)
	case 'T': // SD - Scroll Down
		n := csiParam(params, 0, 1)
		vt.scrollDownRegion(scr, scr.scrollTop, scr.scrollBottom, n)
	case 'b': // REP - Repeat preceding graphic character
		n := csiParam(params, 0, 1)
		if n > 4096 {
			n = 4096
		}
		if vt.lastPrintedCh != 0 {
			for i := 0; i < n; i++ {
				vt.putRune(vt.lastPrintedCh)
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
		vt.saveCursor()
	case 'u': // SCORC - Restore Cursor
		vt.restoreCursor()
	case 'm': // SGR
		vt.handleSGR(rawParams)
	}
}

func (vt *vterm) eraseInDisplay(scr *vtermScreen, mode int) {
	scr.pendingWrap = false
	blank := blankVTermCell(vt.style)
	switch mode {
	case 0: // Cursor to end of screen
		vt.eraseInLine(scr, 0)
		for r := scr.curR + 1; r < scr.rows; r++ {
			scr.grid[r] = newVTermRow(scr.cols, vt.style)
		}
	case 1: // Start of screen to cursor
		for r := 0; r < scr.curR; r++ {
			scr.grid[r] = newVTermRow(scr.cols, vt.style)
		}
		vt.eraseInLine(scr, 1)
	case 2: // Entire screen
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
		// Smart \x1b[3J protection:
		// Save current scrollback into savedScroll before resetting scrollback.
		// If the subsequent redraw is a full-history dump (e.g. agy on SIGWINCH) that
		// pushes >= len(savedScroll)/2 fresh scrollback lines, savedScroll is replaced
		// cleanly without duplication. Otherwise savedScroll is preserved so a bare
		// \x1b[3J screen clear never destroys user scrollback history.
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
	scr.pendingWrap = false
	blank := blankVTermCell(vt.style)
	row := &scr.grid[scr.curR]
	switch mode {
	case 0: // Cursor to end of line
		vt.clearWideOverlap(scr, scr.curR, scr.curC)
		for c := scr.curC; c < scr.cols; c++ {
			row.cells[c] = blank
		}
		row.wrapped = false
	case 1: // Start of line to cursor
		vt.clearWideOverlap(scr, scr.curR, scr.curC)
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
	if final != 'h' && final != 'l' {
		return
	}
	set := final == 'h'
	scr := vt.activeScreen()
	for _, p := range params {
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
		case 25: // DECTCEM - Cursor Visible
			scr.cursorVis = set
			if set && vt.inClearDump {
				vt.finalizeClearDump()
			}
		case 47, 1047: // Alternate Screen Buffer
			vt.switchAltScreen(set, false)
		case 1048: // Save/Restore Cursor
			if set {
				vt.saveCursor()
			} else {
				vt.restoreCursor()
			}
		case 1049: // Save Cursor + Alternate Screen Buffer
			vt.switchAltScreen(set, true)
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

func (vt *vterm) switchAltScreen(enter bool, saveRestoreCursor bool) {
	if enter {
		if saveRestoreCursor {
			vt.primary.savedCurR = vt.primary.curR
			vt.primary.savedCurC = vt.primary.curC
			vt.primary.savedStyle = vt.style
			vt.primary.savedOriginMode = vt.primary.originMode
			vt.primary.savedPendingWrap = vt.primary.pendingWrap
		}
		if !vt.inAltScreen {
			vt.inAltScreen = true
			vt.alt = newVTermScreen(vt.primary.rows, vt.primary.cols)
		}
	} else {
		if vt.inAltScreen {
			vt.inAltScreen = false
			for r := range vt.primary.grid {
				vt.primary.grid[r].dirty = true
			}
		}
		if saveRestoreCursor {
			vt.primary.curR = vt.primary.savedCurR
			vt.primary.curC = vt.primary.savedCurC
			vt.style = vt.primary.savedStyle
			vt.primary.originMode = vt.primary.savedOriginMode
			vt.primary.pendingWrap = vt.primary.savedPendingWrap
		}
	}
}

func (vt *vterm) saveCursor() {
	scr := vt.activeScreen()
	scr.savedCurR = scr.curR
	scr.savedCurC = scr.curC
	scr.savedStyle = vt.style
	scr.savedOriginMode = scr.originMode
	scr.savedPendingWrap = scr.pendingWrap
}

func (vt *vterm) restoreCursor() {
	scr := vt.activeScreen()
	scr.curR = scr.savedCurR
	scr.curC = scr.savedCurC
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
	vt.style = scr.savedStyle
	scr.originMode = scr.savedOriginMode
	scr.pendingWrap = scr.savedPendingWrap
}

func (vt *vterm) resetFull() {
	rows, cols := vt.primary.rows, vt.primary.cols
	vt.primary = newVTermScreen(rows, cols)
	vt.alt = newVTermScreen(rows, cols)
	vt.inAltScreen = false
	vt.style = vtermStyle{}
	vt.appCursorKeys = false
	vt.bracketedPaste = false
	vt.focusEvents = false
	vt.mouseMode = 0
	vt.mouseSGR = false
	vt.syncOutput = false
	vt.modifyOtherKeys = 0
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
		// Support colon subparameters such as 4:3 (curly underline), 38:2::r:g:b, 38:5:n
		if strings.Contains(tok, ":") {
			sub := strings.Split(tok, ":")
			code, _ := strconv.Atoi(sub[0])
			switch code {
			case 4:
				vt.style.flags &^= (vtermAttrUnderline | vtermAttrDoubleUnderline | vtermAttrCurlyUnderline)
				subKind := 1
				if len(sub) > 1 {
					subKind, _ = strconv.Atoi(sub[1])
				}
				switch subKind {
				case 0:
					// underline off
				case 2:
					vt.style.flags |= vtermAttrDoubleUnderline
				case 3, 4, 5:
					vt.style.flags |= vtermAttrCurlyUnderline
				default:
					vt.style.flags |= vtermAttrUnderline
				}
			case 38, 48, 58:
				var c vtermColor
				if len(sub) >= 3 && sub[1] == "5" {
					idx, _ := strconv.Atoi(sub[2])
					c = vtermColor{kind: vtermColorIndexed, index: uint8(idx)}
				} else if len(sub) >= 5 && sub[1] == "2" {
					offset := 2
					if len(sub) >= 6 {
						offset = 3
					}
					r, _ := strconv.Atoi(sub[offset])
					g, _ := strconv.Atoi(sub[offset+1])
					b, _ := strconv.Atoi(sub[offset+2])
					c = vtermColor{kind: vtermColorRGB, r: uint8(r), g: uint8(g), b: uint8(b)}
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
			vt.style.flags &^= (vtermAttrDoubleUnderline | vtermAttrCurlyUnderline)
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
			vt.style.flags &^= (vtermAttrUnderline | vtermAttrCurlyUnderline)
			vt.style.flags |= vtermAttrDoubleUnderline
		case n == 22:
			vt.style.flags &^= (vtermAttrBold | vtermAttrDim)
		case n == 23:
			vt.style.flags &^= vtermAttrItalic
		case n == 24:
			vt.style.flags &^= (vtermAttrUnderline | vtermAttrDoubleUnderline | vtermAttrCurlyUnderline)
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
					c := vtermColor{kind: vtermColorIndexed, index: uint8(idx)}
					if n == 38 {
						vt.style.fg = c
					} else if n == 48 {
						vt.style.bg = c
					} else {
						vt.style.ul = c
					}
					i += 2
				} else if mode == 2 && i+4 < len(tokens) {
					r, _ := strconv.Atoi(tokens[i+2])
					g, _ := strconv.Atoi(tokens[i+3])
					b, _ := strconv.Atoi(tokens[i+4])
					c := vtermColor{kind: vtermColorRGB, r: uint8(r), g: uint8(g), b: uint8(b)}
					if n == 38 {
						vt.style.fg = c
					} else if n == 48 {
						vt.style.bg = c
					} else {
						vt.style.ul = c
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
		case n == 59:
			vt.style.ul = vtermColor{kind: vtermColorDefault}
		case n >= 90 && n <= 97:
			vt.style.fg = vtermColor{kind: vtermColorIndexed, index: uint8(n - 90 + 8)}
		case n >= 100 && n <= 107:
			vt.style.bg = vtermColor{kind: vtermColorIndexed, index: uint8(n - 100 + 8)}
		}
	}
}

// Resize updates the dimensions of both primary and alternate screens while
// preserving scrollback and pushing excess primary top rows into scrollback when shrinking vertically.
func (vt *vterm) Resize(rows, cols int) {
	if rows < 1 {
		rows = 1
	}
	if cols < 1 {
		cols = 1
	}
	vt.hasPreRedraw = false
	vt.sawUpwardRedraw = false
	vt.resizeScreen(&vt.primary, rows, cols, true)
	vt.resizeScreen(&vt.alt, rows, cols, false)
}

func (vt *vterm) resizeScreen(scr *vtermScreen, newRows, newCols int, isPrimary bool) {
	if scr.rows == newRows && scr.cols == newCols {
		return
	}

	var defStyle vtermStyle
	// Adjust row count first.
	if newRows < scr.rows {
		drop := scr.rows - newRows
		// Drop blank bottom rows below cursor first before pushing top rows into scrollback.
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
		// If primary screen has scrollback and cursor was near bottom, pull lines back or append blank rows.
		for i := 0; i < add; i++ {
			scr.grid = append(scr.grid, newVTermRow(scr.cols, defStyle))
		}
	}

	// Adjust column count on each row.
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

	scr.rows = newRows
	scr.cols = newCols
	scr.scrollTop = 0
	scr.scrollBottom = newRows - 1
	if scr.curR >= newRows {
		scr.curR = newRows - 1
	}
	if scr.curC >= newCols {
		scr.curC = newCols - 1
	}
	scr.pendingWrap = false
}

// ViewportRow returns the resolved vtermRow for viewport row r (0..rows-1),
// taking vt.scrollOffset into account when scrolled back on the primary screen.
func (vt *vterm) ViewportRow(r int) vtermRow {
	scr := vt.activeScreen()
	if r < 0 || r >= scr.rows {
		return newVTermRow(scr.cols, vtermStyle{})
	}
	if vt.inAltScreen || vt.scrollOffset <= 0 {
		return scr.grid[r]
	}
	sb := vt.effectiveScrollback()
	sbLen := len(sb)
	idx := sbLen - vt.scrollOffset + r
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
		return []byte("\x1b]7502;vterm=1\x07")
	}
	var buf bytes.Buffer
	buf.WriteString("\x1b]7502;vterm=1\x07\x1b[0m\x1b[H")
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

// SnapshotANSI serializes the complete virtual terminal state (scrollback + active
// screen + cursor + modes + OSC 7502 vterm=1 marker) into a single self-contained
// ANSI stream so newly attached clients can reconstruct the exact vterm state.
func (vt *vterm) SnapshotANSI() []byte {
	var buf bytes.Buffer
	buf.WriteString("\x1b]7502;vterm=1\x07")
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

	buf.WriteString("\x1b[?2026h\x1b[?25l\x1b[0m\x1b[H\x1b[2J")

	// 1. Emit scrollback + primary screen on the primary buffer first.
	for _, row := range vt.effectiveScrollback() {
		buf.WriteString(encodeVTermRowANSI(row, vt.primary.cols))
		buf.WriteString("\r\n")
	}
	for r := 0; r < vt.primary.rows; r++ {
		fmt.Fprintf(&buf, "\x1b[%d;1H\x1b[2K", r+1)
		buf.WriteString(encodeVTermRowANSI(vt.primary.grid[r], vt.primary.cols))
	}
	if vt.primary.scrollTop != 0 || vt.primary.scrollBottom != vt.primary.rows-1 {
		fmt.Fprintf(&buf, "\x1b[%d;%dr", vt.primary.scrollTop+1, vt.primary.scrollBottom+1)
	}
	fmt.Fprintf(&buf, "\x1b[%d;%dH", vt.primary.curR+1, vt.primary.curC+1)

	// 2. If currently in alternate screen, enter ?1049h and paint alt screen.
	if vt.inAltScreen {
		buf.WriteString("\x1b[?1049h\x1b[0m\x1b[H\x1b[2J")
		for r := 0; r < vt.alt.rows; r++ {
			fmt.Fprintf(&buf, "\x1b[%d;1H\x1b[2K", r+1)
			buf.WriteString(encodeVTermRowANSI(vt.alt.grid[r], vt.alt.cols))
		}
		if vt.alt.scrollTop != 0 || vt.alt.scrollBottom != vt.alt.rows-1 {
			fmt.Fprintf(&buf, "\x1b[%d;%dr", vt.alt.scrollTop+1, vt.alt.scrollBottom+1)
		}
		fmt.Fprintf(&buf, "\x1b[%d;%dH", vt.alt.curR+1, vt.alt.curC+1)
	}

	scr := vt.activeScreen()
	if scr.cursorShape > 0 {
		fmt.Fprintf(&buf, "\x1b[%d q", scr.cursorShape)
	}
	if vt.appCursorKeys {
		buf.WriteString("\x1b[?1h")
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
	if scr.currentKittyFlags() > 0 {
		fmt.Fprintf(&buf, "\x1b[=%d;1u", scr.currentKittyFlags())
	}
	if vt.modifyOtherKeys > 0 {
		fmt.Fprintf(&buf, "\x1b[>4;%dm", vt.modifyOtherKeys)
	}
	if scr.cursorVis {
		buf.WriteString("\x1b[?25h")
	}
	buf.WriteString("\x1b[?2026l")
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
	if prev.hyperlink != next.hyperlink {
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
	if next.flags&vtermAttrCurlyUnderline != 0 {
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
