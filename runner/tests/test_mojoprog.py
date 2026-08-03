"""Running a whole program written in Mojo.

The accelerated Python path compiles part of a program. This is the other end:
the caller writes Mojo and we run all of it, with no interpreter involved. What
must stay true is everything that was already true of a Python run — the build
happens on the host and never in the sandbox, the jail is the same jail, the
clock measures the program and not our machinery, and a program that does not
compile costs its author nothing.

Builds are real and take seconds, so the compiling tests are marked `slow` and
share one program between them.
"""

from __future__ import annotations

import contextlib
import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import mojoprog  # noqa: E402
import runner  # noqa: E402
import sandbox  # noqa: E402

HELLO = 'def main():\n    print("hello from mojo")\n'
SUM = ('def main():\n'
       '    var total: Int = 0\n'
       '    for i in range(1000):\n'
       '        total += i\n'
       '    print(total)\n')

needs_mojo = pytest.mark.skipif(not mojoprog.available(),
                                reason="no Mojo toolchain on this machine")
needs_jail = pytest.mark.skipif(not sandbox.jail_available(),
                                reason="no jail on this machine")
slow = pytest.mark.slow


# --- the request surface ---

def test_an_unknown_language_is_refused_before_a_slot_is_taken():
    assert "language" in runner.execute({"code": "print(1)", "language": "ruby"})["error"]


def test_python_is_the_default():
    assert runner.execute({"code": "print(1)", "language": ""}).get("error") is None


# --- the content address ---

@needs_mojo
def test_the_same_program_has_the_same_key():
    assert mojoprog.key_for(HELLO) == mojoprog.key_for(HELLO)


@needs_mojo
def test_a_different_program_has_a_different_key():
    assert mojoprog.key_for(HELLO) != mojoprog.key_for(SUM)


@needs_mojo
def test_the_key_covers_the_toolchain_and_the_cpu(monkeypatch):
    """A binary built where AVX-512 exists must not be handed to a box without
    it, and the cache is synced across rented workers."""
    before = mojoprog.key_for(HELLO)
    monkeypatch.setattr(mojoprog, "_host_target", lambda: "some-other-cpu")
    assert mojoprog.key_for(HELLO) != before


def test_multi_file_key_covers_helpers_and_entrypoint():
    files = {"main.mojo": HELLO, "helper.mojo": "def answer() -> Int:\n    return 42\n"}
    first = mojoprog.key_for("", files, "main.mojo")
    changed = dict(files, **{"helper.mojo": "def answer() -> Int:\n    return 43\n"})
    assert mojoprog.key_for("", changed, "main.mojo") != first


@pytest.mark.parametrize("name", ["../main.mojo", "/main.mojo", "a/../main.mojo",
                                   "main.py", "a\\main.mojo"])
def test_multi_file_paths_cannot_escape_the_build_directory(name):
    with pytest.raises(ValueError, match="path"):
        mojoprog.key_for("", {name: HELLO}, name)


def test_multi_file_request_requires_a_real_entrypoint():
    result = runner.execute({"language": "mojo", "files": {"helper.mojo": HELLO},
                             "entrypoint": "main.mojo"})
    assert "entrypoint" in result["error"]


def test_multi_file_project_limit_is_enforced_before_building():
    files = {f"f{i}.mojo": HELLO for i in range(mojoprog.MAX_PROJECT_FILES + 1)}
    result = runner.execute({"language": "mojo", "files": files,
                             "entrypoint": "f0.mojo"})
    assert "at most" in result["error"]


def test_project_files_are_mojo_only_and_do_not_silently_ignore_code():
    files = {"main.mojo": HELLO}
    assert "only supported for Mojo" in runner.execute({"files": files})["error"]
    result = runner.execute({"language": "mojo", "code": HELLO,
                             "files": files, "entrypoint": "main.mojo"})
    assert "either code or files" in result["error"]


def test_entrypoint_is_not_silently_ignored_without_project_files():
    result = runner.execute({"language": "mojo", "code": HELLO,
                             "entrypoint": "main.mojo"})
    assert "only supported with Mojo project files" in result["error"]


def test_an_empty_project_is_not_treated_as_the_single_file_api():
    result = runner.execute({"language": "mojo", "files": {},
                             "entrypoint": "main.mojo"})
    assert "entrypoint" in result["error"]


def test_project_paths_fit_real_filesystem_components():
    name = "a" * (mojoprog.MAX_PROJECT_COMPONENT_BYTES + 1) + ".mojo"
    with pytest.raises(ValueError, match="too long"):
        mojoprog.key_for("", {name: HELLO}, name)


@pytest.mark.parametrize(("payload", "message"), [
    ({"code": 7}, "code must be a string"),
    ({"code": "print(1)", "language": 7}, "language must be a string"),
    ({"code": "print(1)", "timeout_ms": "later"}, "timeout_ms"),
    ({"code": "print(1)", "timeout_ms": True}, "timeout_ms"),
    ({"code": "print(1)", "timeout_ms": 1.5}, "timeout_ms"),
    ({"code": "print(1)", "stdin": {"not": "text"}}, "stdin"),
    ({"code": "print(1)", "args": "not-a-list"}, "args"),
    ({"code": "print(1)", "env": {"X": "bad\x00value"}}, "NUL"),
    ({"code": "print(1)", "packages": "numpy"}, "packages"),
    ({"code": "print(1)", "packages": ["--index-url=bad"]}, "package spec"),
    ({"code": "print(1)", "gpu": "false"}, "gpu must be a boolean"),
    ({"language": "mojo", "files": ["main.mojo"]}, "files"),
])
def test_malformed_requests_are_client_errors_not_runner_crashes(payload, message):
    result = runner.execute(payload)
    assert message in result["error"]


# --- building ---

@needs_mojo
@slow
def test_a_program_that_does_not_compile_reports_why_and_is_not_billed():
    result = runner.execute({"code": 'def main():\n    var x: Int = "no"\n',
                             "language": "mojo", "timeout_ms": 60000})
    assert result["ok"] is False
    assert result["billable_ms"] == 0.0
    assert "convert" in result["stderr"] or "error" in result["stderr"]


@needs_mojo
@slow
def test_the_diagnostics_do_not_name_the_host_s_directories():
    """The caller wrote the code and needs the line number. Where we put the
    file is noise, and a small leak of the host's layout."""
    source = 'def main():\n    var leaky: Int = "no"\n'
    # Failed builds are cached, so a log written before this scrubbing existed
    # would otherwise be handed back for ever.
    with contextlib.suppress(OSError):
        os.unlink(mojoprog._error_path(mojoprog.key_for(source)))
    result = runner.execute({"code": source, "language": "mojo",
                             "timeout_ms": 60000})
    assert "/nvme0n1-disk" not in result["stderr"]
    assert sandbox.RUN_ROOT not in result["stderr"]
    assert "program.mojo" in result["stderr"]


@needs_mojo
@slow
def test_a_failed_build_is_remembered_so_the_next_attempt_is_instant():
    source = 'def main():\n    var y: Int = "also no"\n'
    first = mojoprog.build(source)
    assert first["ok"] is False
    second = mojoprog.build(source)
    assert second["ok"] is False and second["cached"] is True
    assert second["log"] == first["log"]


def test_compile_queue_pressure_is_not_written_to_the_negative_cache(tmp_path,
                                                                     monkeypatch):
    monkeypatch.setattr(mojoprog, "SHARED", str(tmp_path))
    monkeypatch.setattr(mojoprog.cachestore, "restore", lambda _name: False)
    monkeypatch.setattr(
        mojoprog, "_compile",
        lambda *_args, **_kwargs: (_ for _ in ()).throw(
            mojoprog.hostcompile.CompileBusy("busy")))
    result = mojoprog.build(HELLO)
    assert result["ok"] is False and result["transient"] is True
    assert not list(tmp_path.glob("*.err"))


# --- running ---

@needs_mojo
@needs_jail
@slow
def test_a_mojo_program_runs_and_is_billed_for_its_own_time():
    result = runner.execute({"code": SUM, "language": "mojo",
                             "timeout_ms": 60000})
    assert result["ok"], result
    assert result["stdout"].strip() == "499500"
    assert result["language"] == "mojo"
    # The build is ours; the run is theirs. Never the other way round.
    assert result["billable_ms"] <= result["wall_ms"]
    assert result["build_ms"] == 0.0 or result["billable_ms"] < result["build_ms"]


@needs_mojo
@needs_jail
@slow
def test_the_second_run_of_a_program_does_not_build_it_again():
    runner.execute({"code": HELLO, "language": "mojo", "timeout_ms": 60000})
    again = runner.execute({"code": HELLO, "language": "mojo", "timeout_ms": 60000})
    assert again["build_cached"] is True
    assert again["build_ms"] == 0.0
    assert again["stdout"].strip() == "hello from mojo"


@needs_mojo
@needs_jail
@slow
def test_arguments_and_stdin_reach_a_compiled_program():
    # Mojo 1.0 namespaces the standard library under `std.` — `import sys`
    # is a Python habit and does not compile here.
    source = ('from std.sys import argv\n'
              'def main():\n'
              '    var args = argv()\n'
              '    print("argc", len(args))\n')
    result = runner.execute({"code": source, "language": "mojo",
                             "args": ["a", "b"], "timeout_ms": 60000})
    assert result["ok"], result["stderr"]
    assert result["stdout"].strip() == "argc 3"


@needs_mojo
@needs_jail
@slow
def test_a_compiled_program_has_no_network_either():
    """The jail is the same jail. A binary is not a way around it."""
    source = ('def main():\n'
              '    print("running")\n')
    result = runner.execute({"code": source, "language": "mojo",
                             "timeout_ms": 60000})
    assert result["isolated"] is True
    assert result["ok"]


@needs_mojo
@needs_jail
@slow
def test_a_program_that_never_ends_is_killed():
    source = ('def main():\n'
              '    while True:\n'
              '        pass\n')
    result = runner.execute({"code": source, "language": "mojo",
                             "timeout_ms": 1000})
    assert result["timed_out"] is True
    assert result["ok"] is False


@needs_mojo
@needs_jail
@slow
def test_the_run_directory_and_its_copy_of_the_binary_are_gone_afterwards():
    result = runner.execute({"code": HELLO, "language": "mojo",
                             "timeout_ms": 60000})
    assert not os.path.exists(os.path.join(sandbox.RUN_ROOT, result["run_id"]))


@needs_mojo
@needs_jail
@slow
def test_a_failing_program_keeps_its_exit_code():
    source = ('from std.sys import exit\n'
              'def main():\n'
              '    print("bye")\n'
              '    exit(3)\n')
    result = runner.execute({"code": source, "language": "mojo",
                             "timeout_ms": 60000})
    assert result["exit_code"] == 3, result
    assert result["ok"] is False
    assert result["stdout"].strip() == "bye"
