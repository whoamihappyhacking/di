package main

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Snapshot returns the bytes that make a real terminal of the same size show
// exactly this terminal's state: scrollback, both screens, cursor, margins,
// character sets and every input-affecting mode. A client writes it verbatim
// and then switches to raw passthrough, so after attach the agent talks to
// the host terminal directly with no re-rendering in between.
func (vt *vterm) Snapshot() []byte {
	var b bytes.Buffer
	p := &vt.primary
	b.WriteString("\x1b[?2026h\x1b[?25l")
	if len(vt.scrollback) == 0 {
		// Nothing to restore above the screen. Line feeds from the cursor push
		// exactly the rows above it (the shell output before d ran) into the
		// terminal's own history instead of erasing them. This must come before
		// anything that homes the cursor.
		b.WriteString("\x1b]8;;\x1b\\\x1b[0m\r")
		b.WriteString(strings.Repeat("\n", p.rows-1))
	}
	b.WriteString("\x1b]8;;\x1b\\\x1b[0m\x1b[r\x1b[?6l\x1b[?7h\x1b[4l\x1b(B\x1b)B\x0f")
	if vt.title != "" {
		fmt.Fprintf(&b, "\x1b]2;%s\x07", vt.title)
	}
	if vt.cwd != "" {
		fmt.Fprintf(&b, "\x1b]7;%s\x07", vt.cwd)
	}
	b.WriteString("\x1b[H\x1b[2J")
	if len(vt.scrollback) > 0 {
		// Replace the terminal's history with the session's. Each logical line
		// is re-wrapped by the host itself; rows-1 extra line feeds push the
		// whole dump into native scrollback above a blank screen.
		b.WriteString("\x1b[3J")
		for _, line := range vt.scrollback {
			b.WriteString(line.ansi)
			b.WriteString("\r\n")
		}
		b.WriteString(strings.Repeat("\r\n", p.rows-1))
	}
	writeScreenContent(&b, p)
	writeKittyStack(&b, p)
	writeMargins(&b, p)
	writeCursorPosition(&b, p, p.savedCurR, p.savedCurC, p.savedOriginMode)
	writeVTermStyleTransition(&b, vtermStyle{}, p.savedStyle)
	b.WriteString("\x1b7")

	if vt.inAltScreen {
		a := &vt.alt
		b.WriteString("\x1b[?1049h\x1b]8;;\x1b\\\x1b[0m\x1b[r\x1b[?6l\x1b[H\x1b[2J")
		writeScreenContent(&b, a)
		writeKittyStack(&b, a)
		writeMargins(&b, a)
		writeCursorPosition(&b, a, a.savedCurR, a.savedCurC, a.savedOriginMode)
		writeVTermStyleTransition(&b, vtermStyle{}, a.savedStyle)
		b.WriteString("\x1b7")
	}

	scr := vt.activeScreen()
	b.WriteString("\x1b]8;;\x1b\\\x1b[0m")
	writeCursorPosition(&b, scr, scr.curR, scr.curC, scr.originMode)
	if scr.pendingWrap && scr.autoWrap && scr.curC == scr.cols-1 {
		// Re-print the last cell so the host is in the same pending-wrap state.
		cells := scr.grid[scr.curR].cells
		col := scr.cols - 1
		if cells[col].width == 0 && col > 0 {
			col--
			writeCursorPosition(&b, scr, scr.curR, col, scr.originMode)
		}
		b.WriteString(encodeCellsANSI(cells[col:]))
	}

	if vt.g0Charset == '0' {
		b.WriteString("\x1b(0")
	}
	if vt.g1Charset == '0' {
		b.WriteString("\x1b)0")
	}
	if vt.activeCharset == 1 {
		b.WriteByte(0x0e)
	}
	writeVTermStyleTransition(&b, vtermStyle{}, vt.style)

	if !scr.autoWrap {
		b.WriteString("\x1b[?7l")
	}
	if vt.insertMode {
		b.WriteString("\x1b[4h")
	}
	if vt.newlineMode {
		b.WriteString("\x1b[20h")
	}
	b.WriteString(boolMode(1, vt.appCursorKeys))
	if vt.appKeypad {
		b.WriteString("\x1b=")
	} else {
		b.WriteString("\x1b>")
	}
	b.WriteString(boolMode(2004, vt.bracketedPaste))
	b.WriteString(boolMode(1004, vt.focusEvents))
	b.WriteString("\x1b[?1000l\x1b[?1002l\x1b[?1003l")
	if vt.mouseMode > 0 {
		fmt.Fprintf(&b, "\x1b[?%dh", vt.mouseMode)
	}
	b.WriteString(boolMode(1006, vt.mouseSGR))
	if vt.modifyOtherKeys > 0 {
		fmt.Fprintf(&b, "\x1b[>4;%dm", vt.modifyOtherKeys)
	}
	fmt.Fprintf(&b, "\x1b[%d q", scr.cursorShape)
	if vt.progressSeq != "" {
		fmt.Fprintf(&b, "\x1b]9;%s\x07", vt.progressSeq)
	}
	if vt.agentState != "" {
		fmt.Fprintf(&b, "\x1b]7501;%s\x07", vt.agentState)
	}
	if scr.cursorVis {
		b.WriteString("\x1b[?25h")
	}
	b.WriteString("\x1b[?2026l")
	b.Write(vt.PendingSequence())
	return b.Bytes()
}

func boolMode(mode int, set bool) string {
	if set {
		return fmt.Sprintf("\x1b[?%dh", mode)
	}
	return fmt.Sprintf("\x1b[?%dl", mode)
}

func writeScreenContent(b *bytes.Buffer, scr *vtermScreen) {
	continues := false
	for r, row := range scr.grid {
		// A soft-wrapped row is written to the full width so the host wraps into
		// the next row itself and keeps the logical line joined for reflow.
		wraps := row.wrapped && r < scr.rows-1 && len(row.cells) == scr.cols
		cells := row.cells
		if !wraps {
			cells = trimBlankTail(cells, 0)
		}
		if !continues {
			if len(cells) == 0 {
				continue
			}
			fmt.Fprintf(b, "\x1b[%d;1H", r+1)
		}
		b.WriteString(encodeCellsANSI(cells))
		continues = wraps
	}
}

func writeKittyStack(b *bytes.Buffer, scr *vtermScreen) {
	for i := 0; i < scr.kittyLen; i++ {
		fmt.Fprintf(b, "\x1b[>%du", scr.kittyStack[i])
	}
}

func writeMargins(b *bytes.Buffer, scr *vtermScreen) {
	if scr.scrollTop != 0 || scr.scrollBottom != scr.rows-1 {
		fmt.Fprintf(b, "\x1b[%d;%dr", scr.scrollTop+1, scr.scrollBottom+1)
	} else {
		b.WriteString("\x1b[r")
	}
}

func writeCursorPosition(b *bytes.Buffer, scr *vtermScreen, r, c int, origin bool) {
	if origin {
		r = clampInt(r-scr.scrollTop, 0, scr.scrollBottom-scr.scrollTop)
		fmt.Fprintf(b, "\x1b[?6h\x1b[%d;%dH", r+1, c+1)
		return
	}
	fmt.Fprintf(b, "\x1b[?6l\x1b[%d;%dH", r+1, c+1)
}

// scrollIntoHistory moves the top n rows of a rows-high screen into the
// terminal's scrollback and leaves the cursor home on a blank screen.
func scrollIntoHistory(rows, n int) string {
	return fmt.Sprintf("\x1b[r\x1b[%d;1H%s\x1b[H\x1b[2J", rows, strings.Repeat("\n", max(n, 0)))
}

// usedRows is how many rows from the top of the primary screen hold content.
func (vt *vterm) usedRows() int {
	for r := vt.primary.rows - 1; r >= 0; r-- {
		if len(trimBlankTail(vt.primary.grid[r].cells, 0)) > 0 {
			return r + 1
		}
	}
	return 0
}

// hostRestoreSequence undoes everything a session may have changed in the host
// terminal, based on the modes the client saw go by. It runs on every detach,
// switch and exit so the shell never inherits kitty keyboard flags, mouse
// reporting, bracketed paste or an alternate screen.
func hostRestoreSequence(vt *vterm) string {
	var b strings.Builder
	// CAN aborts an escape sequence cut off by the detach.
	b.WriteString("\x18\x1b[?2026l\x1b]8;;\x1b\\\x1b[0m")
	if vt != nil && vt.inAltScreen {
		if n := vt.alt.kittyLen; n > 0 {
			fmt.Fprintf(&b, "\x1b[<%du", n)
		}
		b.WriteString("\x1b[?1049l")
	}
	if vt != nil && vt.primary.kittyLen > 0 {
		fmt.Fprintf(&b, "\x1b[<%du", vt.primary.kittyLen)
	}
	// Resetting scroll margins or origin mode homes the cursor, which would
	// put the shell prompt on top of the program's last screen. Only reset
	// them when the program changed them, and put the cursor back after.
	if vt != nil {
		p := &vt.primary
		curR, curC, origin := p.curR, p.curC, p.originMode
		if vt.inAltScreen {
			// Leaving the alternate screen restores the cursor saved on entry.
			curR, curC, origin = p.savedCurR, p.savedCurC, p.savedOriginMode
		}
		if origin || p.scrollTop != 0 || p.scrollBottom != p.rows-1 {
			fmt.Fprintf(&b, "\x1b[r\x1b[?6l\x1b[%d;%dH", curR+1, curC+1)
		}
	}
	b.WriteString("\x1b[?7h\x1b[4l\x1b[20l\x1b[?1l\x1b>\x1b[?2004l\x1b[?1004l")
	b.WriteString("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[>4m")
	b.WriteString("\x1b(B\x1b)B\x0f\x1b[0 q\x1b[?25h")
	b.WriteString("\x1b]9;4;0\x07\x1b]7501;state=clear\x07\x1b]21337;status=\x07")
	return b.String()
}

// Preview renders the bottom of the session as colored lines that fit in a
// cols x rows box (the fzf preview window).
func (vt *vterm) Preview(cols, rows int) string {
	cols = max(cols, 10)
	rows = max(rows, 1)
	scr := vt.activeScreen()
	last := -1
	for r := scr.rows - 1; r >= 0; r-- {
		if len(trimBlankTail(scr.grid[r].cells, 0)) > 0 {
			last = r
			break
		}
	}
	var lines [][]vtermCell
	for r := 0; r <= last; r++ {
		// Join soft-wrapped rows so text re-wraps at the preview width.
		logical := scr.grid[r].cells
		if scr.grid[r].wrapped && r < last {
			logical = append([]vtermCell(nil), logical...)
			for scr.grid[r].wrapped && r < last {
				r++
				logical = append(logical, scr.grid[r].cells...)
			}
		}
		lines = append(lines, fitPreviewLine(trimBlankTail(logical, 0), cols)...)
	}
	lines = collapseBlankLines(lines)
	if !vt.inAltScreen {
		var older [][]vtermCell
		for i := len(vt.scrollback) - 1; i >= 0 && len(older)+len(lines) < rows; i-- {
			line := vt.scrollback[i]
			older = append(fitPreviewLine(decodeANSICells(line.ansi, line.width), cols), older...)
		}
		lines = collapseBlankLines(append(older, lines...))
	}
	for len(lines) > 0 && len(lines[0]) == 0 {
		lines = lines[1:]
	}
	if len(lines) > rows {
		lines = lines[len(lines)-rows:]
	}
	out := make([]string, len(lines))
	for i, cells := range lines {
		out[i] = encodeCellsANSI(cells)
	}
	return strings.Join(out, "\n")
}

func collapseBlankLines(lines [][]vtermCell) [][]vtermCell {
	out := lines[:0:0]
	for i, line := range lines {
		if len(line) == 0 && i > 0 && len(lines[i-1]) == 0 {
			continue
		}
		out = append(out, line)
	}
	return out
}

// fitPreviewLine makes a line fit into cols: rules are truncated, padded UI
// lines lose interior spaces, everything else wraps.
func fitPreviewLine(cells []vtermCell, cols int) [][]vtermCell {
	if len(cells) <= cols {
		return [][]vtermCell{cells}
	}
	if isRuleLike(cells) {
		return [][]vtermCell{cutCells(cells, cols)}
	}
	if shrunk, ok := shrinkSpaceRuns(cells, cols); ok {
		return [][]vtermCell{shrunk}
	}
	var out [][]vtermCell
	for len(cells) > cols {
		cut := cutCells(cells, cols)
		out = append(out, cut)
		cells = cells[len(cut):]
	}
	return append(out, cells)
}

func cutCells(cells []vtermCell, cols int) []vtermCell {
	if len(cells) <= cols {
		return cells
	}
	cut := cols
	if cells[cut].width == 0 {
		cut-- // keep a wide character together
	}
	return cells[:cut]
}

func isRuleLike(cells []vtermCell) bool {
	rule, other := 0, 0
	for _, c := range cells {
		switch c.ch {
		case '─', '━', '═', '-', '╌', '┄':
			rule++
		case ' ', 0, '│', '╭', '╮', '╯', '╰', '┌', '┐', '└', '┘', '├', '┤', '┬', '┴', '┼':
		default:
			other++
		}
	}
	return rule >= 8 && rule >= other*3
}

func shrinkSpaceRuns(cells []vtermCell, cols int) ([]vtermCell, bool) {
	type run struct{ start, n int }
	var runs []run
	spare := 0
	for i := 0; i < len(cells); {
		if cells[i].ch != ' ' || cells[i].width != 1 || cells[i].combining != "" {
			i++
			continue
		}
		j := i
		for j < len(cells) && cells[j].ch == ' ' && cells[j].width == 1 && cells[j].combining == "" {
			j++
		}
		if j-i >= 3 {
			runs = append(runs, run{i, j - i})
			spare += j - i - 2
		}
		i = j
	}
	excess := len(cells) - cols
	if spare < excess {
		return nil, false
	}
	remove := make([]int, len(runs))
	for ; excess > 0; excess-- {
		best := -1
		for k, r := range runs {
			if r.n-remove[k] > 2 && (best < 0 || r.n-remove[k] > runs[best].n-remove[best]) {
				best = k
			}
		}
		remove[best]++
	}
	out := make([]vtermCell, 0, cols)
	prev := 0
	for k, r := range runs {
		keep := r.start + r.n - remove[k]
		out = append(out, cells[prev:keep]...)
		prev = r.start + r.n
	}
	return append(out, cells[prev:]...), true
}

// Summary returns the most recent meaningful text on screen, skipping agent
// UI chrome such as input boxes, hints and model/status footers. A visible
// spinner line is reported separately as the current activity.
func (vt *vterm) Summary() (activity, text string) {
	scr := vt.activeScreen()
	lines := make([]string, 0, scr.rows+16)
	if !vt.inAltScreen {
		for i := max(len(vt.scrollback)-16, 0); i < len(vt.scrollback); i++ {
			line := vt.scrollback[i]
			lines = append(lines, cellsPlainText(decodeANSICells(line.ansi, line.width)))
		}
	}
	for _, row := range scr.grid {
		lines = append(lines, row.plainText())
	}
	var parts []string
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || isPureHorizontalRule(line) {
			continue
		}
		if spin, ok := parseSpinnerLine(line); ok {
			if activity == "" && len(parts) == 0 {
				activity = cleanSummaryText(spin)
			}
			continue
		}
		if isUIChromeLine(line) {
			continue
		}
		cleaned := cleanSummaryText(line)
		if cleaned == "" {
			continue
		}
		parts = append([]string{cleaned}, parts...)
		if len(parts) >= 2 || runeDisplayWidth(strings.Join(parts, " ")) >= 56 {
			break
		}
	}
	return activity, strings.Join(parts, " ")
}

// CleanTitle strips spinner and status glyphs agents prefix to window titles.
func (vt *vterm) CleanTitle() string {
	title := strings.TrimLeftFunc(vt.title, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '~' && r != '/' && r != '[' && r != '(' && r != '.'
	})
	return strings.TrimSpace(title)
}

func parseSpinnerLine(line string) (string, bool) {
	r, size := utf8.DecodeRuneInString(line)
	if r >= 0x2800 && r <= 0x28ff {
		return strings.TrimSpace(line[size:]), true
	}
	return "", false
}

var chromePrefixes = []string{
	"? for shortcuts", "esc to cancel", "Argon ·", "8 Flash ·", "Flash ·", "Pro ·",
	"Gemini ", "GPT-", "Claude ", "└ Tip:", "⚡ ", "✻ ", "⏵⏵", "cache in:", "Reloaded keybindings",
}

var chromeContains = []string{
	"for shortcuts", "shift+tab to", "esc to interrupt", "Esc to cancel", "ctrl+o to expand",
	"ctrl+c to", "MCP:", "think:high", "think:medium", "context left", "Context left",
}

func isUIChromeLine(line string) bool {
	switch line {
	case ">", "❯", "$", "#", "↳", "›":
		return true
	}
	for _, p := range chromePrefixes {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	for _, s := range chromeContains {
		if strings.Contains(line, s) {
			return true
		}
	}
	// Box drawing borders and empty prompts without text content.
	return strings.TrimFunc(line, func(r rune) bool {
		return strings.ContainsRune("─━│╭╮╯╰ >❯›", r)
	}) == ""
}

func cleanSummaryText(line string) string {
	line = strings.TrimLeft(line, "•*-│╭╰>⏺● ")
	line = strings.TrimRight(line, "│╮╯` ")
	return strings.Join(strings.Fields(line), " ")
}

func isPureHorizontalRule(s string) bool {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) < 20 {
		return false
	}
	for _, r := range runes {
		if r != '─' && r != '━' && r != '═' && r != '-' {
			return false
		}
	}
	return true
}
