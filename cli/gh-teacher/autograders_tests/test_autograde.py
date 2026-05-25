"""Pure-helper tests for `autograde.py`.

The HTTP / subprocess layer is exercised end-to-end by the
functional smoke test against a live classroom; these tests focus
on the data-shape invariants the rest of the loop depends on:
score-marker extraction, test-entry construction, result.json
assembly, status/summary derivation, URL construction, and tarball
extraction safety.
"""

from __future__ import annotations

import datetime
import io
import json
import pathlib
import tarfile
import urllib.error

import pytest

from conftest import autograde as ag


# ---------------------------------------------------------------------------
# username_from_repo
# ---------------------------------------------------------------------------


class TestUsernameFromRepo:
    def test_canonical_repo_strips_classroom_assignment_prefix(self):
        # The well-trodden path: repo name is `<classroom>-<assignment>-<username>`
        # (lowercased by gh student accept). Strip the prefix to recover username.
        assert ag.username_from_repo(
            "cs50/cs-principles-hello-alice",
            "cs-principles",
            "hello",
            "alice",
        ) == "alice"

    def test_case_insensitive_prefix_match(self):
        # Repo names are lowercased on accept; case mismatch in the classroom
        # or assignment shouldn't trip the prefix check.
        assert ag.username_from_repo(
            "cs50/cs-principles-hello-alice",
            "CS-Principles",
            "Hello",
            "fallback",
        ) == "alice"

    def test_repo_without_prefix_falls_back_to_actor(self):
        # Hand-created repo for testing — doesn't follow the contract.
        assert ag.username_from_repo(
            "cs50/some-other-repo",
            "cs-principles",
            "hello",
            "fallback-actor",
        ) == "fallback-actor"

    def test_repository_without_slash_is_treated_as_bare_repo(self):
        # Defensive: GITHUB_REPOSITORY should always have owner/, but if
        # it doesn't we shouldn't crash on the split.
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
# extract_score_marker
# ---------------------------------------------------------------------------


class TestExtractScoreMarker:
    def test_marker_present_with_int_returns_value(self):
        test = {"user_properties": [["classroom50_score", 5]]}
        assert ag.extract_score_marker(test) == 5

    def test_marker_present_as_tuple_returns_value(self):
        # pytest-json-report serializes user_properties as a list, but
        # be defensive — tuples should also work.
        test = {"user_properties": [("classroom50_score", 3)]}
        assert ag.extract_score_marker(test) == 3

    def test_marker_absent_returns_none(self):
        # Test without @pytest.mark.score(N) — caller defaults to 1.
        test = {"user_properties": []}
        assert ag.extract_score_marker(test) is None

    def test_user_properties_missing_returns_none(self):
        test = {}
        assert ag.extract_score_marker(test) is None

    def test_user_properties_not_a_list_returns_none(self):
        # A hand-edited / malformed pytest report shouldn't crash us.
        test = {"user_properties": "garbage"}
        assert ag.extract_score_marker(test) is None

    def test_marker_with_bool_value_returns_none(self):
        # @pytest.mark.score(True) is nonsense; don't treat True as 1.
        test = {"user_properties": [["classroom50_score", True]]}
        assert ag.extract_score_marker(test) is None

    def test_marker_with_string_value_returns_none(self):
        # @pytest.mark.score("5") — string, not int. Default to 1.
        test = {"user_properties": [["classroom50_score", "5"]]}
        assert ag.extract_score_marker(test) is None

    def test_marker_with_float_value_returns_none(self):
        # Only integer scores are allowed (the v1 schema mandates int).
        test = {"user_properties": [["classroom50_score", 2.5]]}
        assert ag.extract_score_marker(test) is None

    def test_marker_with_negative_value_returns_none(self):
        # @pytest.mark.score(-3) is nonsensical; reject as malformed.
        test = {"user_properties": [["classroom50_score", -3]]}
        assert ag.extract_score_marker(test) is None

    def test_marker_with_zero_returns_zero(self):
        # Zero-point tests (e.g. style-check that's informational only).
        test = {"user_properties": [["classroom50_score", 0]]}
        assert ag.extract_score_marker(test) == 0

    def test_first_classroom50_score_marker_wins(self):
        # If multiple are present (shouldn't happen but be defensive),
        # the first wins — pytest's marker order is documented.
        test = {
            "user_properties": [
                ["other_prop", "x"],
                ["classroom50_score", 4],
                ["classroom50_score", 7],
            ]
        }
        assert ag.extract_score_marker(test) == 4

    def test_malformed_user_property_entries_skipped(self):
        # A short or non-pair user_properties entry shouldn't trip the
        # iteration — skip and continue.
        test = {
            "user_properties": [
                "garbage",
                ["only-one-elem"],
                ["classroom50_score", 2],
            ]
        }
        assert ag.extract_score_marker(test) == 2


# ---------------------------------------------------------------------------
# build_test_entry
# ---------------------------------------------------------------------------


class TestBuildTestEntry:
    def test_passing_test_with_marker_scores_full_value(self):
        entry = ag.build_test_entry({
            "nodeid": "test_hello.py::test_says_hello",
            "outcome": "passed",
            "user_properties": [["classroom50_score", 4]],
        })
        assert entry == {
            "test-name": "test_hello.py::test_says_hello",
            "passed": True,
            "score": 4,
            "max-score": 4,
        }

    def test_passing_test_without_marker_defaults_to_one(self):
        entry = ag.build_test_entry({
            "nodeid": "test_hello.py::test_says_hello",
            "outcome": "passed",
            "user_properties": [],
        })
        assert entry["score"] == 1
        assert entry["max-score"] == 1
        assert entry["passed"] is True

    def test_failing_test_with_marker_scores_zero(self):
        entry = ag.build_test_entry({
            "nodeid": "test_hello.py::test_fails",
            "outcome": "failed",
            "user_properties": [["classroom50_score", 5]],
        })
        assert entry["score"] == 0
        assert entry["max-score"] == 5
        assert entry["passed"] is False

    def test_skipped_test_counts_as_zero_passed(self):
        # Skipped != passed. Counts against the max-score total.
        entry = ag.build_test_entry({
            "nodeid": "test_hello.py::test_skipped",
            "outcome": "skipped",
            "user_properties": [["classroom50_score", 2]],
        })
        assert entry["passed"] is False
        assert entry["score"] == 0
        assert entry["max-score"] == 2

    def test_error_test_counts_as_zero_passed(self):
        # Errored tests (e.g. fixture raised) also count as failed.
        entry = ag.build_test_entry({
            "nodeid": "test_hello.py::test_errored",
            "outcome": "error",
            "user_properties": [],
        })
        assert entry["passed"] is False
        assert entry["score"] == 0
        assert entry["max-score"] == 1

    def test_missing_nodeid_falls_back_to_empty_string(self):
        # Defensive — pytest reports should always have a nodeid.
        entry = ag.build_test_entry({"outcome": "passed"})
        assert entry["test-name"] == ""

    def test_zero_score_marker_is_honored(self):
        entry = ag.build_test_entry({
            "nodeid": "test_style.py::test_pep8",
            "outcome": "passed",
            "user_properties": [["classroom50_score", 0]],
        })
        assert entry["score"] == 0
        assert entry["max-score"] == 0


# ---------------------------------------------------------------------------
# build_result
# ---------------------------------------------------------------------------


class TestBuildResult:
    WHEN = datetime.datetime(2026, 6, 1, 14, 33, 11, tzinfo=datetime.timezone.utc)

    BASE_KWARGS = dict(
        classroom="cs-principles",
        assignment="hello",
        username="alice",
        submission="submit/2026-06-01T14-32-05Z",
        commit_url="https://github.com/cs50/cs-principles-hello-alice/commit/abc",
        release_url="https://github.com/cs50/cs-principles-hello-alice/releases/tag/submit%2F2026-06-01T14-32-05Z",
        review_url="https://github.com/cs50/cs-principles-hello-alice/commit/abc",
    )

    def test_schema_and_identity_populated(self):
        result = ag.build_result(when=self.WHEN, tests=[], **self.BASE_KWARGS)
        assert result["schema"] == ag.RESULT_SCHEMA_V1
        assert result["classroom"] == "cs-principles"
        assert result["assignment"] == "hello"
        assert result["usernames"] == ["alice"]
        assert result["submission"] == "submit/2026-06-01T14-32-05Z"

    def test_empty_tests_yields_zero_scores(self):
        # The "no tests configured" path — valid v1 payload, score 0/0.
        result = ag.build_result(when=self.WHEN, tests=[], **self.BASE_KWARGS)
        assert result["score"] == 0
        assert result["max-score"] == 0
        assert result["tests"] == []

    def test_scores_sum_across_tests(self):
        tests = [
            {"test-name": "a", "passed": True, "score": 3, "max-score": 3},
            {"test-name": "b", "passed": False, "score": 0, "max-score": 2},
            {"test-name": "c", "passed": True, "score": 1, "max-score": 1},
        ]
        result = ag.build_result(when=self.WHEN, tests=tests, **self.BASE_KWARGS)
        assert result["score"] == 4
        assert result["max-score"] == 6

    def test_datetime_formatted_as_utc_iso8601(self):
        result = ag.build_result(when=self.WHEN, tests=[], **self.BASE_KWARGS)
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

    def test_empty_tests_yields_success_with_no_tests_summary(self):
        # Vacuous pass — "submitted, no tests configured".
        result = self._result(tests=[])
        status, summary = ag.derive_status_and_summary(result)
        assert status == "success"
        assert "no tests configured" in summary
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
        assert "1/2" in summary  # 1 of 2 tests passed

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

    def test_fallback_summary_overrides_default_no_tests_text(self):
        # Used by the error paths to surface a different "no tests"
        # message (e.g. "test tarball fetch failed").
        result = self._result(tests=[])
        status, summary = ag.derive_status_and_summary(
            result, fallback_summary="tarball fetch failed"
        )
        assert status == "success"
        assert summary == "tarball fetch failed"


# ---------------------------------------------------------------------------
# build_release_body
# ---------------------------------------------------------------------------


class TestBuildReleaseBody:
    def test_renders_score_header_and_test_table(self):
        result = {
            "score": 3,
            "max-score": 5,
            "tests": [
                {"test-name": "test_a", "passed": True, "score": 3, "max-score": 3},
                {"test-name": "test_b", "passed": False, "score": 0, "max-score": 2},
            ],
        }
        body = ag.build_release_body(result, summary="3/5 passed")
        assert "### Classroom50 autograde: 3/5" in body
        assert "| Test | Result | Score |" in body
        assert "| test_a | PASS | 3 / 3 |" in body
        assert "| test_b | FAIL | 0 / 2 |" in body
        assert "Status: 3/5 passed" in body

    def test_empty_tests_renders_no_tests_note(self):
        result = {"score": 0, "max-score": 0, "tests": []}
        body = ag.build_release_body(
            result, summary="submitted — no tests configured"
        )
        assert "### Classroom50 autograde: 0/0" in body
        assert "no tests configured" in body
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
        body = ag.build_release_body(result, summary="all passed")
        assert "test_a\\|with\\|pipes" in body


# ---------------------------------------------------------------------------
# URL construction
# ---------------------------------------------------------------------------


class TestUrlConstruction:
    def test_tests_tarball_url_basic(self):
        assert ag.tests_tarball_url(
            "https://cs50.github.io/classroom50/cs-principles", "hello"
        ) == "https://cs50.github.io/classroom50/cs-principles/autograders/tests/hello.tar.gz"

    def test_commit_url(self):
        assert ag.commit_url(
            "https://github.com", "cs50/cs-principles-hello-alice", "abc123"
        ) == "https://github.com/cs50/cs-principles-hello-alice/commit/abc123"

    def test_release_url_encodes_submit_slash(self):
        # The submit tag's '/' must be URL-encoded so GitHub's release
        # path resolves correctly.
        result = ag.release_url(
            "https://github.com",
            "cs50/cs-principles-hello-alice",
            "submit/2026-06-01T14-32-05Z",
        )
        assert "submit%2F2026-06-01T14-32-05Z" in result
        assert "/releases/tag/" in result


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
            "hello/test_hello.py": "def test_x(): pass\n",
            "hello/conftest.py": "# fixtures\n",
        })
        ag.extract_tarball(data, tmp_path)
        assert (tmp_path / "hello" / "test_hello.py").is_file()
        assert (tmp_path / "hello" / "conftest.py").is_file()

    def test_corrupt_tarball_raises(self, tmp_path):
        with pytest.raises(tarfile.TarError):
            ag.extract_tarball(b"not a tarball", tmp_path)

    def test_path_traversal_blocked(self, tmp_path):
        # The Python 3.12 `filter='data'` default refuses members with
        # absolute paths or `..` segments. Defense-in-depth even though
        # the tarball comes from the teacher's config repo.
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w:gz") as tar:
            data = b"hostile"
            info = tarfile.TarInfo(name="../../etc/evil")
            info.size = len(data)
            tar.addfile(info, io.BytesIO(data))
        with pytest.raises((tarfile.TarError, OSError, ValueError)):
            ag.extract_tarball(buf.getvalue(), tmp_path)


# ---------------------------------------------------------------------------
# download_tests_tarball (via stubbed urlopen)
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


class TestDownloadTestsTarball:
    def test_returns_body_on_200(self, monkeypatch):
        def fake_urlopen(req, timeout=None):
            return _FakeResponse(b"tarball-bytes")

        monkeypatch.setattr(ag.urllib.request, "urlopen", fake_urlopen)
        body = ag.download_tests_tarball("https://example.invalid/cs", "hello")
        assert body == b"tarball-bytes"

    def test_returns_none_on_404(self, monkeypatch):
        def fake_urlopen(req, timeout=None):
            raise urllib.error.HTTPError(req.full_url, 404, "Not Found", {}, None)

        monkeypatch.setattr(ag.urllib.request, "urlopen", fake_urlopen)
        body = ag.download_tests_tarball("https://example.invalid/cs", "hello")
        assert body is None

    def test_retries_on_5xx_then_succeeds(self, monkeypatch):
        # Exercise the retry loop: fail twice with 503, succeed third.
        calls = {"n": 0}

        def fake_urlopen(req, timeout=None):
            calls["n"] += 1
            if calls["n"] < 3:
                raise urllib.error.HTTPError(req.full_url, 503, "Service Unavailable", {}, None)
            return _FakeResponse(b"ok")

        monkeypatch.setattr(ag.urllib.request, "urlopen", fake_urlopen)
        monkeypatch.setattr(ag.time, "sleep", lambda _s: None)  # don't actually sleep
        body = ag.download_tests_tarball("https://example.invalid/cs", "hello")
        assert body == b"ok"
        assert calls["n"] == 3

    def test_non_404_4xx_raises_immediately(self, monkeypatch):
        calls = {"n": 0}

        def fake_urlopen(req, timeout=None):
            calls["n"] += 1
            raise urllib.error.HTTPError(req.full_url, 403, "Forbidden", {}, None)

        monkeypatch.setattr(ag.urllib.request, "urlopen", fake_urlopen)
        with pytest.raises(urllib.error.HTTPError):
            ag.download_tests_tarball("https://example.invalid/cs", "hello")
        assert calls["n"] == 1  # no retry on 4xx

    def test_exceeds_max_size_raises(self, monkeypatch):
        oversized = b"x" * (ag.MAX_TARBALL_BYTES + 100)

        def fake_urlopen(req, timeout=None):
            return _FakeResponse(oversized)

        monkeypatch.setattr(ag.urllib.request, "urlopen", fake_urlopen)
        with pytest.raises(ValueError, match="exceeds.*byte ceiling"):
            ag.download_tests_tarball("https://example.invalid/cs", "hello")


# ---------------------------------------------------------------------------
# write_outputs
# ---------------------------------------------------------------------------


class TestWriteOutputs:
    def test_writes_result_release_body_and_github_output(self, tmp_path):
        result = {
            "schema": ag.RESULT_SCHEMA_V1,
            "score": 3,
            "max-score": 5,
            "tests": [
                {"test-name": "t", "passed": True, "score": 3, "max-score": 3},
            ],
        }
        result_path = tmp_path / "result.json"
        body_path = tmp_path / "release-body.md"
        gh_output = tmp_path / "github-output"
        gh_output.write_text("")  # touch

        ag.write_outputs(
            result=result,
            status="failure",
            summary="3/5 passed",
            result_path=result_path,
            release_body_path=body_path,
            github_output_path=str(gh_output),
        )

        assert json.loads(result_path.read_text())["score"] == 3
        assert "### Classroom50 autograde: 3/5" in body_path.read_text()
        gh = gh_output.read_text()
        assert "status=failure\n" in gh
        assert "summary=3/5 passed\n" in gh

    def test_summary_newlines_replaced_with_spaces(self, tmp_path):
        # $GITHUB_OUTPUT is line-oriented (key=value); a newline in the
        # value would break parsing. Defensive scrub.
        result = {"score": 0, "max-score": 0, "tests": []}
        result_path = tmp_path / "result.json"
        body_path = tmp_path / "release-body.md"
        gh_output = tmp_path / "github-output"
        gh_output.write_text("")

        ag.write_outputs(
            result=result,
            status="error",
            summary="line one\nline two\rline three",
            result_path=result_path,
            release_body_path=body_path,
            github_output_path=str(gh_output),
        )

        gh = gh_output.read_text()
        assert "summary=line one line two line three\n" in gh
        # Only two lines (status + summary), no stray newlines.
        assert gh.count("\n") == 2

    def test_no_github_output_does_not_crash(self, tmp_path):
        # Running locally for development — GITHUB_OUTPUT may not be set.
        result = {"score": 0, "max-score": 0, "tests": []}
        ag.write_outputs(
            result=result,
            status="success",
            summary="ok",
            result_path=tmp_path / "result.json",
            release_body_path=tmp_path / "release-body.md",
            github_output_path=None,
        )
        assert (tmp_path / "result.json").is_file()


# ---------------------------------------------------------------------------
# write_managed_conftest
# ---------------------------------------------------------------------------


class TestWriteManagedConftest:
    def test_writes_marker_registration(self, tmp_path):
        dest = tmp_path / "subdir" / "conftest.py"
        ag.write_managed_conftest(dest)
        content = dest.read_text()
        assert "pytest_configure" in content
        assert "score(value: int)" in content
        assert "classroom50_score" in content

    def test_creates_parent_directories(self, tmp_path):
        dest = tmp_path / "deep" / "nested" / "conftest.py"
        ag.write_managed_conftest(dest)
        assert dest.is_file()


# ---------------------------------------------------------------------------
# Schema sentinel pinning
# ---------------------------------------------------------------------------


def test_result_schema_sentinel_matches_collect_scores():
    # Cross-binary contract — must match RESULT_SCHEMA_V1 in
    # cli/gh-teacher/skeleton/dotgithub/scripts/collect_scores.py
    # and the Go-side consts.
    assert ag.RESULT_SCHEMA_V1 == "classroom50/result/v1"
