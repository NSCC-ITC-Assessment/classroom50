"""Pytest suite for the `python`-type declarative test. Lives in the
template (and therefore in every student checkout) because the runner
executes the `run` command in the student's repo."""

from greet import greet


def test_greets_alice():
    assert greet("Alice") == "hello, Alice!"


def test_greets_anyone():
    assert greet("Margo") == "hello, Margo!"
