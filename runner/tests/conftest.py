"""Point the test process at the same Mojo toolchain the sandbox resolves.

Without this, `accel.enabled()` is false in-process and the acceleration tests
skip themselves on a machine that can in fact compile.
"""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import sandbox  # noqa: E402

if sandbox.mojo_binary():
    os.environ.setdefault("MOJOSUB_MOJO", sandbox.mojo_binary())
    os.environ["MODULAR_HOME"] = sandbox.modular_home()
    sys.path.insert(0, sandbox.MOJOSUB_PATH)
