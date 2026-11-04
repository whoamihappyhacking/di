package main

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	scrollbarTrackChar = '▕' // U+2595 Right One Eighth Block
	scrollbarThumbChar = '▐' // U+2590 Right Half Block
	scrollbarTrackFg   = 238
	scrollbarThumbFg   = 245

	syncOutputFailOpenTimeout = 100 * time.Millisecond
)

type scrollbarThumb struct {
	offset int
	size   int
}

func computeScrollbarThumb(metrics vtermScrollMetrics) (scrollbarThumb, bool) {
	if metrics.ScrollOffset <= 0 || metrics.ViewportRows <= 0 || metrics.TotalRows <= metrics.ViewportRows {
		return scrollbarThumb{}, false
	}
	v := metrics.ViewportRows
	t := metrics.TotalRows
	thumbSize := (v*v + t/2) / t
	if thumbSize < 1 {
		thumbSize = 1
	}
	if thumbSize > v {
		thumbSize = v
	}
	maxOffset := t - v
	scrollableTrack := v - thumbSize
	distFromTop := maxOffset - metrics.ScrollOffset
	if distFromTop < 0 {
		distFromTop = 0
	}
	thumbOffset := 0
	if maxOffset > 0 && scrollableTrack > 0 {
		thumbOffset = (distFromTop*scrollableTrack + maxOffset/2) / maxOffset
	}
	return scrollbarThumb{
		offset: thumbOffset,
		size:   thumbSize,
	}, true
}

// BlitEncoder diffs the authoritative vterm state against the last rendered frame
// and encodes minimal ANSI updates wrapped in DEC 2026 synchronized output,
// modeled after herdr's BlitEncoder (src/protocol/render_ansi.rs).
type BlitEncoder struct {
	initialized     bool
	rows            int
	cols            int
	cells           []vtermCell
	inAltScreen     bool
	scrollOffset    int
	curR            int
	curC            int
	cursorVis       bool
	cursorShape     uint8
	autoWrap        bool
	appCursorKeys   bool
	appKeypad       bool
	bracketedPaste  bool
	focusEvents     bool
	mouseMode       int
	mouseSGR        bool
	kittyFlags      uint8
	modifyOtherKeys int
	title           string
	cwd             string
	agentState      string
	progressSeq     string
}

func newBlitEncoder() *BlitEncoder {
	return &BlitEncoder{
		autoWrap: true,
	}
}

func (b *BlitEncoder) Reset() {
	b.initialized = false
	b.cells = nil
}

func (b *BlitEncoder) buildTargetGrid(vt *vterm) (rows, cols int, grid []vtermCell) {
	rows, cols = vt.Size()
	grid = make([]vtermCell, rows*cols)
	var defStyle vtermStyle
	blank := blankVTermCell(defStyle)
	sb := vt.effectiveScrollback()
	for r := 0; r < rows; r++ {
		vrow := vt.viewportRowWithScrollback(r, sb)
		rowSlice := grid[r*cols : (r+1)*cols]
		copy(rowSlice, vrow.cells)
		for c := len(vrow.cells); c < cols; c++ {
			rowSlice[c] = blank
		}
	}

	// Overlay herdr-style scrollbar on the rightmost column when scrolled back.
	if thumb, ok := computeScrollbarThumb(vt.ScrollMetrics()); ok && cols >= 2 {
		for r := 0; r < rows; r++ {
			idx := r*cols + (cols - 1)
			// Clean up wide char leader if it straddled cols-2 and cols-1.
			if grid[idx].width == 0 && cols >= 2 && grid[idx-1].width == 2 {
				grid[idx-1] = blankVTermCell(grid[idx-1].style)
			}
			ch := scrollbarTrackChar
			fg := uint8(scrollbarTrackFg)
			if r >= thumb.offset && r < thumb.offset+thumb.size {
				ch = scrollbarThumbChar
				fg = uint8(scrollbarThumbFg)
			}
			grid[idx] = vtermCell{
				ch:    ch,
				width: 1,
				style: vtermStyle{
					fg: vtermColor{kind: vtermColorIndexed, index: fg},
				},
			}
		}
	}
	return rows, cols, grid
}

// Encode produces the minimal synchronized ANSI byte stream needed to bring the
// attached client terminal in sync with vt. Returns nil if nothing changed.
func (b *BlitEncoder) Encode(vt *vterm, forceFull bool, now time.Time) []byte {
	rows, cols, target := b.buildTargetGrid(vt)
	scr := vt.activeScreen()

	var settledR, settledC int
	var settledVis bool
	if vt.IsScrolledBack() {
		settledR, settledC, settledVis = vt.cursorSettle.ForceHidden(scr.curR, scr.curC)
	} else {
		settledR, settledC, settledVis = vt.cursorSettle.Observe(scr.curR, scr.curC, scr.cursorVis, now)
	}
	settledR = clampInt(settledR, 0, rows-1)
	settledC = clampInt(settledC, 0, cols-1)

	passthrough := vt.DrainPassthrough()
	replaced := vt.ConsumeFullScrollbackReplaced()
	needFull := forceFull || replaced || !b.initialized || b.rows != rows || b.cols != cols || b.inAltScreen != vt.InAltScreen()

	if needFull {
		vt.DrainPushedScrollback()
		return b.encodeFull(vt, rows, cols, target, settledR, settledC, settledVis, passthrough)
	}

	return b.encodeDiff(vt, rows, cols, target, settledR, settledC, settledVis, passthrough)
}

func (b *BlitEncoder) encodeFull(
	vt *vterm,
	rows, cols int,
	target []vtermCell,
	settledR, settledC int,
	settledVis bool,
	passthrough []byte,
) []byte {
	var buf bytes.Buffer
	buf.Write(passthrough)

	scr := vt.activeScreen()
	b.writeMetadataChanges(&buf, vt, true)

	buf.WriteString("\x1b[?2026h\x1b[?25l\x1b]8;;\x1b\\\x1b[0m\x1b[r\x1b[?6l\x1b[?7l")

	if vt.InAltScreen() {
		// If forcing a full repaint while in alt screen, also refresh primary scrollback
		// first if we weren't already in alt screen.
		if !b.initialized {
			buf.WriteString("\x1b[?1049l\x1b[H\x1b[2J\x1b[3J")
			sb := vt.effectiveScrollback()
			if len(sb) > 0 {
				for _, row := range sb {
					buf.WriteString(encodeVTermRowANSI(row, cols))
					buf.WriteString("\r\n")
				}
				for r := 0; r < vt.primary.rows-1; r++ {
					buf.WriteString("\r\n")
				}
			}
			for r := 0; r < vt.primary.rows; r++ {
				fmt.Fprintf(&buf, "\x1b[%d;1H\x1b[2K", r+1)
				buf.WriteString(encodeVTermRowANSI(vt.primary.grid[r], cols))
			}
		}
		buf.WriteString("\x1b[?1049h\x1b[0m\x1b[r\x1b[?6l\x1b[?7l\x1b[H\x1b[2J")
	} else {
		if b.initialized && b.inAltScreen {
			buf.WriteString("\x1b[?1049l")
		}
		// Repaint native scrollback + active viewport atomically inside ?2026h.
		buf.WriteString("\x1b[0m\x1b[r\x1b[?6l\x1b[?7l\x1b[H\x1b[2J\x1b[3J")
		sb := vt.effectiveScrollback()
		if len(sb) > 0 {
			for _, row := range sb {
				buf.WriteString(encodeVTermRowANSI(row, cols))
				buf.WriteString("\r\n")
			}
			// Flush the last rows-1 scrollback lines off the visible screen into native scrollback!
			for r := 0; r < rows-1; r++ {
				buf.WriteString("\r\n")
			}
		}
	}

	var curStyle vtermStyle
	var defStyle vtermStyle
	hasStyle := false
	for r := 0; r < rows; r++ {
		if hasStyle && curStyle != defStyle {
			if curStyle.hyperlink != "" {
				buf.WriteString("\x1b]8;;\x1b\\")
			}
			buf.WriteString("\x1b[0m")
			curStyle = defStyle
			hasStyle = false
		}
		fmt.Fprintf(&buf, "\x1b[%d;1H\x1b[2K", r+1)
		rowCells := target[r*cols : (r+1)*cols]
		// Find last non-blank cell on this row.
		end := cols
		for end > 0 {
			c := rowCells[end-1]
			if c.width == 1 && (c.ch == ' ' || c.ch == 0) && c.combining == "" && c.style == defStyle {
				end--
			} else {
				break
			}
		}
		for c := 0; c < end; c++ {
			cell := rowCells[c]
			if cell.width == 0 {
				continue
			}
			if cell.width == 2 && c+1 >= cols {
				break
			}
			if !hasStyle || cell.style != curStyle {
				writeVTermStyleTransition(&buf, curStyle, cell.style, !hasStyle)
				curStyle = cell.style
				hasStyle = true
			}
			if cell.ch == 0 {
				buf.WriteByte(' ')
			} else {
				buf.WriteRune(cell.ch)
			}
			if cell.combining != "" {
				buf.WriteString(cell.combining)
			}
		}
	}
	if hasStyle && curStyle != defStyle {
		if curStyle.hyperlink != "" {
			buf.WriteString("\x1b]8;;\x1b\\")
		}
		buf.WriteString("\x1b[0m")
	}

	b.writeTerminalModes(&buf, vt, scr, true)
	fmt.Fprintf(&buf, "\x1b[%d;%dH", settledR+1, settledC+1)
	if settledVis {
		buf.WriteString("\x1b[?25h")
	} else {
		buf.WriteString("\x1b[?25l")
	}
	buf.WriteString("\x1b[?2026l")
	if settledVis {
		fmt.Fprintf(&buf, "\x1b[%d;%dH", settledR+1, settledC+1)
	}

	b.initialized = true
	b.rows = rows
	b.cols = cols
	b.cells = make([]vtermCell, len(target))
	copy(b.cells, target)
	b.inAltScreen = vt.InAltScreen()
	b.scrollOffset = vt.ScrollOffset()
	b.curR = settledR
	b.curC = settledC
	b.cursorVis = settledVis
	return buf.Bytes()
}

func (b *BlitEncoder) encodeDiff(
	vt *vterm,
	rows, cols int,
	target []vtermCell,
	settledR, settledC int,
	settledVis bool,
	passthrough []byte,
) []byte {
	scr := vt.activeScreen()

	var scrollBuf bytes.Buffer
	// If on the live primary screen (ScrollOffset() == 0) and new rows were pushed into
	// scrollback, push those exact rows into the host terminal's native scrollback buffer
	// and update b.cells to match the resulting host screen!
	if !vt.InAltScreen() && vt.ScrollOffset() == 0 {
		pushed := vt.DrainPushedScrollback()
		if len(pushed) > 0 {
			scrollBuf.WriteString("\x1b]8;;\x1b\\\x1b[0m\x1b[r\x1b[?6l\x1b[?7l")
			if len(pushed) < rows {
				k := len(pushed)
				// Ensure top k rows on the host screen match pushed[0..k-1] before scrolling them off.
				for i := 0; i < k; i++ {
					rowStr := encodeVTermRowANSI(pushed[i], cols)
					fmt.Fprintf(&scrollBuf, "\x1b[%d;1H\x1b[2K%s", i+1, rowStr)
				}
				fmt.Fprintf(&scrollBuf, "\x1b[%d;1H", rows)
				for i := 0; i < k; i++ {
					scrollBuf.WriteString("\r\n")
				}
				// Shift b.cells up by k rows.
				copy(b.cells, b.cells[k*cols:])
				blank := blankVTermCell(vtermStyle{})
				for i := (rows - k) * cols; i < rows*cols; i++ {
					b.cells[i] = blank
				}
			} else {
				// At least a full screen of lines scrolled: paint the first `rows` lines on rows 1..rows,
				// stream any additional lines at row `rows`, and then emit `rows` CRLFs to flush all of
				// them off the top of the screen into native scrollback, leaving the host screen blank.
				for i := 0; i < rows; i++ {
					rowStr := encodeVTermRowANSI(pushed[i], cols)
					fmt.Fprintf(&scrollBuf, "\x1b[%d;1H\x1b[2K%s", i+1, rowStr)
				}
				for i := rows; i < len(pushed); i++ {
					rowStr := encodeVTermRowANSI(pushed[i], cols)
					fmt.Fprintf(&scrollBuf, "\r\n%s", rowStr)
				}
				fmt.Fprintf(&scrollBuf, "\x1b[%d;1H", rows)
				for i := 0; i < rows; i++ {
					scrollBuf.WriteString("\r\n")
				}
				blank := blankVTermCell(vtermStyle{})
				for i := range b.cells {
					b.cells[i] = blank
				}
			}
			if scr.autoWrap {
				scrollBuf.WriteString("\x1b[?7h")
			}
		}
	}

	var cellBuf bytes.Buffer
	cellsChanged := b.writeChangedCells(&cellBuf, rows, cols, target, scr.autoWrap)

	var metaBuf bytes.Buffer
	b.writeMetadataChanges(&metaBuf, vt, false)

	var modeBuf bytes.Buffer
	b.writeTerminalModes(&modeBuf, vt, scr, false)

	cursorMoved := settledR != b.curR || settledC != b.curC
	cursorVisChanged := settledVis != b.cursorVis
	b.scrollOffset = vt.ScrollOffset()

	if len(passthrough) == 0 &&
		scrollBuf.Len() == 0 &&
		!cellsChanged &&
		metaBuf.Len() == 0 &&
		modeBuf.Len() == 0 &&
		!cursorMoved &&
		!cursorVisChanged {
		return nil
	}

	var out bytes.Buffer
	out.Write(passthrough)
	out.Write(metaBuf.Bytes())

	out.WriteString("\x1b[?2026h\x1b[?25l")
	out.Write(scrollBuf.Bytes())
	out.Write(cellBuf.Bytes())
	out.Write(modeBuf.Bytes())

	if settledVis {
		fmt.Fprintf(&out, "\x1b[%d;%dH\x1b[?25h", settledR+1, settledC+1)
	} else if cursorMoved || cellsChanged || scrollBuf.Len() > 0 || modeBuf.Len() > 0 {
		fmt.Fprintf(&out, "\x1b[%d;%dH", settledR+1, settledC+1)
	}
	out.WriteString("\x1b[?2026l")

	// Post-sync IME cursor anchor when visible cursor moved or cells were redrawn.
	if settledVis && (cursorMoved || cellsChanged || scrollBuf.Len() > 0) {
		fmt.Fprintf(&out, "\x1b[%d;%dH", settledR+1, settledC+1)
	}

	copy(b.cells, target)
	b.curR = settledR
	b.curC = settledC
	b.cursorVis = settledVis
	return out.Bytes()
}

// writeChangedCells diffs b.cells against target and writes minimal CUP + SGR + glyph
// transitions into buf, handling wide-character pair invalidation cleanly.
func (b *BlitEncoder) writeChangedCells(buf *bytes.Buffer, rows, cols int, target []vtermCell, restoreAutoWrap bool) bool {
	changed := false
	var curStyle vtermStyle
	hasStyle := false
	termR, termC := -1, -1

	for r := 0; r < rows; r++ {
		rowStart := r * cols
		for c := 0; c < cols; c++ {
			idx := rowStart + c
			next := target[idx]
			prev := b.cells[idx]

			// Wide continuation cell is painted by its leader at c-1.
			if next.width == 0 {
				continue
			}

			cellDirty := next != prev
			// If this is a wide character leader, also check if its continuation cell changed.
			if !cellDirty && next.width == 2 && c+1 < cols {
				if target[idx+1] != b.cells[idx+1] {
					cellDirty = true
				}
			}
			// If previous frame had a wide leader at idx and now it's width 1, ensure c+1 is also invalidated.
			if prev.width == 2 && next.width == 1 && c+1 < cols {
				b.cells[idx+1].width = 255
			}

			if !cellDirty {
				continue
			}

			if !changed {
				changed = true
				buf.WriteString("\x1b[?7l")
			}
			if termR != r || termC != c {
				fmt.Fprintf(buf, "\x1b[%d;%dH", r+1, c+1)
				termR = r
				termC = c
			}
			if !hasStyle || next.style != curStyle {
				writeVTermStyleTransition(buf, curStyle, next.style, !hasStyle)
				curStyle = next.style
				hasStyle = true
			}
			if next.ch == 0 {
				buf.WriteByte(' ')
			} else {
				buf.WriteRune(next.ch)
			}
			if next.combining != "" {
				buf.WriteString(next.combining)
			}
			step := int(next.width)
			if step < 1 {
				step = 1
			}
			termC += step
			if termC >= cols {
				// Terminal cursor at right margin may enter pending-wrap state; force explicit CUP next.
				termR = -1
				termC = -1
			}
		}
	}

	if hasStyle {
		var defStyle vtermStyle
		if curStyle != defStyle {
			if curStyle.hyperlink != "" {
				buf.WriteString("\x1b]8;;\x1b\\")
			}
			buf.WriteString("\x1b[0m")
		}
	}
	if changed && restoreAutoWrap {
		buf.WriteString("\x1b[?7h")
	}
	return changed
}

func (b *BlitEncoder) writeMetadataChanges(buf *bytes.Buffer, vt *vterm, force bool) {
	if (force && vt.title != "") || (!force && vt.title != b.title) {
		buf.WriteString("\x1b]2;")
		buf.WriteString(vt.title)
		buf.WriteByte(0x07)
	}
	b.title = vt.title

	if (force && vt.cwd != "") || (!force && vt.cwd != b.cwd && vt.cwd != "") {
		buf.WriteString("\x1b]7;")
		buf.WriteString(vt.cwd)
		buf.WriteByte(0x07)
	}
	b.cwd = vt.cwd

	if (force && vt.agentState != "") || (!force && vt.agentState != b.agentState) {
		buf.WriteString("\x1b]7501;")
		if vt.agentState == "" {
			buf.WriteString("state=clear")
		} else {
			buf.WriteString(vt.agentState)
		}
		buf.WriteByte(0x07)
	}
	b.agentState = vt.agentState

	if (force && vt.progressSeq != "") || (!force && vt.progressSeq != b.progressSeq) {
		buf.WriteString("\x1b]9;")
		if vt.progressSeq == "" {
			buf.WriteString("4;0")
		} else {
			buf.WriteString(vt.progressSeq)
		}
		buf.WriteByte(0x07)
	}
	b.progressSeq = vt.progressSeq
}

func (b *BlitEncoder) writeTerminalModes(buf *bytes.Buffer, vt *vterm, scr *vtermScreen, force bool) {
	if force || scr.cursorShape != b.cursorShape {
		fmt.Fprintf(buf, "\x1b[%d q", scr.cursorShape)
		b.cursorShape = scr.cursorShape
	}

	if force || scr.autoWrap != b.autoWrap {
		if scr.autoWrap {
			buf.WriteString("\x1b[?7h")
		} else {
			buf.WriteString("\x1b[?7l")
		}
		b.autoWrap = scr.autoWrap
	}

	if force || vt.appCursorKeys != b.appCursorKeys {
		if vt.appCursorKeys {
			buf.WriteString("\x1b[?1h")
		} else {
			buf.WriteString("\x1b[?1l")
		}
		b.appCursorKeys = vt.appCursorKeys
	}

	if force || vt.appKeypad != b.appKeypad {
		if vt.appKeypad {
			buf.WriteString("\x1b=")
		} else {
			buf.WriteString("\x1b>")
		}
		b.appKeypad = vt.appKeypad
	}

	if force || vt.bracketedPaste != b.bracketedPaste {
		if vt.bracketedPaste {
			buf.WriteString("\x1b[?2004h")
		} else {
			buf.WriteString("\x1b[?2004l")
		}
		b.bracketedPaste = vt.bracketedPaste
	}

	if force || vt.focusEvents != b.focusEvents {
		if vt.focusEvents {
			buf.WriteString("\x1b[?1004h")
		} else {
			buf.WriteString("\x1b[?1004l")
		}
		b.focusEvents = vt.focusEvents
	}

	if force || vt.mouseMode != b.mouseMode {
		if force && vt.mouseMode == 0 {
			buf.WriteString("\x1b[?1000l\x1b[?1002l\x1b[?1003l")
		} else if b.mouseMode > 0 && b.mouseMode != vt.mouseMode {
			fmt.Fprintf(buf, "\x1b[?%dl", b.mouseMode)
		}
		if vt.mouseMode > 0 {
			fmt.Fprintf(buf, "\x1b[?%dh", vt.mouseMode)
		}
		b.mouseMode = vt.mouseMode
	}

	if force || vt.mouseSGR != b.mouseSGR {
		if vt.mouseSGR {
			buf.WriteString("\x1b[?1006h")
		} else {
			buf.WriteString("\x1b[?1006l")
		}
		b.mouseSGR = vt.mouseSGR
	}

	kitty := scr.currentKittyFlags()
	if kitty != b.kittyFlags || (force && kitty > 0) {
		if b.kittyFlags > 0 {
			buf.WriteString("\x1b[<1u")
		}
		if kitty > 0 {
			fmt.Fprintf(buf, "\x1b[>%du", kitty)
		}
		b.kittyFlags = kitty
	}

	if vt.modifyOtherKeys != b.modifyOtherKeys || (force && vt.modifyOtherKeys > 0) {
		if vt.modifyOtherKeys > 0 {
			fmt.Fprintf(buf, "\x1b[>4;%dm", vt.modifyOtherKeys)
		} else if b.modifyOtherKeys > 0 {
			buf.WriteString("\x1b[>4;0m")
		}
		b.modifyOtherKeys = vt.modifyOtherKeys
	}
}

// clientSessionRenderer manages the per-attachment vterm state machine,
// BlitEncoder diff engine, cursor settle timer, and viewport scrollback.
type clientSessionRenderer struct {
	mu               sync.Mutex
	dst              io.Writer
	vt               *vterm
	blit             *BlitEncoder
	tracker          *redrawTracker
	replyFn          func([]byte)
	settleTimer      *time.Timer
	syncTimer        *time.Timer
	inBracketedPaste bool
	closed           bool
}

func newClientSessionRenderer(dst io.Writer, rows, cols int, tracker *redrawTracker) *clientSessionRenderer {
	if rows <= 0 {
		rows = defaultVTermRows
	}
	if cols <= 0 {
		cols = defaultVTermCols
	}
	return &clientSessionRenderer{
		dst:     dst,
		vt:      newVTerm(rows, cols),
		blit:    newBlitEncoder(),
		tracker: tracker,
	}
}

func (r *clientSessionRenderer) SetReplyWriter(fn func([]byte)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replyFn = fn
}

func (r *clientSessionRenderer) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if r.settleTimer != nil {
		r.settleTimer.Stop()
		r.settleTimer = nil
	}
	if r.syncTimer != nil {
		r.syncTimer.Stop()
		r.syncTimer = nil
	}
	var restore bytes.Buffer
	if r.blit.kittyFlags > 0 {
		restore.WriteString("\x1b[<1u")
		r.blit.kittyFlags = 0
	}
	if r.blit.modifyOtherKeys > 0 {
		restore.WriteString("\x1b[>4;0m")
		r.blit.modifyOtherKeys = 0
	}
	if restore.Len() > 0 && r.dst != nil {
		_, _ = r.dst.Write(restore.Bytes())
	}
}

func (r *clientSessionRenderer) SawServerVTerm() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.vt.SawServerVTerm()
}

func (r *clientSessionRenderer) HasServerScrollback() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.vt.SawServerVTerm() || r.vt.ScrollbackLen() > 0
}

func (r *clientSessionRenderer) writeLocked(out []byte) error {
	if len(out) == 0 {
		return nil
	}
	if _, err := r.dst.Write(out); err != nil {
		r.blit.Reset()
		return err
	}
	return nil
}

func (r *clientSessionRenderer) ProcessChunk(chunk []byte) error {
	if len(chunk) == 0 {
		return nil
	}
	if r.tracker != nil {
		r.tracker.observe(chunk)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.vt.Write(chunk)
	replies := r.vt.DrainReplies()
	if len(replies) > 0 && !r.vt.SawServerQueries() && r.replyFn != nil {
		// Legacy servers (prior to SnapshotANSI snap=end) only answer ?2026$p themselves;
		// answer other terminal queries (e.g. ?u, 6n, 5n, c, >c) from the client mirror.
		filtered := bytes.ReplaceAll(replies, []byte("\x1b[?2026;2$y"), nil)
		if len(filtered) > 0 {
			r.replyFn(filtered)
		}
	}

	if r.vt.syncOutput {
		r.scheduleSyncFailOpenLocked()
		return nil
	}
	if r.syncTimer != nil {
		r.syncTimer.Stop()
	}

	now := time.Now()
	out := r.blit.Encode(r.vt, false, now)
	r.scheduleCursorSettleLocked(now)
	return r.writeLocked(out)
}

func (r *clientSessionRenderer) RepaintFull() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	now := time.Now()
	out := r.blit.Encode(r.vt, true, now)
	r.scheduleCursorSettleLocked(now)
	return r.writeLocked(out)
}

func (r *clientSessionRenderer) ResizeAndRepaint(rows, cols int) (sizeChanged bool, hasScrollback bool, err error) {
	if rows <= 0 || cols <= 0 {
		return false, false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false, false, nil
	}
	oldRows, oldCols := r.vt.Size()
	sizeChanged = rows != oldRows || cols != oldCols
	if sizeChanged {
		r.vt.Resize(rows, cols)
		r.blit.Reset()
	}
	hasScrollback = r.vt.SawServerVTerm() || r.vt.ScrollbackLen() > 0
	now := time.Now()
	out := r.blit.Encode(r.vt, true, now)
	r.scheduleCursorSettleLocked(now)
	err = r.writeLocked(out)
	return sizeChanged, hasScrollback, err
}

func (r *clientSessionRenderer) scheduleSyncFailOpenLocked() {
	if r.syncTimer == nil {
		r.syncTimer = time.AfterFunc(syncOutputFailOpenTimeout, r.onSyncTimeout)
	} else {
		r.syncTimer.Reset(syncOutputFailOpenTimeout)
	}
}

func (r *clientSessionRenderer) onSyncTimeout() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.vt.syncOutput {
		return
	}
	r.vt.syncOutput = false
	if r.vt.sawUpwardRedraw {
		r.vt.commitInPlaceScrollback()
	}
	if r.vt.inClearDump {
		r.vt.finalizeClearDump()
	}
	now := time.Now()
	out := r.blit.Encode(r.vt, false, now)
	r.scheduleCursorSettleLocked(now)
	_ = r.writeLocked(out)
}

func (r *clientSessionRenderer) scheduleCursorSettleLocked(now time.Time) {
	deadline, ok := r.vt.cursorSettle.NextDeadline()
	if !ok {
		if r.settleTimer != nil {
			r.settleTimer.Stop()
		}
		return
	}
	wait := deadline.Sub(now)
	if wait < time.Millisecond {
		wait = time.Millisecond
	}
	if r.settleTimer == nil {
		r.settleTimer = time.AfterFunc(wait, r.onCursorSettleTimer)
	} else {
		r.settleTimer.Reset(wait)
	}
}

func (r *clientSessionRenderer) onCursorSettleTimer() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	now := time.Now()
	if !r.vt.cursorSettle.Advance(now) {
		r.scheduleCursorSettleLocked(now)
		return
	}
	out := r.blit.Encode(r.vt, false, now)
	r.scheduleCursorSettleLocked(now)
	_ = r.writeLocked(out)
}

// HandleInput processes local scrollback navigation keys (PageUp/PageDown/Wheel)
// and resets the scrollback offset on normal keystrokes.
func (r *clientSessionRenderer) HandleInput(chunk []byte) (consumed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}

	// Track bracketed paste boundaries so pasted sequences are never intercepted as local navigation.
	if bytes.Contains(chunk, []byte("\x1b[200~")) {
		r.inBracketedPaste = true
	}
	if r.inBracketedPaste {
		if bytes.Contains(chunk, []byte("\x1b[201~")) {
			r.inBracketedPaste = false
		}
		r.vt.cursorSettle.OnInput()
		if r.vt.ScrollReset() {
			now := time.Now()
			out := r.blit.Encode(r.vt, false, now)
			r.scheduleCursorSettleLocked(now)
			_ = r.writeLocked(out)
		}
		return false
	}

	rows, _ := r.vt.Size()
	halfPage := rows / 2
	if halfPage < 1 {
		halfPage = 1
	}

	if dir, shiftHeld, count := parseMouseWheelEvents(chunk); dir != 0 {
		// Forward mouse wheel events to the child app when in alternate screen or when
		// child mouse tracking is enabled (unless Shift is held to force local scrollback).
		if (r.vt.InAltScreen() || r.vt.mouseMode != 0) && !shiftHeld {
			return false
		}
		var scrolled bool
		if !r.vt.InAltScreen() && r.vt.ScrollbackLen() > 0 {
			lines := 3 * count
			if dir < 0 {
				scrolled = r.vt.ScrollUp(lines)
			} else {
				scrolled = r.vt.ScrollDown(lines)
			}
		}
		if scrolled {
			now := time.Now()
			out := r.blit.Encode(r.vt, false, now)
			r.scheduleCursorSettleLocked(now)
			_ = r.writeLocked(out)
		}
		return true
	}

	if !r.vt.InAltScreen() && r.vt.ScrollbackLen() > 0 {
		allowLocalPage := (!r.vt.appCursorKeys && r.vt.mouseMode == 0) || r.vt.IsScrolledBack()
		if isShiftPageUpInput(chunk) || (allowLocalPage && isPageUpInput(chunk)) {
			if r.vt.ScrollUp(halfPage) {
				now := time.Now()
				out := r.blit.Encode(r.vt, false, now)
				r.scheduleCursorSettleLocked(now)
				_ = r.writeLocked(out)
			}
			return true
		}
		if (isShiftPageDownInput(chunk) || (allowLocalPage && isPageDownInput(chunk))) && r.vt.IsScrolledBack() {
			if r.vt.ScrollDown(halfPage) {
				now := time.Now()
				out := r.blit.Encode(r.vt, false, now)
				r.scheduleCursorSettleLocked(now)
				_ = r.writeLocked(out)
			}
			return true
		}
	}

	_, hadPending := r.vt.cursorSettle.NextDeadline()
	r.vt.cursorSettle.OnInput()
	reset := r.vt.ScrollReset()
	if reset || hadPending {
		now := time.Now()
		out := r.blit.Encode(r.vt, false, now)
		r.scheduleCursorSettleLocked(now)
		_ = r.writeLocked(out)
	}
	return false
}

func isPageUpInput(buf []byte) bool {
	return bytes.Equal(buf, []byte("\x1b[5~")) || bytes.Equal(buf, []byte("\x1b[5;2~"))
}

func isShiftPageUpInput(buf []byte) bool {
	return bytes.Equal(buf, []byte("\x1b[5;2~"))
}

func isPageDownInput(buf []byte) bool {
	return bytes.Equal(buf, []byte("\x1b[6~")) || bytes.Equal(buf, []byte("\x1b[6;2~"))
}

func isShiftPageDownInput(buf []byte) bool {
	return bytes.Equal(buf, []byte("\x1b[6;2~"))
}

// parseMouseWheelDirection returns -1 for wheel up, +1 for wheel down, or 0 if not a wheel event.
func parseMouseWheelDirection(buf []byte) int {
	dir, _, _ := parseMouseWheelEvents(buf)
	return dir
}

// parseMouseWheelEvents parses one or more coalesced mouse wheel sequences in buf.
// Returns (dir, shiftHeld, count) if buf consists solely of mouse wheel events.
func parseMouseWheelEvents(buf []byte) (dir int, shiftHeld bool, count int) {
	if len(buf) < 6 {
		return 0, false, 0
	}
	rem := buf
	for len(rem) > 0 {
		d, shift, consumed := parseOneMouseWheelEvent(rem)
		if d == 0 || consumed <= 0 {
			return 0, false, 0
		}
		if dir == 0 {
			dir = d
		} else if d != dir {
			dir = d
		}
		if shift {
			shiftHeld = true
		}
		count++
		rem = rem[consumed:]
	}
	return dir, shiftHeld, count
}

func parseOneMouseWheelEvent(buf []byte) (dir int, shiftHeld bool, consumed int) {
	if len(buf) < 6 || buf[0] != 0x1b || buf[1] != '[' {
		return 0, false, 0
	}
	// X10 mouse wheel: \x1b[M<cb><x><y> (6 bytes)
	if buf[2] == 'M' {
		if len(buf) < 6 {
			return 0, false, 0
		}
		cb := int(buf[3]) - 32
		if cb&64 == 0 {
			return 0, false, 0
		}
		d := 1
		if cb&1 == 0 {
			d = -1
		}
		return d, cb&4 != 0, 6
	}
	// SGR mouse wheel: \x1b[<%d;%d;%d[Mm]
	if buf[2] == '<' {
		end := 3
		for end < len(buf) && buf[end] != 'M' && buf[end] != 'm' {
			if (buf[end] < '0' || buf[end] > '9') && buf[end] != ';' {
				return 0, false, 0
			}
			end++
		}
		if end >= len(buf) {
			return 0, false, 0
		}
		var cb, x, y int
		var last byte
		if _, err := fmt.Sscanf(string(buf[3:end+1]), "%d;%d;%d%c", &cb, &x, &y, &last); err == nil && cb&64 != 0 {
			d := 1
			if cb&1 == 0 {
				d = -1
			}
			return d, cb&4 != 0, end + 1
		}
		return 0, false, 0
	}
	// URXVT mouse wheel: \x1b[%d;%d;%dM
	end := 2
	for end < len(buf) && buf[end] != 'M' {
		if (buf[end] < '0' || buf[end] > '9') && buf[end] != ';' {
			return 0, false, 0
		}
		end++
	}
	if end >= len(buf) {
		return 0, false, 0
	}
	var cb, x, y int
	if _, err := fmt.Sscanf(string(buf[2:end+1]), "%d;%d;%dM", &cb, &x, &y); err == nil && cb&64 != 0 {
		d := 1
		if cb&1 == 0 {
			d = -1
		}
		return d, cb&4 != 0, end + 1
	}
	return 0, false, 0
}
