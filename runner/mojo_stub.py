"""A `mojo` that cannot compile, only ask — and then wait for the answer.

The sandbox has no compiler. `MOJOSUB_MOJO` points here instead, so when
mojosub decides a kernel is worth building, the build turns into a request: the
generated Mojo source is dropped in the run's request directory, where the host
picks it up (`hostcompile.watch`), compiles it as the service user, and drops
the shared library back beside the request.

This process then waits for that library and hands it to mojosub as if it had
built it. The wait happens on mojosub's own background compile thread, so the
user's program keeps running interpreted throughout — and the moment the
library lands, the *same run* starts dispatching natively. Before this, a
kernel's first run was interpreted end to end and only the next one was fast,
which is the wrong answer for somebody who submits a program once.

Two things are unchanged, and both matter:

- No compiler, linker, or writable shared cache inside the jail. A tenant
  cannot plant a shared library that another tenant's run would load.
- Every entry in the shared cache was produced by *our* compiler from the exact
  source its content hash names, which is what makes sharing safe at all.

If the wait runs out, or the source does not build, this fails exactly as it
used to and the run stays on CPython.

argv mirrors the real thing: `--version`, and
`build --emit shared-lib --optimization-level=N src.mojo -o out.so`.
"""

from __future__ import annotations

import os
import shutil
import sys
import time
import uuid

# How long to wait for the host to build. Zero restores the old fire-and-forget
# behaviour. The cap that actually matters is the run's own deadline: this
# thread is a daemon and dies with the program.
WAIT_MS = float(os.environ.get("MOJOJOJO_COMPILE_WAIT_MS", "8000"))
POLL_S = 0.01


def _await_build(requests: str, name: str, out_path: str, deadline: float) -> bool:
    """Wait for `<name>.so` (or `<name>.err`) to appear beside the request."""
    lib = os.path.join(requests, name + ".so")
    err = os.path.join(requests, name + ".err")
    # Written by bootstrap when the user's code returns. Past that point a
    # library this run could still load does not exist, and waiting for one
    # only makes the caller sit through a compile they are not being charged
    # for and cannot use — the host finishes it either way.
    stop = os.path.join(requests, "stop")
    while time.time() < deadline:
        if os.path.exists(lib):
            try:
                # Copied, not linked: mojosub renames the output into its cache
                # and the request directory is cleaned up with the run.
                shutil.copyfile(lib, out_path)
                return True
            except OSError:
                return False
        if os.path.exists(err) or os.path.exists(stop):
            return False
        time.sleep(POLL_S)
    return False


def main() -> int:
    argv = sys.argv[1:]
    if "--version" in argv:
        # mojosub hashes this into the cache key, so it has to be the version
        # of the compiler that will actually build the request.
        print(os.environ.get("MOJOJOJO_MOJO_VERSION", "mojojojo-host-compiler"))
        return 0

    if not argv or argv[0] != "build":
        print(f"mojojojo: unsupported mojo invocation: {' '.join(argv)}", file=sys.stderr)
        return 2

    source = next((a for a in argv[1:] if a.endswith(".mojo")), None)
    out_path = argv[argv.index("-o") + 1] if "-o" in argv[:-1] else ""
    requests = os.environ.get("MOJOJOJO_COMPILE_REQUESTS", "")
    name = uuid.uuid4().hex
    queued = False
    if source and requests and os.path.isdir(requests):
        try:
            # Written under a temporary name and renamed, so the watcher never
            # sees a half-written request and compiles a truncated unit.
            partial = os.path.join(requests, name + ".mojo.part")
            shutil.copyfile(source, partial)
            os.replace(partial, os.path.join(requests, name + ".mojo"))
            queued = True
        except OSError as exc:
            print(f"mojojojo: could not queue compile: {exc}", file=sys.stderr)

    if queued and out_path and WAIT_MS > 0:
        if _await_build(requests, name, out_path, time.time() + WAIT_MS / 1000.0):
            return 0

    # Non-zero on purpose: there is no library to load in this process. The
    # message is what surfaces to the caller as the reason a kernel is not yet
    # native, so it says what will happen rather than looking like a failure.
    print("mojojojo: queued for compilation on the host; "
          "this kernel runs natively from the next execution onward",
          file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
