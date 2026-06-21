package classroomcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadConfig(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		p := filepath.Join(dir, ".classroom50.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write temp config: %v", err)
		}
		return p
	}

	valid := `classroom: cs-principles
assignment: hello
source:
  owner: cs50
  repo: hello-template
  branch: main
`

	t.Run("valid config round-trips", func(t *testing.T) {
		cfg, err := ReadConfig(write(t, valid))
		if err != nil {
			t.Fatalf("ReadConfig: %v", err)
		}
		if cfg.Classroom != "cs-principles" || cfg.Assignment != "hello" {
			t.Errorf("got %+v, want classroom=cs-principles assignment=hello", cfg)
		}
		if cfg.Source == nil ||
			cfg.Source.Owner != "cs50" || cfg.Source.Repo != "hello-template" || cfg.Source.Branch != "main" {
			t.Errorf("source = %+v, want cs50/hello-template@main", cfg.Source)
		}
	})

	t.Run("template-less config (no source) round-trips", func(t *testing.T) {
		cfg, err := ReadConfig(write(t, "classroom: cs-principles\nassignment: solo\n"))
		if err != nil {
			t.Fatalf("ReadConfig(template-less): %v", err)
		}
		if cfg.Source != nil {
			t.Errorf("Source = %+v, want nil for a template-less config", cfg.Source)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		_, err := ReadConfig(filepath.Join(t.TempDir(), "nope.yaml"))
		if err == nil || !strings.Contains(err.Error(), "read") {
			t.Fatalf("err = %v, want a read error", err)
		}
	})

	t.Run("malformed YAML errors", func(t *testing.T) {
		_, err := ReadConfig(write(t, "classroom: [unterminated"))
		if err == nil || !strings.Contains(err.Error(), "parse") {
			t.Fatalf("err = %v, want a parse error", err)
		}
	})

	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing classroom", "assignment: hello\nsource:\n  owner: o\n  repo: r\n  branch: main\n", "missing classroom"},
		{"missing assignment", "classroom: c\nsource:\n  owner: o\n  repo: r\n  branch: main\n", "missing assignment"},
		{"incomplete source owner", "classroom: c\nassignment: a\nsource:\n  repo: r\n  branch: main\n", "incomplete source"},
		{"incomplete source branch", "classroom: c\nassignment: a\nsource:\n  owner: o\n  repo: r\n", "incomplete source"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadConfig(write(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
