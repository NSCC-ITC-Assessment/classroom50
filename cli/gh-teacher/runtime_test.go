package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRuntime_HostPaths(t *testing.T) {
	cases := []struct {
		name    string
		runtime runtimeRef
		wantErr string
	}{
		{
			name:    "empty is valid",
			runtime: runtimeRef{},
		},
		{
			name:    "ubuntu-latest with python",
			runtime: runtimeRef{RunsOn: "ubuntu-latest", Python: "3.12"},
		},
		{
			name:    "all language fields",
			runtime: runtimeRef{Python: "3.12", Node: "20", Java: "21", Go: "1.23"},
		},
		{
			name:    "apt packages",
			runtime: runtimeRef{Apt: []string{"build-essential", "valgrind", "lib-fake.dev"}},
		},
		{
			name:    "self-hosted runner rejected",
			runtime: runtimeRef{RunsOn: "self-hosted-grading"},
			wantErr: "allow-list",
		},
		{
			name:    "unknown github label rejected",
			runtime: runtimeRef{RunsOn: "ubuntu-30.04"},
			wantErr: "allow-list",
		},
		{
			name:    "python version with semicolon rejected",
			runtime: runtimeRef{Python: "3.12; rm -rf /"},
			wantErr: "runtime.python",
		},
		{
			name:    "apt with shell metacharacters rejected",
			runtime: runtimeRef{Apt: []string{"build-essential;rm"}},
			wantErr: "runtime.apt",
		},
		{
			name:    "apt with uppercase rejected",
			runtime: runtimeRef{Apt: []string{"Foo"}},
			wantErr: "runtime.apt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRuntime(tc.runtime)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q missing substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateRuntime_ContainerPaths(t *testing.T) {
	cases := []struct {
		name    string
		runtime runtimeRef
		wantErr string
	}{
		{
			name: "image only",
			runtime: runtimeRef{
				Container: &containerSpec{Image: "ghcr.io/cs50/grading-env:1.2"},
			},
		},
		{
			name: "image with credentials",
			runtime: runtimeRef{
				Container: &containerSpec{
					Image: "ghcr.io/private/grader:latest",
					Credentials: &containerCreds{
						Username: "cs50-bot",
						Password: "${{ secrets.GHCR_TOKEN }}",
					},
				},
			},
		},
		{
			name: "image with apt rejected",
			runtime: runtimeRef{
				Container: &containerSpec{Image: "ubuntu:24.04"},
				Apt:       []string{"build-essential"},
			},
			wantErr: "runtime.apt is not allowed when runtime.container",
		},
		{
			name: "macos runs-on with container rejected",
			runtime: runtimeRef{
				RunsOn:    "macos-latest",
				Container: &containerSpec{Image: "ubuntu:24.04"},
			},
			wantErr: "Ubuntu hosts only",
		},
		{
			name: "empty image rejected",
			runtime: runtimeRef{
				Container: &containerSpec{Image: ""},
			},
			wantErr: "runtime.container.image must not be empty",
		},
		{
			name: "image with shell metacharacters rejected",
			runtime: runtimeRef{
				Container: &containerSpec{Image: "ubuntu:24.04;rm"},
			},
			wantErr: "characters other than",
		},
		{
			name: "raw password rejected",
			runtime: runtimeRef{
				Container: &containerSpec{
					Image: "ghcr.io/cs50/grader:1",
					Credentials: &containerCreds{
						Username: "cs50-bot",
						Password: "ghp_actualtokenvaluedonotuse",
					},
				},
			},
			wantErr: "${{ secrets.NAME }}",
		},
		{
			name: "credentials missing username rejected",
			runtime: runtimeRef{
				Container: &containerSpec{
					Image: "ghcr.io/cs50/grader:1",
					Credentials: &containerCreds{
						Password: "${{ secrets.X }}",
					},
				},
			},
			wantErr: "username and password",
		},
		{
			name: "credentials missing password rejected",
			runtime: runtimeRef{
				Container: &containerSpec{
					Image: "ghcr.io/cs50/grader:1",
					Credentials: &containerCreds{
						Username: "cs50-bot",
					},
				},
			},
			wantErr: "username and password",
		},
		{
			name: "user 32-char accepted (boundary)",
			runtime: runtimeRef{
				Container: &containerSpec{
					Image: "cs50/cli:latest",
					User:  "a" + strings.Repeat("b", 31), // first char + 31 = 32
				},
			},
		},
		{
			name: "user 33-char rejected (over boundary)",
			runtime: runtimeRef{
				Container: &containerSpec{
					Image: "cs50/cli:latest",
					User:  "a" + strings.Repeat("b", 32), // first char + 32 = 33
				},
			},
			wantErr: "runtime.container.user",
		},
		{
			name: "user trailing colon rejected",
			runtime: runtimeRef{
				Container: &containerSpec{Image: "cs50/cli:latest", User: "1000:"},
			},
			wantErr: "runtime.container.user",
		},
		{
			name: "user multi-colon rejected",
			runtime: runtimeRef{
				Container: &containerSpec{Image: "cs50/cli:latest", User: "1000:1000:1000"},
			},
			wantErr: "runtime.container.user",
		},
		{
			name: "user leading underscore accepted",
			runtime: runtimeRef{
				Container: &containerSpec{Image: "cs50/cli:latest", User: "_appuser"},
			},
		},
		{
			name: "user root accepted",
			runtime: runtimeRef{
				Container: &containerSpec{Image: "cs50/cli:latest", User: "root"},
			},
		},
		{
			name: "user numeric uid accepted",
			runtime: runtimeRef{
				Container: &containerSpec{Image: "cs50/cli:latest", User: "0"},
			},
		},
		{
			name: "user uid:gid accepted",
			runtime: runtimeRef{
				Container: &containerSpec{Image: "cs50/cli:latest", User: "1000:1000"},
			},
		},
		{
			name: "user with shell metacharacters rejected",
			runtime: runtimeRef{
				Container: &containerSpec{Image: "cs50/cli:latest", User: "root; rm -rf /"},
			},
			wantErr: "runtime.container.user",
		},
		{
			name: "user with leading hyphen rejected",
			runtime: runtimeRef{
				Container: &containerSpec{Image: "cs50/cli:latest", User: "-rm"},
			},
			wantErr: "runtime.container.user",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRuntime(tc.runtime)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q missing substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseRuntimeFile_Empty(t *testing.T) {
	got, err := parseRuntimeFile("")
	if err != nil {
		t.Fatalf("empty path should be no-op, got error: %v", err)
	}
	if got != nil {
		t.Errorf("empty path should yield nil runtimeRef, got %#v", got)
	}
}

func TestParseRuntimeFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	body := `{
  "runs-on": "ubuntu-latest",
  "python": "3.12",
  "apt": ["build-essential"]
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := parseRuntimeFile(path)
	if err != nil {
		t.Fatalf("parseRuntimeFile: %v", err)
	}
	if got == nil {
		t.Fatal("got nil runtimeRef on happy path")
	}
	if got.RunsOn != "ubuntu-latest" || got.Python != "3.12" {
		t.Errorf("fields not parsed: %#v", got)
	}
	if len(got.Apt) != 1 || got.Apt[0] != "build-essential" {
		t.Errorf("apt not parsed: %#v", got.Apt)
	}
}

func TestParseRuntimeFile_UnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	// Typo: `run-on` instead of `runs-on`. DisallowUnknownFields
	// must surface this as a decode error rather than silently
	// falling through to defaults.
	body := `{"run-on": "ubuntu-latest"}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := parseRuntimeFile(path)
	if err == nil {
		t.Fatal("expected decode error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "run-on") {
		t.Errorf("error should name the offending field, got %q", err)
	}
}

func TestParseRuntimeFile_TrailingContentRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	body := `{"runs-on": "ubuntu-latest"} {"extra": "object"}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := parseRuntimeFile(path)
	if err == nil {
		t.Fatal("expected error for trailing content, got nil")
	}
}

func TestParseRuntimeFile_ValidationFailureWrapsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	body := `{"runs-on": "self-hosted"}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := parseRuntimeFile(path)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should reference the offending file path, got %q", err)
	}
}

func TestValidateAssignmentEntry_RuntimePropagates(t *testing.T) {
	// Bad runtime block must fail validateAssignmentEntry — the
	// write path uses the same validator under the hood.
	entry := assignmentEntry{
		Slug:       "hello",
		Name:       "Hello",
		Template:   templateRef{Owner: "cs50", Repo: "hello-template", Branch: "main"},
		Mode:       "individual",
		Autograder: "default",
		Runtime:    &runtimeRef{Apt: []string{"BAD;PKG"}},
	}
	err := validateAssignmentEntry(entry)
	if err == nil {
		t.Fatal("expected runtime validation to bubble up, got nil")
	}
	if !strings.Contains(err.Error(), "runtime.apt") {
		t.Errorf("err should mention runtime.apt, got %q", err)
	}
}

func TestParseAssignments_RuntimeRoundTrips(t *testing.T) {
	in := []byte(`{
  "schema": "classroom50/assignments/v1",
  "assignments": [
    {
      "slug": "hello",
      "name": "Hello",
      "template": { "owner": "cs50", "repo": "hello-template", "branch": "main" },
      "mode": "individual",
      "autograder": "default",
      "runtime": {
        "runs-on": "ubuntu-latest",
        "python": "3.12",
        "apt": ["build-essential"]
      }
    }
  ]
}`)
	file, err := parseAssignments(in)
	if err != nil {
		t.Fatalf("parseAssignments: %v", err)
	}
	got := file.Assignments[0].Runtime
	if got == nil {
		t.Fatal("runtime block dropped on parse")
	}
	if got.RunsOn != "ubuntu-latest" || got.Python != "3.12" || len(got.Apt) != 1 {
		t.Errorf("runtime fields not parsed: %#v", got)
	}

	// Re-encode and re-parse to confirm round-trip stability.
	encoded, err := encodeAssignments(file)
	if err != nil {
		t.Fatalf("encodeAssignments: %v", err)
	}
	again, err := parseAssignments(encoded)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if again.Assignments[0].Runtime == nil {
		t.Fatal("runtime block dropped on re-encode")
	}
}

func TestParseAssignments_RejectsInvalidRuntime(t *testing.T) {
	in := []byte(`{
  "schema": "classroom50/assignments/v1",
  "assignments": [
    {
      "slug": "hello",
      "name": "Hello",
      "template": { "owner": "cs50", "repo": "hello-template", "branch": "main" },
      "mode": "individual",
      "autograder": "default",
      "runtime": {
        "runs-on": "self-hosted-grading"
      }
    }
  ]
}`)
	_, err := parseAssignments(in)
	if err == nil {
		t.Fatal("expected parse to reject self-hosted runs-on, got nil")
	}
	if !strings.Contains(err.Error(), "allow-list") {
		t.Errorf("err should reference allow-list, got %q", err)
	}
}
