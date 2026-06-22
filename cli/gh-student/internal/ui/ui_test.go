package ui

import (
	"bytes"
	"strings"
	"testing"
)

func TestUI_PlainMode_ASCIIFallbacks(t *testing.T) {
	var buf bytes.Buffer
	u := NewForced(&buf, false)
	u.Warn("you are inside a git repository")
	u.Detail("preparing snapshot")

	got := buf.String()
	for _, want := range []string{
		"Warning: you are inside a git repository",
		"preparing snapshot",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plain output missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("plain output must not contain ANSI escapes:\n%q", got)
	}
	if strings.Contains(got, "\u26a0") {
		t.Errorf("plain output must not contain the warning symbol:\n%q", got)
	}
}

func TestUI_ColorMode_WrapsWithANSIAndSymbols(t *testing.T) {
	var buf bytes.Buffer
	u := NewForced(&buf, true)
	u.Warn("heads up")

	got := buf.String()
	if !strings.Contains(got, "\x1b[") {
		t.Errorf("color output should contain ANSI escapes:\n%q", got)
	}
	if !strings.Contains(got, "\u26a0") {
		t.Errorf("color warn() should use the warning symbol:\n%q", got)
	}
	// The literal "Warning:" must survive in color mode too — downstream
	// scrapers and tests key on it.
	if !strings.Contains(got, "Warning:") {
		t.Errorf("color warn() must still contain the literal \"Warning:\":\n%q", got)
	}
}

func TestUI_Warn_AlwaysContainsWarningPrefix(t *testing.T) {
	for _, color := range []bool{false, true} {
		var buf bytes.Buffer
		NewForced(&buf, color).Warn("something is off")
		if !strings.Contains(buf.String(), "Warning:") {
			t.Errorf("warn(color=%v) must contain \"Warning:\": %q", color, buf.String())
		}
	}
}

// A buffer-backed UI yields a non-active spinner (non-TTY), so its
// fallback prints stable plain lines with no cursor escapes.
func TestUI_Spinner_NonTTYPlain(t *testing.T) {
	var buf bytes.Buffer
	u := NewForced(&buf, false)
	sp := u.Spinner("Working")
	if sp.Active() {
		t.Fatal("a buffer-backed spinner must not be active")
	}
	sp.Start()
	sp.Stop("Working")
	got := buf.String()
	if strings.Contains(got, "\x1b[") || strings.Contains(got, "\r") {
		t.Errorf("non-TTY spinner must not emit escapes or carriage returns:\n%q", got)
	}
	if !strings.Contains(got, "Working...") || !strings.Contains(got, "Working done") {
		t.Errorf("non-TTY spinner should print plain start/done lines:\n%q", got)
	}
}
