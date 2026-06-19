package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/foundation50/classroom50-cli-shared/ghauth"
)

// ui renders init's human-facing output (progress headers, status
// lines, warnings, and the end-of-run summary) to a single writer —
// always stderr, so stdout stays machine-stable for --json and ad-hoc
// greps. Styling (ANSI color + box-drawing) is gated on `color`, which
// degrades to plain ASCII on non-TTYs and when NO_COLOR /
// CLASSROOM50_NO_COLOR is set. The `Warning: ` prefix is preserved in
// both modes because existing tests and downstream readers key on it.
type ui struct {
	w     io.Writer
	color bool
	tty   bool
}

// Minimal SGR codes. Hand-rolled rather than pulling in a color library
// (muesli/termenv is only an indirect dep) — init needs three colors and
// bold, nothing more.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiDim    = "\x1b[2m"
)

// summaryWrapWidth is the soft wrap width for long warning/summary
// bodies so a 600-char read-back warning doesn't render as one
// unreadable line (the Team-plan run that motivated this).
const summaryWrapWidth = 76

// newUI builds a ui writing to w, auto-detecting color: w must be a TTY
// (we check stderr, the writer init uses) and neither NO_COLOR nor
// CLASSROOM50_NO_COLOR may be set. Callers that write somewhere other
// than os.Stderr (tests, captured buffers) get color=false, which is the
// safe default.
func newUI(w io.Writer) *ui {
	return &ui{w: w, color: detectColor(w), tty: detectTTY(w)}
}

// newUIForced builds a ui with an explicit color setting, for
// deterministic tests that must exercise the colored or plain renderer
// regardless of where the test's output happens to go. tty follows color
// (forced-color callers are exercising the interactive renderer).
func newUIForced(w io.Writer, color bool) *ui {
	return &ui{w: w, color: color, tty: color}
}

// detectTTY reports whether w is an interactive terminal (only os.Stderr
// is eligible — init's human channel). Independent of color so that
// NO_COLOR still allows in-place progress on a real terminal.
func detectTTY(w io.Writer) bool {
	if w != io.Writer(os.Stderr) {
		return false
	}
	return ghauth.IsCharDevice(os.Stderr)
}

// detectColor reports whether color should be emitted to w. Only os.Stderr
// is eligible (init's human channel); any other writer is treated as a
// capture/redirect and gets plain output. NO_COLOR (the de-facto standard)
// and CLASSROOM50_NO_COLOR both force plain.
func detectColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("CLASSROOM50_NO_COLOR") != "" {
		return false
	}
	if w != io.Writer(os.Stderr) {
		return false
	}
	return ghauth.IsCharDevice(os.Stderr)
}

// paint wraps s in an SGR code when color is on; otherwise returns s
// unchanged.
func (u *ui) paint(code, s string) string {
	if !u.color {
		return s
	}
	return code + s + ansiReset
}

// step prints a `[n/total] label` phase header. On a color TTY the
// counter is dimmed and the label bold; plain otherwise.
func (u *ui) step(n, total int, label string) {
	counter := fmt.Sprintf("[%d/%d]", n, total)
	_, _ = fmt.Fprintf(u.w, "%s %s\n", u.paint(ansiDim, counter), u.paint(ansiBold, label))
}

// section prints an un-numbered phase header (e.g. "Preflight checks"),
// bold on a color TTY. Used for phases that aren't part of the numbered
// step sequence.
func (u *ui) section(label string) {
	_, _ = fmt.Fprintf(u.w, "%s\n", u.paint(ansiBold, label))
}

// progress renders a single self-rewriting status line on a TTY. Call
// update for each step (it rewrites the same line via carriage return),
// then done to finalize. On a non-TTY it's a no-op (callers fall back to
// stable per-line output), so the machine-stable channel never sees
// cursor-control escapes.
type progress struct {
	u     *ui
	total int
	shown bool
}

func (u *ui) newProgress(total int) *progress {
	return &progress{u: u, total: total}
}

// active reports whether the progress line actually renders (TTY only).
// Callers use this to decide whether to suppress the verbose per-step
// output that would otherwise scroll the progress line away.
func (p *progress) active() bool { return p.u.tty }

// update rewrites the in-place line to "[n/total] label". \r returns to
// column 0 and \x1b[K clears to end of line so a shorter label can't
// leave trailing characters from a longer previous one.
func (p *progress) update(n int, label string) {
	if !p.u.tty {
		return
	}
	p.shown = true
	counter := fmt.Sprintf("[%d/%d]", n, p.total)
	_, _ = fmt.Fprintf(p.u.w, "\r\x1b[K%s %s", p.u.paint(ansiDim, counter), label)
}

// done finalizes the progress line as "[total/total] Done" and moves to
// the next line so subsequent output (the summary) starts cleanly.
func (p *progress) done() {
	if !p.u.tty || !p.shown {
		return
	}
	counter := fmt.Sprintf("[%d/%d]", p.total, p.total)
	_, _ = fmt.Fprintf(p.u.w, "\r\x1b[K%s %s\n", p.u.paint(ansiDim, counter), p.u.paint(ansiGreen, "Done"))
}

// abort clears the in-place line without the "Done" marker, for the
// error path so a failure message isn't appended to a half-written
// progress line.
func (p *progress) abort() {
	if !p.u.tty || !p.shown {
		return
	}
	_, _ = fmt.Fprint(p.u.w, "\r\x1b[K")
}

// ok prints a success line: "✓ <msg>" on a color TTY, "[ok] <msg>"
// plain. Used for per-step confirmations on the human channel (the
// machine-stable stdout lines are emitted separately).
func (u *ui) ok(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if u.color {
		_, _ = fmt.Fprintf(u.w, "%s %s\n", u.paint(ansiGreen, "\u2713"), msg)
		return
	}
	_, _ = fmt.Fprintf(u.w, "[ok] %s\n", msg)
}

// warn prints a warning. It ALWAYS contains the literal "Warning: " so
// existing substring assertions and downstream log scrapers keep
// working; on a color TTY it's additionally prefixed with a yellow ⚠
// and the leading "Warning:" is yellowed. Long bodies are soft-wrapped
// (continuation lines indented) so a single huge warning stays readable.
func (u *ui) warn(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	body := "Warning: " + msg
	wrapped := softWrap(body, summaryWrapWidth, "  ")
	if u.color {
		// Color only the first line's "Warning:" token; the wrap keeps
		// the rest plain so multi-line bodies don't smear escape codes.
		wrapped = strings.Replace(wrapped, "Warning:", u.paint(ansiYellow, "Warning:"), 1)
		_, _ = fmt.Fprintf(u.w, "%s %s\n", u.paint(ansiYellow, "\u26a0"), wrapped)
		return
	}
	_, _ = fmt.Fprintf(u.w, "%s\n", wrapped)
}

// heading prints a section heading on the human channel — bold on a
// color TTY, an ASCII-underlined label plain. A blank line precedes it
// so sections are visually separated.
func (u *ui) heading(label string) {
	if u.color {
		_, _ = fmt.Fprintf(u.w, "\n%s\n", u.paint(ansiBold, label))
		return
	}
	_, _ = fmt.Fprintf(u.w, "\n%s\n", label)
}

// result prints the one-line outcome banner that opens the final report:
// a ✓ / ⚠ / ✗ glyph (green/yellow/red on a color TTY, ASCII tag plain)
// plus the message.
func (u *ui) result(status preflightStatus, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if !u.color {
		tag := map[preflightStatus]string{preflightOK: "[ok]", preflightWarn: "[!]", preflightFail: "[x]"}[status]
		_, _ = fmt.Fprintf(u.w, "%s %s\n", tag, msg)
		return
	}
	glyph := map[preflightStatus]string{
		preflightOK:   u.paint(ansiGreen, "\u2713"),
		preflightWarn: u.paint(ansiYellow, "\u26a0"),
		preflightFail: u.paint(ansiRed, "\u2717"),
	}[status]
	_, _ = fmt.Fprintf(u.w, "%s %s\n", glyph, u.paint(ansiBold, msg))
}

// numbered prints an ordinal list item ("  1. <msg>") under a report
// heading, with a hanging wrap aligned under the text. Used for a list
// of manual steps that are NOT checkboxes — e.g. settings GitHub exposes
// no API to read, so they're an instruction list to eyeball, not state
// the CLI tracks.
func (u *ui) numbered(n int, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	prefix := fmt.Sprintf("%d. ", n)
	// Indent continuation lines past the "  N. " prefix so wrapped text
	// lines up under the item body, not the number.
	hang := strings.Repeat(" ", 2+len(prefix))
	_, _ = fmt.Fprintf(u.w, "  %s%s\n", prefix, softWrap(msg, summaryWrapWidth, hang))
}

// okItem prints a checked-off list item under a report heading: "  ✓
// <msg>" on a color TTY, "  [x] <msg>" plain. The two-space indent and
// hanging wrap match checkbox() so a mixed list of done (✓) and to-do
// ([ ]) items lines up under the same heading.
func (u *ui) okItem(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if u.color {
		_, _ = fmt.Fprintf(u.w, "  %s %s\n", u.paint(ansiGreen, "\u2713"), softWrap(msg, summaryWrapWidth, "      "))
		return
	}
	_, _ = fmt.Fprintf(u.w, "  [x] %s\n", softWrap(msg, summaryWrapWidth, "      "))
}

// checkbox prints an actionable to-do item as an unchecked box. The body
// is soft-wrapped with a hanging indent so a long instruction stays
// readable and lined up under the text (not the box).
func (u *ui) checkbox(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	_, _ = fmt.Fprintf(u.w, "  [ ] %s\n", softWrap(msg, summaryWrapWidth, "      "))
}

// detail prints an indented, dimmed continuation line (e.g. a URL under
// a checklist). Dimmed on a color TTY, plain otherwise.
func (u *ui) detail(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	_, _ = fmt.Fprintf(u.w, "  %s\n", u.paint(ansiDim, msg))
}

// item prints a plain status line in the report's summary section,
// two-space indented.
func (u *ui) item(format string, a ...any) {
	_, _ = fmt.Fprintf(u.w, "  %s\n", fmt.Sprintf(format, a...))
}

// next prints the prominent next-command call to action.
func (u *ui) next(command string) {
	if u.color {
		_, _ = fmt.Fprintf(u.w, "\n%s %s\n", u.paint(ansiBold, "Next:"), u.paint(ansiGreen, command))
		return
	}
	_, _ = fmt.Fprintf(u.w, "\nNext: %s\n", command)
}

// blank prints a single empty line for visual separation.
func (u *ui) blank() {
	_, _ = fmt.Fprintln(u.w)
}

// softWrap wraps s to width columns on spaces, indenting continuation
// lines with indent. Words longer than width are left intact (URLs).
func softWrap(s string, width int, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return s
	}
	var b strings.Builder
	lineLen := 0
	for i, word := range words {
		switch {
		case i == 0:
			b.WriteString(word)
			lineLen = displayLen(word)
		case lineLen+1+displayLen(word) > width:
			b.WriteString("\n")
			b.WriteString(indent)
			b.WriteString(word)
			lineLen = displayLen(indent) + displayLen(word)
		default:
			b.WriteString(" ")
			b.WriteString(word)
			lineLen += 1 + displayLen(word)
		}
	}
	return b.String()
}

// displayLen approximates the printed width of s as its rune count,
// ignoring ANSI escape sequences. Good enough for layout of the ASCII +
// occasional emoji content init emits.
func displayLen(s string) int {
	n := 0
	inEscape := false
	for _, r := range s {
		switch {
		case inEscape:
			if r == 'm' {
				inEscape = false
			}
		case r == '\x1b':
			inEscape = true
		default:
			n++
		}
	}
	return n
}
