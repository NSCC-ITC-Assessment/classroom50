package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestUI_Progress_TTYRewritesInPlace(t *testing.T) {
	var buf bytes.Buffer
	u := newUIForced(&buf, true) // forced color => tty true
	p := u.newProgress(3)
	if !p.active() {
		t.Fatal("forced-color ui should have an active progress line")
	}
	p.update(1, "First")
	p.update(2, "Second")
	p.done()

	out := buf.String()
	// In-place updates use carriage return + clear-to-EOL.
	if !strings.Contains(out, "\r") {
		t.Errorf("progress should rewrite in place via carriage return:\n%q", out)
	}
	if !strings.Contains(out, "[1/3]") || !strings.Contains(out, "[2/3]") {
		t.Errorf("progress should render each step counter:\n%q", out)
	}
	if !strings.Contains(out, "[3/3]") || !strings.Contains(out, "Done") {
		t.Errorf("progress done() should render [3/3] Done:\n%q", out)
	}
}

func TestUI_Progress_NonTTYIsSilent(t *testing.T) {
	var buf bytes.Buffer
	u := newUIForced(&buf, false) // plain => tty false
	p := u.newProgress(3)
	if p.active() {
		t.Error("a non-TTY ui must not have an active progress line")
	}
	p.update(1, "First")
	p.done()
	if buf.Len() != 0 {
		t.Errorf("progress must emit nothing on a non-TTY:\n%q", buf.String())
	}
}

func TestUI_PlainMode_ASCIIFallbacks(t *testing.T) {
	var buf bytes.Buffer
	u := newUIForced(&buf, false)
	u.ok("config repo created")
	u.step(3, 7, "Config repo")
	u.warn("something is off")

	got := buf.String()
	for _, want := range []string{
		"[ok] config repo created",
		"[3/7] Config repo",
		"Warning: something is off",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plain output missing %q\n%s", want, got)
		}
	}
	// No ANSI escapes and no Unicode symbols in plain mode.
	if strings.Contains(got, "\x1b[") {
		t.Errorf("plain output must not contain ANSI escapes:\n%q", got)
	}
	for _, sym := range []string{"\u2713", "\u2022", "\u26a0"} {
		if strings.Contains(got, sym) {
			t.Errorf("plain output must not contain symbol %q:\n%q", sym, got)
		}
	}
}

func TestUI_ColorMode_WrapsWithANSIAndSymbols(t *testing.T) {
	var buf bytes.Buffer
	u := newUIForced(&buf, true)
	u.ok("done")
	u.warn("heads up")

	got := buf.String()
	if !strings.Contains(got, "\x1b[") {
		t.Errorf("color output should contain ANSI escapes:\n%q", got)
	}
	if !strings.Contains(got, "\u2713") {
		t.Errorf("color ok() should use the check symbol:\n%q", got)
	}
	if !strings.Contains(got, "\u26a0") {
		t.Errorf("color warn() should use the warning symbol:\n%q", got)
	}
	// The literal "Warning:" substring must survive even in color mode
	// (existing assertions and log scrapers depend on it).
	if !strings.Contains(got, "Warning:") {
		t.Errorf("color warn() must still contain the literal \"Warning:\":\n%q", got)
	}
}

func TestUI_Warn_AlwaysContainsWarningPrefix(t *testing.T) {
	for _, color := range []bool{false, true} {
		var buf bytes.Buffer
		newUIForced(&buf, color).warn("the org is enterprise-pinned")
		if !strings.Contains(buf.String(), "Warning:") {
			t.Errorf("warn(color=%v) must contain \"Warning:\": %q", color, buf.String())
		}
	}
}

func TestUI_ReportHelpers_PlainAndColor(t *testing.T) {
	// Plain mode: ASCII tags, no box-drawing, no ANSI.
	var plain bytes.Buffer
	up := newUIForced(&plain, false)
	up.result(preflightFail, "cs50: init INCOMPLETE")
	up.heading("Action required")
	up.checkbox(`uncheck "Allow members to delete or transfer repositories"`)
	up.detail("at https://github.com/organizations/cs50/settings/member_privileges")
	up.next("gh teacher classroom add cs50 <short-name>")
	ps := plain.String()
	for _, want := range []string{
		"[x] cs50: init INCOMPLETE",
		"Action required",
		`[ ] uncheck "Allow members to delete or transfer repositories"`,
		"at https://github.com/organizations/cs50/settings/member_privileges",
		"Next: gh teacher classroom add cs50 <short-name>",
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("plain report missing %q:\n%s", want, ps)
		}
	}
	if strings.Contains(ps, "\x1b[") {
		t.Errorf("plain report must not contain ANSI escapes:\n%q", ps)
	}
	if strings.Contains(ps, "\u250c") || strings.Contains(ps, "\u2502") {
		t.Errorf("report must not use box-drawing chars:\n%q", ps)
	}

	// Color mode: checkbox + Next still carry the literal content.
	var color bytes.Buffer
	uc := newUIForced(&color, true)
	uc.result(preflightOK, "cs50: init complete")
	uc.checkbox("do a thing")
	uc.next("gh teacher classroom add cs50 <short-name>")
	cs := color.String()
	if !strings.Contains(cs, "\x1b[") {
		t.Errorf("color report should contain ANSI escapes:\n%q", cs)
	}
	if !strings.Contains(cs, "[ ] do a thing") {
		t.Errorf("color checkbox should still render the unchecked box + text:\n%q", cs)
	}
	if !strings.Contains(cs, "Next:") || !strings.Contains(cs, "gh teacher classroom add cs50 <short-name>") {
		t.Errorf("color report should render the Next command:\n%q", cs)
	}
}

func TestSoftWrap(t *testing.T) {
	// A long body wraps onto multiple lines with the continuation indent.
	body := "Warning: this is a fairly long warning message that should wrap across multiple lines so it stays readable in a narrow terminal window"
	got := softWrap(body, 40, "  ")
	if !strings.Contains(got, "\n") {
		t.Errorf("expected wrapping for a long body, got single line:\n%s", got)
	}
	for _, ln := range strings.Split(got, "\n")[1:] {
		if !strings.HasPrefix(ln, "  ") {
			t.Errorf("continuation lines should be indented, got %q", ln)
		}
	}

	// A long unbreakable token (URL) is left intact even past the width.
	url := "https://github.com/organizations/some-really-long-org-name/settings/member_privileges"
	wrapped := softWrap("see "+url, 40, "  ")
	if !strings.Contains(wrapped, url) {
		t.Errorf("a long URL token must not be broken:\n%s", wrapped)
	}
}

func TestDisplayLen_IgnoresANSI(t *testing.T) {
	plain := "hello"
	colored := ansiGreen + "hello" + ansiReset
	if displayLen(plain) != displayLen(colored) {
		t.Errorf("displayLen should ignore ANSI: plain=%d colored=%d", displayLen(plain), displayLen(colored))
	}
	if displayLen(plain) != 5 {
		t.Errorf("displayLen(%q) = %d, want 5", plain, displayLen(plain))
	}
}
