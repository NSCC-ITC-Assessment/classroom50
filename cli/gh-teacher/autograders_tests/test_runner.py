"""Tests for the runner-side bootstrap (`runner.py`).

The runner's job is to (1) download + extract the per-assignment
bundle, (2) resolve the entrypoint (per-assignment override,
classroom default, or vacuous-pass fallback), (3) exec it with the
right env / cwd, and (4) validate / synthesize the autograder's
outputs. These tests cover the data-shape invariants the rest of
the loop depends on plus the URL helpers and HTTP retry logic.

Integration testing of the diagnostic-stub autograder.py (shipped
inside gh-teacher) lives in test_default_autograder.py
(subprocess-driven).
"""

from __future__ import annotations

import datetime
import io
import json
import tarfile
import urllib.error

import pytest

from conftest import runner as ag, collect_scores as cs


# ---------------------------------------------------------------------------
# username_from_repo
# ---------------------------------------------------------------------------


class TestUsernameFromRepo:
    def test_canonical_repo_strips_classroom_assignment_prefix(self):
        # Repo name is `<classroom>-<assignment>-<username>`
        # (lowercased by gh student accept). Strip the prefix.
        assert ag.username_from_repo(
            "cs50/cs-principles-hello-alice",
            "cs-principles",
            "hello",
            "alice",
        ) == "alice"

    def test_case_insensitive_prefix_match(self):
        assert ag.username_from_repo(
            "cs50/cs-principles-hello-alice",
            "CS-Principles",
            "Hello",
            "fallback",
        ) == "alice"

    def test_repo_without_prefix_falls_back_to_actor(self):
        assert ag.username_from_repo(
            "cs50/some-other-repo",
            "cs-principles",
            "hello",
            "fallback-actor",
        ) == "fallback-actor"

    def test_repository_without_slash_is_treated_as_bare_repo(self):
        assert ag.username_from_repo(
            "cs-principles-hello-alice",
            "cs-principles",
            "hello",
            "actor",
        ) == "alice"

    def test_empty_repository_falls_back_to_actor(self):
        assert ag.username_from_repo("", "cs-principles", "hello", "actor") == "actor"

    def test_username_with_hyphens_preserved(self):
        # Usernames can contain hyphens; only the prefix is stripped.
        assert ag.username_from_repo(
            "cs50/cs-principles-hello-alice-the-second",
            "cs-principles",
            "hello",
            "fallback",
        ) == "alice-the-second"


# ---------------------------------------------------------------------------
# URL construction
# ---------------------------------------------------------------------------


class TestUrlConstruction:
    def test_bundle_url_basic(self):
        assert ag.bundle_url(
            "https://cs50.github.io/classroom50", "cs-principles", "hello"
        ) == "https://cs50.github.io/classroom50/cs-principles/autograders/hello.tar.gz"

    def test_bundle_url_url_encodes_segments(self):
        # Slug + classroom validation upstream limits both to
        # [a-z0-9-]; defensive URL-encoding catches any hand-edit
        # that smuggles other chars through.
        assert ag.bundle_url(
            "https://cs50.github.io/classroom50", "cs principles", "hello world"
        ) == "https://cs50.github.io/classroom50/cs%20principles/autograders/hello%20world.tar.gz"

    def test_classroom_default_autograder_url(self):
        # The classroom default lives under the classroom's prefix on
        # Pages — one fetch per submission when no per-assignment
        # override is in the bundle. Different classrooms can have
        # different defaults.
        assert ag.classroom_default_autograder_url(
            "https://cs50.github.io/classroom50",
            "cs-principles",
        ) == "https://cs50.github.io/classroom50/cs-principles/autograder.py"

    def test_classroom_default_autograder_url_quotes_classroom(self):
        # Classroom names with characters that need URL-escaping must
        # round-trip safely (defense in depth — validateShortName
        # blocks most of these at write time).
        assert ag.classroom_default_autograder_url(
            "https://cs50.github.io/classroom50",
            "cs principles",
        ) == "https://cs50.github.io/classroom50/cs%20principles/autograder.py"

    def test_commit_url(self):
        assert ag.commit_url(
            "https://github.com", "cs50/cs-principles-hello-alice", "abc123"
        ) == "https://github.com/cs50/cs-principles-hello-alice/commit/abc123"

    def test_release_url_encodes_submit_slash(self):
        # The submit tag's '/' must be URL-encoded so GitHub's
        # release path resolves correctly.
        result = ag.release_url(
            "https://github.com",
            "cs50/cs-principles-hello-alice",
            "submit/2026-06-01T14-32-05Z",
        )
        assert "submit%2F2026-06-01T14-32-05Z" in result
        assert "/releases/tag/" in result


# ---------------------------------------------------------------------------
# empty_result
# ---------------------------------------------------------------------------


class TestEmptyResult:
    WHEN = datetime.datetime(2026, 6, 1, 14, 33, 11, tzinfo=datetime.timezone.utc)

    BASE_KWARGS = dict(
        classroom="cs-principles",
        assignment="hello",
        username="alice",
        submission="submit/2026-06-01T14-32-05Z-a1b2c3d",
        commit_link="https://github.com/cs50/cs-principles-hello-alice/commit/abc",
        release_link="https://github.com/cs50/cs-principles-hello-alice/releases/tag/submit%2F2026-06-01T14-32-05Z-a1b2c3d",
    )

    def test_schema_and_identity_populated(self):
        result = ag.empty_result(when=self.WHEN, **self.BASE_KWARGS)
        assert result["schema"] == ag.RESULT_SCHEMA_V1
        assert result["classroom"] == "cs-principles"
        assert result["assignment"] == "hello"
        assert result["usernames"] == ["alice"]
        assert result["submission"] == "submit/2026-06-01T14-32-05Z-a1b2c3d"

    def test_zero_scores_and_empty_tests(self):
        result = ag.empty_result(when=self.WHEN, **self.BASE_KWARGS)
        assert result["score"] == 0
        assert result["max-score"] == 0
        assert result["tests"] == []

    def test_review_url_defaults_to_commit_link(self):
        # Review URL points at the same commit by default — teachers
        # can override by writing a non-empty result.json themselves.
        result = ag.empty_result(when=self.WHEN, **self.BASE_KWARGS)
        assert result["review"] == result["commit"]

    def test_datetime_formatted_as_utc_iso8601(self):
        result = ag.empty_result(when=self.WHEN, **self.BASE_KWARGS)
        assert result["datetime"] == "2026-06-01T14:33:11Z"


# ---------------------------------------------------------------------------
# derive_status_and_summary
# ---------------------------------------------------------------------------


class TestDeriveStatusAndSummary:
    def _result(self, tests, *, score=0, max_score=0, assignment="hello"):
        return {
            "tests": tests,
            "score": score,
            "max-score": max_score,
            "assignment": assignment,
        }

    def test_empty_tests_yields_success_with_no_autograder_summary(self):
        # Vacuous pass — the runner's `Finalizer.no_autograder()` path
        # (and any per-assignment autograder that produces an empty
        # tests array) routes through this branch so the framing
        # stays consistent.
        result = self._result(tests=[])
        status, summary = ag.derive_status_and_summary(result)
        assert status == "success"
        assert "no autograder configured" in summary
        assert "hello" in summary

    def test_all_passed_yields_success(self):
        tests = [
            {"test-name": "a", "passed": True, "score": 3, "max-score": 3},
            {"test-name": "b", "passed": True, "score": 2, "max-score": 2},
        ]
        result = self._result(tests=tests, score=5, max_score=5)
        status, summary = ag.derive_status_and_summary(result)
        assert status == "success"
        assert "5/5" in summary
        assert "all tests passed" in summary

    def test_some_failed_yields_failure(self):
        tests = [
            {"test-name": "a", "passed": True, "score": 3, "max-score": 3},
            {"test-name": "b", "passed": False, "score": 0, "max-score": 2},
        ]
        result = self._result(tests=tests, score=3, max_score=5)
        status, summary = ag.derive_status_and_summary(result)
        assert status == "failure"
        assert "3/5" in summary
        assert "1/2" in summary

    def test_all_failed_yields_failure(self):
        tests = [
            {"test-name": "a", "passed": False, "score": 0, "max-score": 3},
            {"test-name": "b", "passed": False, "score": 0, "max-score": 2},
        ]
        result = self._result(tests=tests, score=0, max_score=5)
        status, summary = ag.derive_status_and_summary(result)
        assert status == "failure"
        assert "0/5" in summary
        assert "0/2" in summary


# ---------------------------------------------------------------------------
# render_release_body
# ---------------------------------------------------------------------------


class TestRenderReleaseBody:
    def test_renders_score_header_and_test_table(self):
        result = {
            "score": 3,
            "max-score": 5,
            "tests": [
                {"test-name": "test_a", "passed": True, "score": 3, "max-score": 3},
                {"test-name": "test_b", "passed": False, "score": 0, "max-score": 2},
            ],
        }
        body = ag.render_release_body(result, summary="3/5 passed")
        assert "### classroom50 autograde: 3/5" in body
        assert "| Test | Result | Score |" in body
        assert "| test_a | PASS | 3 / 3 |" in body
        assert "| test_b | FAIL | 0 / 2 |" in body
        assert "Status: 3/5 passed" in body

    def test_empty_tests_renders_summary_only(self):
        result = {"score": 0, "max-score": 0, "tests": []}
        body = ag.render_release_body(
            result, summary="submitted — no autograder configured"
        )
        assert "### classroom50 autograde: 0/0" in body
        assert "no autograder configured" in body
        assert "| Test | Result | Score |" not in body

    def test_pipe_in_test_name_is_escaped(self):
        # Pipes in test names would otherwise break the Markdown table.
        result = {
            "score": 1,
            "max-score": 1,
            "tests": [
                {"test-name": "test_a|with|pipes", "passed": True, "score": 1, "max-score": 1},
            ],
        }
        body = ag.render_release_body(result, summary="all passed")
        assert "test_a\\|with\\|pipes" in body


# ---------------------------------------------------------------------------
# validate_result
# ---------------------------------------------------------------------------


class TestValidateResult:
    # validate_result mirrors collect_scores's strictness so a payload
    # passing the runner also passes the gradebook ingest. The BASE
    # below is a minimum v1-valid payload — every required string,
    # int, and list field present.
    BASE = {
        "schema": "classroom50/result/v1",
        "classroom": "cs-principles",
        "assignment": "hello",
        "usernames": ["alice"],
        "submission": "submit/2026-06-01T14-32-05Z-a1b2c3d",
        "commit": "https://github.com/cs50/cs-principles-hello-alice/commit/abc",
        "release": "https://github.com/cs50/cs-principles-hello-alice/releases/tag/submit%2F...",
        "review": "https://github.com/cs50/cs-principles-hello-alice/commit/abc",
        "datetime": "2026-06-01T14:33:11Z",
        "score": 0,
        "max-score": 0,
        "tests": [],
    }

    def test_valid_payload_returns_none(self):
        assert ag.validate_result(
            self.BASE, classroom="cs-principles", assignment="hello"
        ) is None

    def test_non_dict_rejected(self):
        err = ag.validate_result(
            ["not", "a", "dict"], classroom="cs-principles", assignment="hello"
        )
        assert err is not None
        assert "not a JSON object" in err

    def test_wrong_schema_rejected(self):
        bad = {**self.BASE, "schema": "classroom50/result/v2"}
        err = ag.validate_result(bad, classroom="cs-principles", assignment="hello")
        assert err is not None
        assert "schema" in err

    def test_classroom_mismatch_rejected(self):
        bad = {**self.BASE, "classroom": "wrong-room"}
        err = ag.validate_result(bad, classroom="cs-principles", assignment="hello")
        assert err is not None
        assert "classroom" in err

    def test_assignment_mismatch_rejected(self):
        bad = {**self.BASE, "assignment": "wrong-slug"}
        err = ag.validate_result(bad, classroom="cs-principles", assignment="hello")
        assert err is not None
        assert "assignment" in err

    def test_tests_not_a_list_rejected(self):
        bad = {**self.BASE, "tests": {"not": "a-list"}}
        err = ag.validate_result(bad, classroom="cs-principles", assignment="hello")
        assert err is not None
        assert "tests" in err

    def test_usernames_wrong_shape_rejected(self):
        for bad_usernames in (None, "alice", ["a", "b"], [], [""]):
            bad = {**self.BASE, "usernames": bad_usernames}
            err = ag.validate_result(bad, classroom="cs-principles", assignment="hello")
            assert err is not None, f"{bad_usernames!r} should be rejected"
            assert "usernames" in err

    def test_submission_wrong_shape_rejected(self):
        for bad_sub in (None, 42, "not-a-submit-tag", "submit", ""):
            bad = {**self.BASE, "submission": bad_sub}
            err = ag.validate_result(bad, classroom="cs-principles", assignment="hello")
            assert err is not None, f"{bad_sub!r} should be rejected"
            assert "submission" in err

    def test_score_must_be_non_negative_int(self):
        for bad in (-1, "5", 5.5, True):
            payload = {**self.BASE, "score": bad}
            err = ag.validate_result(payload, classroom="cs-principles", assignment="hello")
            assert err is not None, f"{bad!r} should be rejected"
            assert "score" in err

    def test_score_greater_than_max_rejected(self):
        bad = {**self.BASE, "score": 5, "max-score": 4}
        err = ag.validate_result(bad, classroom="cs-principles", assignment="hello")
        assert err is not None
        assert "score" in err and "max-score" in err

    def test_test_entry_must_be_dict(self):
        bad = {**self.BASE, "tests": ["just a string"]}
        err = ag.validate_result(bad, classroom="cs-principles", assignment="hello")
        assert err is not None
        assert "tests[0]" in err

    def test_test_entry_field_validation(self):
        for bad_test, expected in [
            ({"test-name": "", "passed": True, "score": 0, "max-score": 0}, "test-name"),
            ({"test-name": "x", "passed": "yes", "score": 0, "max-score": 0}, "passed"),
            ({"test-name": "x", "passed": True, "score": -1, "max-score": 0}, "score"),
            ({"test-name": "x", "passed": True, "score": True, "max-score": 1}, "score"),
            ({"test-name": "x", "passed": True, "score": 0, "max-score": -1}, "max-score"),
            ({"test-name": "x", "passed": True, "score": 5, "max-score": 4}, "score"),
        ]:
            payload = {**self.BASE, "tests": [bad_test]}
            err = ag.validate_result(payload, classroom="cs-principles", assignment="hello")
            assert err is not None, f"{bad_test!r} should be rejected"
            assert expected in err

    def test_extra_fields_allowed(self):
        # The autograder is free to add fields beyond the v1 baseline
        # (e.g. its own diagnostic block); we only check the contract.
        ok = {**self.BASE, "extra_field": "anything"}
        assert ag.validate_result(
            ok, classroom="cs-principles", assignment="hello"
        ) is None


# ---------------------------------------------------------------------------
# extract_tarball
# ---------------------------------------------------------------------------


class TestExtractTarball:
    def _build_tarball(self, files: dict[str, str]) -> bytes:
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w:gz") as tar:
            for name, content in files.items():
                data = content.encode("utf-8")
                info = tarfile.TarInfo(name=name)
                info.size = len(data)
                tar.addfile(info, io.BytesIO(data))
        return buf.getvalue()

    def test_extracts_files_to_dest(self, tmp_path):
        data = self._build_tarball({
            "hello/autograder.py": "# entrypoint\n",
            "hello/test_helper.py": "# fixture\n",
        })
        ag.extract_tarball(data, tmp_path)
        assert (tmp_path / "hello" / "autograder.py").is_file()
        assert (tmp_path / "hello" / "test_helper.py").is_file()

    def test_corrupt_tarball_raises(self, tmp_path):
        with pytest.raises(tarfile.TarError):
            ag.extract_tarball(b"not a tarball", tmp_path)

    def test_path_traversal_blocked(self, tmp_path):
        # Python 3.12+ filter='data' refuses members with absolute
        # paths or `..` segments. Defense-in-depth even though the
        # bundle comes from the teacher's config repo.
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w:gz") as tar:
            data = b"hostile"
            info = tarfile.TarInfo(name="../../etc/evil")
            info.size = len(data)
            tar.addfile(info, io.BytesIO(data))
        with pytest.raises((tarfile.TarError, OSError, ValueError)):
            ag.extract_tarball(buf.getvalue(), tmp_path)


class TestSafeExtractallLegacy:
    """Exercises the <3.12 fallback explicitly, regardless of the
    interpreter pytest itself runs on. Teachers can pin
    `runtime.python: 3.10` (or run inside a container with an older
    python), and runner.py must produce identical safety guarantees
    on both code paths."""

    def _build_tarball(self, mutate) -> bytes:
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w:gz") as tar:
            mutate(tar)
        return buf.getvalue()

    def _open(self, blob: bytes) -> tarfile.TarFile:
        return tarfile.open(fileobj=io.BytesIO(blob), mode="r:gz")

    def test_extracts_regular_files(self, tmp_path):
        def mutate(tar):
            for name, content in (("hello/autograder.py", b"# entry\n"),
                                  ("hello/fixture.py", b"# helper\n")):
                info = tarfile.TarInfo(name=name)
                info.size = len(content)
                tar.addfile(info, io.BytesIO(content))
        with self._open(self._build_tarball(mutate)) as tar:
            ag._safe_extractall_legacy(tar, tmp_path)
        assert (tmp_path / "hello" / "autograder.py").is_file()
        assert (tmp_path / "hello" / "fixture.py").is_file()

    def test_rejects_parent_traversal(self, tmp_path):
        def mutate(tar):
            info = tarfile.TarInfo(name="../../etc/evil")
            info.size = 1
            tar.addfile(info, io.BytesIO(b"x"))
        with self._open(self._build_tarball(mutate)) as tar:
            with pytest.raises(ValueError, match="unsafe tar path"):
                ag._safe_extractall_legacy(tar, tmp_path)

    def test_rejects_absolute_path(self, tmp_path):
        def mutate(tar):
            info = tarfile.TarInfo(name="/etc/evil")
            info.size = 1
            tar.addfile(info, io.BytesIO(b"x"))
        with self._open(self._build_tarball(mutate)) as tar:
            with pytest.raises(ValueError, match="unsafe tar path"):
                ag._safe_extractall_legacy(tar, tmp_path)

    def test_rejects_symlink(self, tmp_path):
        def mutate(tar):
            info = tarfile.TarInfo(name="link")
            info.type = tarfile.SYMTYPE
            info.linkname = "/etc/passwd"
            tar.addfile(info)
        with self._open(self._build_tarball(mutate)) as tar:
            with pytest.raises(ValueError, match="unsupported tar member type"):
                ag._safe_extractall_legacy(tar, tmp_path)

    def test_rejects_hardlink(self, tmp_path):
        def mutate(tar):
            info = tarfile.TarInfo(name="link")
            info.type = tarfile.LNKTYPE
            info.linkname = "/etc/passwd"
            tar.addfile(info)
        with self._open(self._build_tarball(mutate)) as tar:
            with pytest.raises(ValueError, match="unsupported tar member type"):
                ag._safe_extractall_legacy(tar, tmp_path)

    def test_rejects_fifo(self, tmp_path):
        def mutate(tar):
            info = tarfile.TarInfo(name="pipe")
            info.type = tarfile.FIFOTYPE
            tar.addfile(info)
        with self._open(self._build_tarball(mutate)) as tar:
            with pytest.raises(ValueError, match="unsupported tar member type"):
                ag._safe_extractall_legacy(tar, tmp_path)


# ---------------------------------------------------------------------------
# fetch_url (HTTP layer with retry)
# ---------------------------------------------------------------------------


class _FakeResponse:
    def __init__(self, body: bytes):
        self._body = body

    def read(self, n: int | None = None) -> bytes:
        if n is None:
            return self._body
        return self._body[:n]

    def __enter__(self):
        return self

    def __exit__(self, *args):
        return False


class TestFetchUrl:
    def test_returns_body_on_200(self, monkeypatch):
        def fake_urlopen(req, timeout=None):
            return _FakeResponse(b"some bytes")

        monkeypatch.setattr(ag.urllib.request, "urlopen", fake_urlopen)
        body = ag.fetch_url("https://example.invalid/x")
        assert body == b"some bytes"

    def test_returns_none_on_404(self, monkeypatch):
        # 404 is the "not present" signal — for the bundle URL it
        # means "no per-assignment override"; for the default-
        # autograder URL it means publish-pages.yaml hasn't run yet.
        def fake_urlopen(req, timeout=None):
            raise urllib.error.HTTPError(req.full_url, 404, "Not Found", {}, None)

        monkeypatch.setattr(ag.urllib.request, "urlopen", fake_urlopen)
        assert ag.fetch_url("https://example.invalid/x") is None

    def test_retries_on_5xx_then_succeeds(self, monkeypatch):
        calls = {"n": 0}

        def fake_urlopen(req, timeout=None):
            calls["n"] += 1
            if calls["n"] < 3:
                raise urllib.error.HTTPError(req.full_url, 503, "Service Unavailable", {}, None)
            return _FakeResponse(b"ok")

        monkeypatch.setattr(ag.urllib.request, "urlopen", fake_urlopen)
        monkeypatch.setattr(ag.time, "sleep", lambda _s: None)
        assert ag.fetch_url("https://example.invalid/x") == b"ok"
        assert calls["n"] == 3

    def test_non_404_4xx_raises_immediately(self, monkeypatch):
        calls = {"n": 0}

        def fake_urlopen(req, timeout=None):
            calls["n"] += 1
            raise urllib.error.HTTPError(req.full_url, 403, "Forbidden", {}, None)

        monkeypatch.setattr(ag.urllib.request, "urlopen", fake_urlopen)
        with pytest.raises(urllib.error.HTTPError):
            ag.fetch_url("https://example.invalid/x")
        assert calls["n"] == 1

    def test_exceeds_max_size_raises(self, monkeypatch):
        oversized = b"x" * (ag.MAX_FETCH_BYTES + 100)

        def fake_urlopen(req, timeout=None):
            return _FakeResponse(oversized)

        monkeypatch.setattr(ag.urllib.request, "urlopen", fake_urlopen)
        with pytest.raises(ValueError, match="exceeds.*byte ceiling"):
            ag.fetch_url("https://example.invalid/x")

    def test_retries_on_url_error_then_succeeds(self, monkeypatch):
        # URLError is the network-layer fallback (DNS, TCP reset,
        # truncated body). Same retry contract as 5xx: backoff up to
        # FETCH_ATTEMPTS times, succeed on later attempt.
        calls = {"n": 0}

        def fake_urlopen(req, timeout=None):
            calls["n"] += 1
            if calls["n"] < 3:
                raise urllib.error.URLError("connection reset")
            return _FakeResponse(b"ok")

        monkeypatch.setattr(ag.urllib.request, "urlopen", fake_urlopen)
        monkeypatch.setattr(ag.time, "sleep", lambda _s: None)
        assert ag.fetch_url("https://example.invalid/x") == b"ok"
        assert calls["n"] == 3

    def test_5xx_retry_exhaustion_raises(self, monkeypatch):
        # All attempts fail with 5xx → raise the last error rather
        # than swallowing it.
        calls = {"n": 0}

        def fake_urlopen(req, timeout=None):
            calls["n"] += 1
            raise urllib.error.HTTPError(req.full_url, 503, "Service Unavailable", {}, None)

        monkeypatch.setattr(ag.urllib.request, "urlopen", fake_urlopen)
        monkeypatch.setattr(ag.time, "sleep", lambda _s: None)
        with pytest.raises(urllib.error.HTTPError):
            ag.fetch_url("https://example.invalid/x")
        assert calls["n"] == ag.FETCH_ATTEMPTS

    def test_url_error_retry_exhaustion_raises(self, monkeypatch):
        calls = {"n": 0}

        def fake_urlopen(req, timeout=None):
            calls["n"] += 1
            raise urllib.error.URLError("connection reset")

        monkeypatch.setattr(ag.urllib.request, "urlopen", fake_urlopen)
        monkeypatch.setattr(ag.time, "sleep", lambda _s: None)
        with pytest.raises(urllib.error.URLError):
            ag.fetch_url("https://example.invalid/x")
        assert calls["n"] == ag.FETCH_ATTEMPTS


# ---------------------------------------------------------------------------
# output_has_status / append_outputs
# ---------------------------------------------------------------------------


class TestOutputHasStatus:
    def test_returns_false_when_unset(self):
        assert ag.output_has_status(None) is False

    def test_returns_false_when_file_missing(self, tmp_path):
        assert ag.output_has_status(str(tmp_path / "does-not-exist")) is False

    def test_returns_false_when_file_empty(self, tmp_path):
        path = tmp_path / "empty"
        path.write_text("")
        assert ag.output_has_status(str(path)) is False

    def test_returns_true_when_status_present(self, tmp_path):
        path = tmp_path / "out"
        path.write_text("status=success\nsummary=ok\n")
        assert ag.output_has_status(str(path)) is True

    def test_returns_false_when_only_unrelated_keys(self, tmp_path):
        path = tmp_path / "out"
        path.write_text("other-key=value\nanother=foo\n")
        assert ag.output_has_status(str(path)) is False


class TestAppendOutputs:
    def test_writes_status_and_summary(self, tmp_path):
        path = tmp_path / "out"
        path.write_text("")
        ag.append_outputs(str(path), "success", "all good")
        text = path.read_text()
        assert "status=success\n" in text
        assert "summary=all good\n" in text

    def test_summary_newlines_replaced_with_spaces(self, tmp_path):
        # $GITHUB_OUTPUT is line-oriented; newlines in values would
        # break parsing. Defensive scrub.
        path = tmp_path / "out"
        path.write_text("")
        ag.append_outputs(str(path), "error", "line one\nline two\rline three")
        text = path.read_text()
        assert "summary=line one line two line three\n" in text
        assert text.count("\n") == 2

    def test_no_path_does_not_crash(self):
        # Running locally for development — GITHUB_OUTPUT may not be set.
        ag.append_outputs(None, "success", "ok")  # no exception


# ---------------------------------------------------------------------------
# Finalizer (error path synthesis)
# ---------------------------------------------------------------------------


class TestFinalizer:
    def _finalizer(self, *, workspace, github_output):
        return ag.Finalizer(
            workspace=workspace,
            github_output=str(github_output) if github_output else None,
            classroom="cs-principles",
            assignment="hello",
            username="alice",
            submission="submit/2026-06-01T14-32-05Z-a1b2c3d",
            commit_link="https://github.com/cs50/cs-principles-hello-alice/commit/abc",
            release_link="https://github.com/cs50/cs-principles-hello-alice/releases/tag/submit%2F2026-06-01T14-32-05Z-a1b2c3d",
        )

    def test_writes_synthesized_result_and_release_body(self, tmp_path):
        gh_output = tmp_path / "out"
        gh_output.write_text("")
        f = self._finalizer(workspace=tmp_path, github_output=gh_output)

        assert f.error("bundle fetch failed: Connection refused") == 0

        result = json.loads((tmp_path / "result.json").read_text())
        assert result["schema"] == ag.RESULT_SCHEMA_V1
        assert result["classroom"] == "cs-principles"
        assert result["assignment"] == "hello"
        assert result["tests"] == []
        assert result["score"] == 0
        assert result["max-score"] == 0

        body = (tmp_path / "release-body.md").read_text()
        assert "### classroom50 autograde: 0/0" in body
        assert "bundle fetch failed" in body

    def test_writes_status_error_to_github_output(self, tmp_path):
        gh_output = tmp_path / "out"
        gh_output.write_text("")
        f = self._finalizer(workspace=tmp_path, github_output=gh_output)
        f.error("something broke")
        text = gh_output.read_text()
        assert "status=error\n" in text
        assert "something broke" in text

    def test_overwrites_stale_status_from_autograder(self, tmp_path):
        # Autograder may have written status=success before exiting
        # non-zero. The finalizer appends status=error so the final
        # state in the file ends with the error — append_outputs is
        # always called regardless of prior content.
        gh_output = tmp_path / "out"
        gh_output.write_text("status=success\nsummary=fake\n")
        f = self._finalizer(workspace=tmp_path, github_output=gh_output)
        f.error("real failure")
        lines = gh_output.read_text().splitlines()
        # Last status= line wins by GitHub Actions semantics; ours is appended.
        last_status = next(l for l in reversed(lines) if l.startswith("status="))
        assert last_status == "status=error"

    def test_no_github_output_does_not_crash(self, tmp_path):
        f = self._finalizer(workspace=tmp_path, github_output=None)
        f.error("no env var set")
        # result.json + release-body.md still written.
        assert (tmp_path / "result.json").is_file()
        assert (tmp_path / "release-body.md").is_file()

    def test_no_autograder_synthesizes_vacuous_pass(self, tmp_path):
        # Lean-scaffold path: classroom hasn't run set-default and
        # the assignment has no override either. Distinct from
        # error() — this is a valid mid-setup state, so status is
        # success (not error) and the gradebook records 0/0.
        gh_output = tmp_path / "out"
        gh_output.write_text("")
        f = self._finalizer(workspace=tmp_path, github_output=gh_output)

        assert f.no_autograder() == 0

        result = json.loads((tmp_path / "result.json").read_text())
        assert result["schema"] == ag.RESULT_SCHEMA_V1
        assert result["tests"] == []
        assert result["score"] == 0
        assert result["max-score"] == 0

        body = (tmp_path / "release-body.md").read_text()
        assert "### classroom50 autograde: 0/0" in body
        assert "no autograder configured" in body

        text = gh_output.read_text()
        assert "status=success\n" in text
        assert "no autograder configured" in text


# ---------------------------------------------------------------------------
# Schema sentinel pinning
# ---------------------------------------------------------------------------


def test_result_schema_sentinel_matches_collect_scores():
    # Cross-binary contract — must match RESULT_SCHEMA_V1 in
    # cli/gh-teacher/skeleton/dotgithub/scripts/collect_scores.py
    # and the Go-side consts.
    # Compare the live constants from both modules instead of
    # asserting against a hardcoded string — a bump in one without
    # the other was previously invisible to the test suite.
    assert ag.RESULT_SCHEMA_V1 == cs.RESULT_SCHEMA_V1
    assert ag.RESULT_SCHEMA_V1 == "classroom50/result/v1"


def test_submit_tag_prefix_matches_collect_scores():
    # Cross-binary contract: collect_scores filters releases on this
    # prefix; the runner workflow tags with it. A drift would silently
    # drop submissions from the gradebook.
    assert cs.SUBMIT_TAG_PREFIX == "submit/"


def test_result_asset_name_matches_collect_scores():
    # Cross-binary contract: the runner workflow uploads `result.json`;
    # collect_scores fetches by this exact name.
    assert cs.RESULT_ASSET_NAME == "result.json"
