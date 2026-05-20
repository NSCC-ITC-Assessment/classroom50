package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
	"gopkg.in/yaml.v3"
)

func TestRenderClassroomMetadata_FullShape(t *testing.T) {
	// `.classroom50.yml` carries four blocks: identity (classroom +
	// assignment), source (template), config (config repo), and
	// autograde (version sentinel). The round-trip pins the on-disk
	// shape so a reader branching on schema knows the current keys.
	cfg := ClassroomConfig{
		Classroom:  "cs-principles",
		Assignment: "hello",
		Source: ClassroomSource{
			Owner:  "cs50",
			Repo:   "hello-template",
			Branch: "main",
		},
		Config: ClassroomConfigRef{
			Owner:  "cs50-fall-2026",
			Repo:   "classroom50",
			Branch: "main",
			Path:   "cs-principles",
		},
		Autograde: AutogradeMetadata{
			Version: "0.2.0",
		},
	}
	out, err := renderClassroomMetadata(cfg)
	if err != nil {
		t.Fatalf("renderClassroomMetadata: %v", err)
	}

	// String scalars must be double-quoted so YAML doesn't
	// auto-type slugs like "yes" or "2026".
	wantSubs := []string{
		`classroom: "cs-principles"`,
		`assignment: "hello"`,
		`owner: "cs50"`,
		`repo: "hello-template"`,
		`branch: "main"`,
		`owner: "cs50-fall-2026"`,
		`repo: "classroom50"`,
		`path: "cs-principles"`,
		`version: "0.2.0"`,
	}
	for _, s := range wantSubs {
		if !strings.Contains(string(out), s) {
			t.Errorf("expected %q in rendered metadata, got:\n%s", s, out)
		}
	}

	// Block structure: each top-level key appears once at column 0.
	for _, key := range []string{"classroom:", "assignment:", "source:", "config:", "autograde:"} {
		if !strings.Contains(string(out), "\n"+key) && !strings.HasPrefix(string(out), key) {
			t.Errorf("missing top-level key %q in:\n%s", key, out)
		}
	}

	// Round-trip: yaml.Unmarshal must recover the original config.
	var round ClassroomConfig
	if err := yaml.Unmarshal(out, &round); err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if round != cfg {
		t.Errorf("round-trip mismatch:\n got: %#v\nwant: %#v", round, cfg)
	}
}

func TestRenderClassroomMetadata_OmitsEmptyOptionalBlocks(t *testing.T) {
	// Metadata without Config/Autograde must round-trip cleanly:
	// zero-valued optional blocks must omit so the file doesn't
	// sprout empty `config: {}` keys.
	cfg := ClassroomConfig{
		Classroom:  "cs-principles",
		Assignment: "hello",
		Source: ClassroomSource{
			Owner:  "cs50",
			Repo:   "hello-template",
			Branch: "main",
		},
	}
	out, err := renderClassroomMetadata(cfg)
	if err != nil {
		t.Fatalf("renderClassroomMetadata: %v", err)
	}

	for _, key := range []string{"config:", "autograde:"} {
		if strings.Contains(string(out), key) {
			t.Errorf("zero-value %s should be omitted, got:\n%s", key, out)
		}
	}
}

func TestRenderClassroomMetadata_PreservesNumericLookingSlugs(t *testing.T) {
	// Pins double-quoting: a numeric-looking classroom slug must
	// not encode as a YAML integer — downstream string compares
	// against args would break.
	cfg := ClassroomConfig{
		Classroom:  "2026",
		Assignment: "hello",
		Source:     ClassroomSource{Owner: "cs50", Repo: "hello-template", Branch: "main"},
	}
	out, err := renderClassroomMetadata(cfg)
	if err != nil {
		t.Fatalf("renderClassroomMetadata: %v", err)
	}
	if !strings.Contains(string(out), `classroom: "2026"`) {
		t.Errorf("classroom should be double-quoted to preserve string type, got:\n%s", out)
	}
}

func TestIsHTTPNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"direct 404 HTTPError", &api.HTTPError{StatusCode: http.StatusNotFound}, true},
		{"direct 409 HTTPError", &api.HTTPError{StatusCode: http.StatusConflict}, false},
		{
			name: "wrapped 404 HTTPError still resolves",
			err:  fmt.Errorf("GET something: %w", &api.HTTPError{StatusCode: http.StatusNotFound}),
			want: true,
		},
		{"plain error", errors.New("network unreachable"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHTTPNotFound(tc.err); got != tc.want {
				t.Fatalf("isHTTPNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
