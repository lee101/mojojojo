"""Work out what a program needs installed, from the program.

`import pandas` should be enough. Asking a caller to also list `pandas` in a
`packages` field is asking them to say the same thing twice and to be blamed
when the two disagree — so the imports are read out of the source and the
environment is built to match.

Rules, in order:

- Standard library imports are ignored (`sys.stdlib_module_names`).
- Anything already importable in the base environment is ignored, so `numpy`
  never triggers a rebuild.
- The rest are mapped to distributions. Most module names *are* the
  distribution name; the ones that are not are a short, well-known list, and
  guessing wrong there is what makes auto-install feel broken (`import cv2`
  pip-installing something called `cv2` that is not OpenCV).
- Relative imports and anything that does not look like a module name are
  dropped rather than passed to a package manager.

Environment builds are never billed, so a wrong guess costs us time, not the
caller money.
"""

from __future__ import annotations

import ast
import os
import re
import sys

import mojolibs

# Module name -> PyPI distribution, where they differ. Kept deliberately short:
# every entry is a claim about the world that can go stale.
KNOWN = {
    "cv2": "opencv-python-headless",
    "PIL": "pillow",
    "sklearn": "scikit-learn",
    "skimage": "scikit-image",
    "yaml": "pyyaml",
    "bs4": "beautifulsoup4",
    "dateutil": "python-dateutil",
    "dotenv": "python-dotenv",
    "serial": "pyserial",
    "usb": "pyusb",
    "OpenSSL": "pyopenssl",
    "Crypto": "pycryptodome",
    "jwt": "pyjwt",
    "google": "google-api-python-client",
    "docx": "python-docx",
    "pptx": "python-pptx",
    "fitz": "pymupdf",
    "psycopg2": "psycopg2-binary",
    "MySQLdb": "mysqlclient",
    "zmq": "pyzmq",
    "attr": "attrs",
    "pkg_resources": "setuptools",
    "lxml": "lxml",
    "IPython": "ipython",
    "torch": "torch",
    "tensorflow": "tensorflow",
    "PyQt5": "pyqt5",
    "wx": "wxpython",
    "gi": "pygobject",
    "magic": "python-magic",
    "snappy": "python-snappy",
    "ruamel": "ruamel.yaml",
    "importlib_metadata": "importlib-metadata",
    "typing_extensions": "typing-extensions",
    "PIL.Image": "pillow",
}

# Never install these even if imported: they are ours, or they are not on PyPI
# under that name, or they would be a security surprise.
BLOCKED = {"mojosub", "accel", "bootstrap", "sandbox", "runner", "envs",
           "hostcompile", "mojo_stub", "__main__"}
# Namespaced packages supplied from checked-out, host-built Mojo ports are not
# PyPI dependencies. Their upstream counterparts still are: code can try
# `mojo_qrcode` and fall back to `qrcode` on the cold request without uv first
# attempting to install an unrelated or nonexistent `mojo-qrcode` project.
LOCAL_PORTS = frozenset(port.package for port in mojolibs.PORTS)

MAX_AUTO = int(os.environ.get("MOJOJOJO_MAX_AUTO_PACKAGES", "12"))
_NAME = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


def top_level_imports(source: str) -> list[str]:
    """Module names a program imports, at any nesting depth."""
    try:
        tree = ast.parse(source)
    except SyntaxError:
        return []
    found: list[str] = []
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                found.append(alias.name.split(".")[0])
        elif isinstance(node, ast.ImportFrom):
            if node.level:
                continue  # relative import: part of the caller's own program
            if node.module:
                found.append(node.module.split(".")[0])
    seen: dict[str, None] = {}
    for name in found:
        if _NAME.match(name):
            seen.setdefault(name, None)
    return list(seen)


def _importable(name: str, base_python: str | None) -> bool:
    """Whether the base environment already has this module.

    Checked against *this* interpreter's stdlib plus the base venv's
    site-packages, without importing anything: importing to find out would run
    module-level code from a package the caller has not paid for yet.
    """
    if name in sys.stdlib_module_names:
        return True
    if not base_python:
        return False
    for root in _site_dirs(base_python):
        if os.path.exists(os.path.join(root, name)) or \
           os.path.exists(os.path.join(root, name + ".py")):
            return True
        # A compiled extension: numpy is fine, but so is _cffi_backend.so.
        if any(f.startswith(name + ".") and f.endswith(".so")
               for f in _listdir(root)):
            return True
    return False


_site_cache: dict[str, list[str]] = {}


def _site_dirs(base_python: str) -> list[str]:
    if base_python in _site_cache:
        return _site_cache[base_python]
    prefix = os.path.dirname(os.path.dirname(base_python))
    dirs = []
    lib = os.path.join(prefix, "lib")
    for name in _listdir(lib):
        candidate = os.path.join(lib, name, "site-packages")
        if os.path.isdir(candidate):
            dirs.append(candidate)
    _site_cache[base_python] = dirs
    return dirs


def _listdir(path: str) -> list[str]:
    try:
        return os.listdir(path)
    except OSError:
        return []


def detect(source: str, base_python: str | None = None) -> list[str]:
    """Distributions this program needs and the base environment lacks."""
    wanted: list[str] = []
    for module in top_level_imports(source):
        if module in BLOCKED or module in LOCAL_PORTS or _importable(module, base_python):
            continue
        wanted.append(KNOWN.get(module, module.replace("_", "-")))
        if len(wanted) >= MAX_AUTO:
            break
    return wanted
