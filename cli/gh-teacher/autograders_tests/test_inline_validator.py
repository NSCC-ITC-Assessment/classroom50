"""Behavioral tests for the inline Python validator embedded in
autograde-runner.yaml's setup job.

The validator runs in production on every student submission against
a teacher-controlled assignments.json + a student-controlled
.classroom50.yaml, and it's the last gate before runtime values flow
into the grade job's `runs-on:`/`container:` mapping. Drift between
this validator and `runtime.go` (CLI write-time) would let injection
past one or the other without any test catching it.

These tests extract the YAML-embedded `shell: python3 {0}` block,
write it to a tempfile, and exec it as a subprocess with hand-crafted
.classroom50.yaml + assignments.json fixtures so the validator's
allow-lists, regexes, and emit shape can be exercised independently
of the rest of the workflow.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import textwrap
from pathlib import Path

import pytest
import yaml


_REPO_ROOT = Path(__file__).resolve().parents[3]
_RUNNER_YAML = (
    _REPO_ROOT / "cli" / "gh-teacher" / "skeleton"
    / "dotgithub" / "workflows" / "autograde-runner.yaml"
)


def _extract_inline_python() -> str:
    """Pull the inline Python validator body out of the setup-job's
    `read` step. Identifies the step by its `id: read` + `shell:
    python3 {0}` pair and returns the text of its `run:` block."""
    doc = yaml.safe_load(_RUNNER_YAML.read_text())
    setup_steps = doc["jobs"]["setup"]["steps"]
    for step in setup_steps:
        if step.get("id") == "read" and step.get("shell") == "python3 {0}":
            return step["run"]
    raise RuntimeError("setup.read step with shell: python3 {0} not found")


@pytest.fixture(scope="module")
def inline_script() -> str:
    """The validator body, captured once per module."""
    return _extract_inline_python()


def _run_validator(
    inline_script: str,
    tmp_path: Path,
    *,
    classroom50_yaml: str,
    manifest: dict | None,
    submission_tag: str = "submit/2026-06-01T14-32-05Z-a1b2c3d",
    extra_env: dict | None = None,
) -> tuple[int, str, str, dict]:
    """Run the inline validator with hand-crafted env + fixtures.

    Stubs network: the validator's `get(manifest_url)` call hits a
    file:// URL pointing at the local manifest fixture instead of
    GitHub Pages. Manifest=None → no file written, simulating a 404
    (the validator should fail gracefully).

    Returns (exit_code, stdout, stderr, parsed-GITHUB_OUTPUT-as-dict).
    """
    workdir = tmp_path / "workspace"
    workdir.mkdir()
    (workdir / ".classroom50.yaml").write_text(classroom50_yaml)

    pages_root = tmp_path / "pages"
    pages_root.mkdir()
    classroom_dir = pages_root / "cs-test"
    classroom_dir.mkdir()
    if manifest is not None:
        (classroom_dir / "assignments.json").write_text(json.dumps(manifest))

    script_path = tmp_path / "validator.py"
    script_path.write_text(inline_script)

    gh_output = tmp_path / "github-output"
    gh_output.write_text("")

    # Wire the validator's Pages fetch at a local file:// URL so the
    # `get()` helper resolves without a network round-trip. The
    # validator hard-codes `https://{owner}.github.io/classroom50` —
    # we monkey-patch the env-driven owner to be the literal pages
    # root via REPO_OWNER and a small wrapper.

    # Simpler: replace `f"https://{owner}.github.io/classroom50"`
    # with a file:// URL via env-injected sed-on-source. But the
    # script reads `REPO_OWNER` from env and composes the URL itself.
    # We can't intercept that cleanly without rewriting the script.
    #
    # Pragmatic workaround: write a tiny wrapper that monkey-patches
    # urlopen before exec'ing the validator, so the file:// URL is
    # served from the pages_root fixture.

    wrapper = textwrap.dedent(f"""
    import urllib.request
    import urllib.error
    import os
    from pathlib import Path

    _PAGES_ROOT = Path({str(pages_root)!r})

    _real_urlopen = urllib.request.urlopen

    def _file_urlopen(req, *args, **kwargs):
        url = req.full_url if hasattr(req, "full_url") else str(req)
        if url.startswith("https://test-org.github.io/classroom50/"):
            rel = url.removeprefix("https://test-org.github.io/classroom50/")
            target = _PAGES_ROOT / rel
            if not target.is_file():
                raise urllib.error.HTTPError(url, 404, "Not Found", {{}}, None)
            return open(target, "rb")
        return _real_urlopen(req, *args, **kwargs)

    urllib.request.urlopen = _file_urlopen

    with open({str(script_path)!r}) as fh:
        exec(compile(fh.read(), {str(script_path)!r}, "exec"), {{"__name__": "__main__"}})
    """)
    wrapper_path = tmp_path / "wrapper.py"
    wrapper_path.write_text(wrapper)

    env = dict(os.environ)
    env.update({
        "SUBMISSION_TAG": submission_tag,
        "REPO_OWNER": "test-org",
        "GITHUB_OUTPUT": str(gh_output),
        "GITHUB_REF_NAME": submission_tag,
    })
    if extra_env:
        env.update(extra_env)

    proc = subprocess.run(
        [sys.executable, str(wrapper_path)],
        cwd=str(workdir),
        env=env,
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )

    outputs = {}
    if gh_output.exists():
        for line in gh_output.read_text().splitlines():
            if "=" in line:
                k, v = line.split("=", 1)
                outputs[k] = v

    return proc.returncode, proc.stdout, proc.stderr, outputs


def _classroom_yaml(classroom: str = "cs-test", assignment: str = "hello") -> str:
    """Minimum .classroom50.yaml the validator accepts."""
    return f"classroom: {classroom}\nassignment: {assignment}\n"


def _manifest(*, slug: str = "hello", runtime: dict | None = None) -> dict:
    """Minimum assignments.json with one entry, optional runtime block."""
    entry = {
        "slug": slug,
        "name": "Hello",
        "template": {"owner": "x", "repo": "y", "branch": "main"},
        "mode": "individual",
        "autograder": "default",
    }
    if runtime is not None:
        entry["runtime"] = runtime
    return {
        "schema": "classroom50/assignments/v1",
        "assignments": [entry],
    }


# ---------------------------------------------------------------------------
# Happy paths — runtime block is omitted, host-only, or container
# ---------------------------------------------------------------------------


class TestValidatorHappyPaths:
    def test_no_runtime_defaults_to_ubuntu_latest_python_312(self, inline_script, tmp_path):
        rc, _stdout, _stderr, outputs = _run_validator(
            inline_script, tmp_path,
            classroom50_yaml=_classroom_yaml(),
            manifest=_manifest(),
        )
        assert rc == 0
        assert outputs.get("runs-on") == "ubuntu-latest"
        assert outputs.get("python") == "3.12"
        assert outputs.get("container") == "null"

    def test_host_runtime_with_python_apt(self, inline_script, tmp_path):
        rc, _stdout, _stderr, outputs = _run_validator(
            inline_script, tmp_path,
            classroom50_yaml=_classroom_yaml(),
            manifest=_manifest(runtime={
                "runs-on": "ubuntu-22.04",
                "python": "3.11",
                "apt": ["build-essential", "valgrind"],
            }),
        )
        assert rc == 0
        assert outputs["runs-on"] == "ubuntu-22.04"
        assert outputs["python"] == "3.11"
        assert outputs["apt"] == "build-essential valgrind"
        assert outputs["container"] == "null"

    def test_container_with_user_translates_to_options(self, inline_script, tmp_path):
        rc, _stdout, _stderr, outputs = _run_validator(
            inline_script, tmp_path,
            classroom50_yaml=_classroom_yaml(),
            manifest=_manifest(runtime={
                "container": {"image": "cs50/cli:latest", "user": "root"},
            }),
        )
        assert rc == 0
        container = json.loads(outputs["container"])
        assert container["image"] == "cs50/cli:latest"
        assert container["options"] == "--user root"
        # `user` is NOT emitted — Actions doesn't accept container.user.
        assert "user" not in container


# ---------------------------------------------------------------------------
# Allow-by-omission protection (#1 from the audit)
# ---------------------------------------------------------------------------


class TestUnknownKeyRejection:
    """Hand-edited assignments.json with extra keys must be rejected
    by the runtime validator, mirroring Go's DisallowUnknownFields.
    Without this, `container.options: --privileged` would flow through
    to the grade job."""

    def test_unknown_runtime_key_rejected(self, inline_script, tmp_path):
        rc, _stdout, stderr, _outputs = _run_validator(
            inline_script, tmp_path,
            classroom50_yaml=_classroom_yaml(),
            manifest=_manifest(runtime={
                "python": "3.12",
                "options": "--privileged",  # would be smuggled through
            }),
        )
        assert rc != 0
        assert "runtime has unsupported keys" in stderr or "unsupported" in stderr

    def test_unknown_container_key_rejected(self, inline_script, tmp_path):
        rc, _stdout, stderr, _outputs = _run_validator(
            inline_script, tmp_path,
            classroom50_yaml=_classroom_yaml(),
            manifest=_manifest(runtime={
                "container": {
                    "image": "ubuntu:latest",
                    "options": "--privileged",  # the headline P1 attack
                },
            }),
        )
        assert rc != 0
        assert "runtime.container has unsupported keys" in stderr or "unsupported" in stderr


# ---------------------------------------------------------------------------
# Field validation — regex + isinstance guards
# ---------------------------------------------------------------------------


class TestFieldValidation:
    def test_empty_image_rejected_with_clear_message(self, inline_script, tmp_path):
        rc, _stdout, stderr, _outputs = _run_validator(
            inline_script, tmp_path,
            classroom50_yaml=_classroom_yaml(),
            manifest=_manifest(runtime={"container": {"image": ""}}),
        )
        assert rc != 0
        # Empty image now produces a specific message, not the generic
        # "characters other than ..." regex error.
        assert "must not be empty" in stderr

    def test_image_with_shell_metacharacters_rejected(self, inline_script, tmp_path):
        rc, _stdout, stderr, _outputs = _run_validator(
            inline_script, tmp_path,
            classroom50_yaml=_classroom_yaml(),
            manifest=_manifest(runtime={"container": {"image": "ubuntu:24.04;rm -rf /"}}),
        )
        assert rc != 0
        assert "image" in stderr

    def test_python_non_string_rejected(self, inline_script, tmp_path):
        # JSON-numeric python field. Without the isinstance guard, the
        # regex match call would raise TypeError (uncaught → traceback,
        # not a clean fail()).
        rc, _stdout, stderr, _outputs = _run_validator(
            inline_script, tmp_path,
            classroom50_yaml=_classroom_yaml(),
            manifest=_manifest(runtime={"python": 312}),
        )
        assert rc != 0
        # Either caught by isinstance guard → clean fail message,
        # or by the regex check. Either way, no Python traceback.
        assert "Traceback" not in stderr
        assert "python" in stderr

    def test_user_with_shell_metacharacters_rejected(self, inline_script, tmp_path):
        rc, _stdout, stderr, _outputs = _run_validator(
            inline_script, tmp_path,
            classroom50_yaml=_classroom_yaml(),
            manifest=_manifest(runtime={
                "container": {"image": "cs50/cli:latest", "user": "root; rm -rf /"},
            }),
        )
        assert rc != 0
        assert "runtime.container.user" in stderr


# ---------------------------------------------------------------------------
# Auth/identity invariants
# ---------------------------------------------------------------------------


class TestIdentityInvariants:
    def test_classroom50_yaml_missing_classroom_field_rejected(self, inline_script, tmp_path):
        rc, _stdout, stderr, _outputs = _run_validator(
            inline_script, tmp_path,
            classroom50_yaml="assignment: hello\n",  # no classroom
            manifest=_manifest(),
        )
        assert rc != 0
        assert "classroom" in stderr

    def test_assignment_not_in_manifest_rejected(self, inline_script, tmp_path):
        rc, _stdout, stderr, _outputs = _run_validator(
            inline_script, tmp_path,
            classroom50_yaml=_classroom_yaml(assignment="missing-slug"),
            manifest=_manifest(),  # only "hello" registered
        )
        assert rc != 0
        assert "missing-slug" in stderr

    def test_classroom_with_path_traversal_rejected(self, inline_script, tmp_path):
        # The slug regex blocks anything but [a-z0-9-]; a hand-edited
        # .classroom50.yaml with `classroom: ../../etc` should fail.
        rc, _stdout, stderr, _outputs = _run_validator(
            inline_script, tmp_path,
            classroom50_yaml="classroom: ../../etc\nassignment: hello\n",
            manifest=_manifest(),
        )
        assert rc != 0
        assert "classroom" in stderr
