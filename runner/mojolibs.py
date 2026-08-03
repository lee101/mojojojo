"""Wire the Mojo library ports into a run.

Sitting next to this repo are ~60 ports of common Python libraries to Mojo
(`../mojo-pillow`, `../mojo-opencv-python`, `../mojo-sklearn`, …). Each is a
Python package whose compute is a compiled Mojo shared library loaded through
ctypes. mojojojo already compiles a caller's *own* loops; these cover the case
where the hot code is inside a library instead.

Two rules shape everything here:

1. **Nothing is substituted silently.** A port covers a subset of its
   original's API, so aliasing `PIL` to `mojopillow` behind a caller's back
   turns a speedup into a mystery AttributeError. Ports are always importable
   under their own name (`import mojopillow`), and aliasing only happens when
   the request asks for it with `mojo_libs: true`.
2. **The sandbox never compiles.** A port whose shared library is not built
   yet is simply not offered to the run; the host builds it afterwards, the
   same way queued kernels work, and the next run gets it.

Imports we have no port for are appended to a wishlist, which is the input the
port factory (`../mojo-factory`) reads to decide what to port next.
"""

from __future__ import annotations

import json
import os
import subprocess
import threading
import time
from dataclasses import dataclass

CODE_ROOT = os.environ.get("MOJOJOJO_CODE_ROOT", "/nvme0n1-disk/code")
DATA_ROOT = os.environ.get("MOJOJOJO_DATA", "/nvme0n1-disk/data/mojojojo")
WISHLIST = os.path.join(DATA_ROOT, "mojo-wishlist.jsonl")
BUILD_TIMEOUT = float(os.environ.get("MOJOJOJO_PORT_BUILD_TIMEOUT", "600"))
ENABLED = os.environ.get("MOJOJOJO_MOJO_LIBS", "1") != "0"


@dataclass(frozen=True)
class Port:
    """One ported library.

    `imports` are the module names a caller would write in ordinary Python;
    `package` is what the port calls itself. `drop_in` says whether aliasing
    the original to the port is safe enough to offer at all — a port covering
    three functions of pandas is useful to import directly and dangerous to
    alias.
    """

    repo: str
    package: str
    imports: tuple[str, ...]
    drop_in: bool = False

    @property
    def path(self) -> str:
        return os.path.join(CODE_ROOT, self.repo)

    @property
    def python_dir(self) -> str:
        return os.path.join(self.path, "python")

    @property
    def lib_dir(self) -> str:
        return os.path.join(self.path, "dist")


# Curated rather than discovered: a repo directory name does not tell you which
# import it stands in for (mojo-opencv-python is `cv2`), and guessing wrong
# means putting the wrong library on someone's path.
PORTS: tuple[Port, ...] = (
    Port("mojo-pillow", "mojopillow", ("PIL", "pillow"), drop_in=True),
    Port("mojo-opencv-python", "mojo_opencv", ("cv2",)),
    Port("mojo-sklearn", "mojosklearn", ("sklearn", "scikit-learn")),
    Port("mojo-numpy", "mojonumpy", ("numpy",)),
    Port("mojo-pandas", "mojopandas", ("pandas",)),
    Port("mojo-scipy", "mojo_scipy", ("scipy",)),
    Port("mojo-arrow", "mojoarrow", ("pyarrow",)),
    Port("mojo-plotly", "mojoplotly", ("plotly",)),
    Port("mojo-statsmodels", "mojostatsmodels", ("statsmodels",)),
    Port("mojo-networkx", "mojonetworkx", ("networkx",)),
    Port("mojo-scikit-image", "mojoskimage", ("skimage",)),
    Port("mojo-regex", "mojoregex", ("regex",), drop_in=True),
    Port("mojo-orjson", "mojo_orjson", ("orjson",), drop_in=True),
    Port("mojo-ujson", "mojoujson", ("ujson",), drop_in=True),
    Port("mojo-msgpack", "mojomsgpack", ("msgpack",), drop_in=True),
    Port("mojo-xxhash", "mojoxxhash", ("xxhash",), drop_in=True),
    Port("mojo-mmh3", "mojommh3", ("mmh3",), drop_in=True),
    Port("mojo-blake3", "mojoblake3", ("blake3",), drop_in=True),
    Port("mojo-cityhash", "mojocityhash", ("cityhash",), drop_in=True),
    Port("mojo-lz4", "mojolz4", ("lz4",), drop_in=True),
    Port("mojo-zstandard", "mojozstandard", ("zstandard",), drop_in=True),
    Port("mojo-snappy", "mojosnappy", ("snappy",), drop_in=True),
    Port("mojo-brotli", "mojobrotli", ("brotli",), drop_in=True),
    Port("mojo-tiktoken", "mojotiktoken", ("tiktoken",)),
    Port("mojo-tokenizers", "mojotokenizers", ("tokenizers",)),
    Port("mojo-sympy", "mojosympy", ("sympy",)),
    Port("mojo-librosa", "mojolibrosa", ("librosa",)),
    Port("mojo-nltk", "mojonltk", ("nltk",)),
    Port("mojo-gensim", "mojogensim", ("gensim",)),
    Port("mojo-annoy", "mojoannoy", ("annoy",)),
    Port("mojo-hnswlib", "mojohnswlib", ("hnswlib",)),
    Port("mojo-faiss-cpu", "mojofaiss", ("faiss",)),
    Port("mojo-lightgbm", "mojolightgbm", ("lightgbm",)),
    Port("mojo-xgboost", "mojoxgboost", ("xgboost",)),
    Port("mojo-shapely", "mojoshapely", ("shapely",)),
    Port("mojo-pyroaring", "mojopyroaring", ("pyroaring",), drop_in=True),
    Port("mojo-sortedcontainers", "mojosortedcontainers", ("sortedcontainers",), drop_in=True),
    Port("mojo-bitarray", "mojobitarray", ("bitarray",), drop_in=True),
    Port("mojo-intervaltree", "mojointervaltree", ("intervaltree",), drop_in=True),
    Port("mojo-datasketch", "mojodatasketch", ("datasketch",)),
    Port("mojo-hyperloglog", "mojohyperloglog", ("hyperloglog",), drop_in=True),
    Port("mojo-pybloom-live", "mojopybloom", ("pybloom_live",), drop_in=True),
    Port("mojo-cbor2", "mojocbor2", ("cbor2",), drop_in=True),
    Port("mojo-protobuf", "mojoprotobuf", ("google.protobuf",)),
    Port("mojo-pycryptodome", "mojopycryptodome", ("Crypto",)),
    Port("mojo-cryptography", "mojocryptography", ("cryptography",)),
    Port("mojo-argon2-cffi", "mojoargon2", ("argon2",)),
    Port("mojo-bcrypt", "mojobcrypt", ("bcrypt",), drop_in=True),
    Port("mojo-soundfile", "mojosoundfile", ("soundfile",)),
    Port("mojo-rasterio", "mojorasterio", ("rasterio",)),
    Port("mojo-pyproj", "mojopyproj", ("pyproj",)),
    Port("mojo-spacy", "mojospacy", ("spacy",)),
    Port("mojo-numba", "mojonumba", ("numba",)),
    Port("mojo-db", "mojodb", ("sqlite3",)),
    Port("mojo-embed", "mojoembed", ("mojoembed",)),

    # --- small, shareable application primitives -------------------------
    # These power the deployable examples in examples/. Each example imports
    # the namespaced port first and falls back to the upstream package while a
    # cold worker builds it, so the first request remains useful too.
    Port("mojo-qrcode", "mojo_qrcode", ("mojo_qrcode", "qrcode"), drop_in=True),
    Port("mojo-croniter", "mojo_croniter", ("mojo_croniter", "croniter"), drop_in=True),
    Port("mojo-phonenumbers", "mojo_phonenumbers",
         ("mojo_phonenumbers", "phonenumbers"), drop_in=True),
    Port("mojo-imagehash", "mojo_imagehash",
         ("mojo_imagehash", "imagehash"), drop_in=True),

    # --- reinforcement learning ------------------------------------------
    # Batched classic-control environments. Registered under `mojo_rl` only: it
    # is not a port *of* gymnasium, it is a faster set of the same environments
    # with a Gymnasium-shaped API, and aliasing `gymnasium` to it would hand a
    # caller three environments where they expected a hundred.
    Port("mojo-rl", "mojo_rl", ("mojo_rl",)),

    # --- machine learning and statistics ---------------------------------
    # All present on disk from the port factory and none a safe drop-in: each
    # covers a subset of a large API, so they are importable under their own
    # name and never aliased behind a caller's back.
    Port("mojo-shap", "mojo_shap", ("shap",)),
    Port("mojo-optuna", "mojo_optuna", ("optuna",)),
    Port("mojo-hyperopt", "mojohyperopt", ("hyperopt",)),
    Port("mojo-scikit-optimize", "mojo_skopt", ("skopt",)),
    Port("mojo-nevergrad", "mojonevergrad", ("nevergrad",)),
    Port("mojo-deap", "mojodeap", ("deap",)),
    Port("mojo-umap-learn", "mojo_umap", ("umap",)),
    Port("mojo-opentsne", "mojo_opentsne", ("openTSNE",)),
    Port("mojo-hdbscan", "mojo_hdbscan", ("hdbscan",)),
    Port("mojo-kmodes", "mojo_kmodes", ("kmodes",)),
    Port("mojo-pyod", "mojopyod", ("pyod",)),
    Port("mojo-lime", "mojolime", ("lime",)),
    Port("mojo-river", "mojoriver", ("river",)),
    Port("mojo-vowpalwabbit", "mojo_vowpalwabbit", ("vowpalwabbit",)),
    Port("mojo-category-encoders", "mojo_category_encoders", ("category_encoders",)),
    Port("mojo-imbalanced-learn", "mojo_imbalanced_learn", ("imblearn",)),
    Port("mojo-featuretools", "mojo_featuretools", ("featuretools",)),
    Port("mojo-implicit", "mojo_implicit", ("implicit",)),
    Port("mojo-lightfm", "mojo_lightfm", ("lightfm",)),
    Port("mojo-hmmlearn", "mojo_hmmlearn", ("hmmlearn",)),
    Port("mojo-pomegranate", "mojopomegranate", ("pomegranate",)),
    Port("mojo-pymc", "mojopymc", ("pymc",)),
    Port("mojo-arviz", "mojoarviz", ("arviz",)),
    Port("mojo-emcee", "mojo_emcee", ("emcee",)),
    Port("mojo-scikit-fuzzy", "mojofuzzy", ("skfuzzy",)),

    # --- time series and quant -------------------------------------------
    Port("mojo-tslearn", "mojotslearn", ("tslearn",)),
    Port("mojo-statsforecast", "mojostatsforecast", ("statsforecast",)),
    Port("mojo-prophet", "mojo_prophet", ("prophet",)),
    Port("mojo-stumpy", "mojostumpy", ("stumpy",)),
    Port("mojo-ruptures", "mojoruptures", ("ruptures",)),
    Port("mojo-filterpy", "mojofilterpy", ("filterpy",)),
    Port("mojo-pykalman", "mojo_pykalman", ("pykalman",)),
    Port("mojo-dtaidistance", "mojo_dtaidistance", ("dtaidistance",)),
    Port("mojo-empyrical", "mojo_empyrical", ("empyrical",)),
    Port("mojo-pyfolio", "mojopyfolio", ("pyfolio",)),
    Port("mojo-vectorbt", "mojo_vectorbt", ("vectorbt",)),
    Port("mojo-zipline", "mojo_zipline", ("zipline",)),

    # --- array, dataframe and numeric kernels ----------------------------
    Port("mojo-polars", "mojopolars", ("polars",)),
    Port("mojo-duckdb", "mojo_duckdb", ("duckdb",)),
    Port("mojo-dask", "mojodask", ("dask",)),
    Port("mojo-numexpr", "mojo_numexpr", ("numexpr",)),
    Port("mojo-bottleneck", "mojo_bottleneck", ("bottleneck",)),
    Port("mojo-numagg", "mojonumagg", ("numagg",)),
    Port("mojo-zarr", "mojo_zarr", ("zarr",)),
    Port("mojo-h5py", "mojo_h5py", ("h5py",)),
    Port("mojo-fastparquet", "mojo_fastparquet", ("fastparquet",)),

    # --- optimization and search -----------------------------------------
    Port("mojo-cvxpy", "mojocvxpy", ("cvxpy",)),
    Port("mojo-pulp", "mojo_pulp", ("pulp",)),
    Port("mojo-ortools", "mojo_ortools", ("ortools",)),
    Port("mojo-whoosh", "mojo_whoosh", ("whoosh",)),

    # Deliberately absent: mojo-surprise and mojo-rank-bm25 name their Python
    # package exactly `surprise` and `rank_bm25` — the same as the libraries they
    # stand in for. Putting those on a run's PYTHONPATH would shadow the real
    # library with a partial port without anybody asking, which is the one thing
    # this module promises not to do. `_shadowing` below refuses them if they are
    # ever added; fixing them means renaming the package in the port.
)

def _shadowing(port: Port) -> bool:
    """Whether putting this port on a run's PYTHONPATH could shadow a real library.

    A port is always importable under its own name, and aliasing only happens
    when the caller asks for it. Both promises break if a port's package is a
    name some real library already owns: adding its directory to PYTHONPATH
    silently replaces that library with a partial port, no alias was requested,
    and nothing in the trace points at us — the caller just gets a mystery
    AttributeError from a module they thought they knew.

    The property that makes that impossible is namespacing, so that is what is
    checked: a port's package lives under `mojo`. It is a cheap invariant with a
    real consequence, and it matters most for ports the factory generates without
    anyone reading them.
    """
    return not port.package.startswith("mojo")


# Ports the registry refuses, with the reason, so a gap is visible rather than
# merely absent. Read by the tests and reported in the runner's stats.
REJECTED: tuple[tuple[str, str], ...] = tuple(
    (p.repo, f"package {p.package!r} is not namespaced under `mojo`, so it "
             f"would shadow whatever real library owns that name")
    for p in PORTS if _shadowing(p)
)

BY_IMPORT: dict[str, Port] = {}
for _port in PORTS:
    if _shadowing(_port):
        continue
    for _name in _port.imports:
        BY_IMPORT.setdefault(_name, _port)

_build_lock = threading.Lock()
_building: set[str] = set()
_stats = {"used": 0, "built": 0, "build_failures": 0, "wishlisted": 0}


def stats() -> dict:
    with _build_lock:
        out = dict(_stats)
    out["ports"] = len(BY_IMPORT)
    out["ready"] = sum(1 for p in PORTS if is_built(p))
    if REJECTED:
        out["rejected"] = [repo for repo, _ in REJECTED]
    return out


def is_built(port: Port) -> bool:
    """True when the port's shared library exists and its Python is present.

    Checked per call rather than cached: a background build finishing is
    exactly the event that should change this answer.
    """
    if not os.path.isdir(port.python_dir):
        return False
    try:
        return any(name.endswith(".so") for name in os.listdir(port.lib_dir))
    except OSError:
        return False


def prepare(imports, enable_aliases: bool = False) -> dict:
    """Decide what a run with these imports gets.

    Returns paths to add to PYTHONPATH, an alias map (only when asked for),
    which ports were offered, which exist but are not built yet, and which
    imports have no port at all.
    """
    if not ENABLED:
        return {"paths": [], "aliases": {}, "used": [], "building": [], "missing": []}

    paths: list[str] = []
    aliases: dict[str, str] = {}
    used: list[str] = []
    building: list[str] = []
    missing: list[str] = []

    for name in imports:
        port = BY_IMPORT.get(name)
        if port is None:
            missing.append(name)
            continue
        if not is_built(port):
            building.append(port.repo)
            _build_async(port)
            continue
        if port.python_dir not in paths:
            paths.append(port.python_dir)
        used.append(port.package)
        if enable_aliases and port.drop_in:
            aliases[name] = port.package

    if missing:
        _wishlist(missing)
    with _build_lock:
        _stats["used"] += len(used)
    return {"paths": paths, "aliases": aliases, "used": used,
            "building": building, "missing": missing}


def _build_async(port: Port) -> None:
    """Build a port on the host, once, in the background.

    The run that discovered the gap does not wait — it just runs on CPython —
    for the same reason kernel compiles are deferred: seconds of compiler
    belong on our clock, and the benefit lands on the next caller.
    """
    with _build_lock:
        if port.repo in _building:
            return
        _building.add(port.repo)

    def worker() -> None:
        try:
            _build(port)
            with _build_lock:
                _stats["built"] += 1
        except Exception:  # noqa: BLE001 - a port that will not build is normal
            with _build_lock:
                _stats["build_failures"] += 1
        finally:
            with _build_lock:
                _building.discard(port.repo)

    threading.Thread(target=worker, daemon=True, name=f"build:{port.repo}").start()


def _build(port: Port) -> None:
    """Ask the port to build its own kernels, with our compiler."""
    import hostcompile  # local import: avoids a cycle at module load

    mojo = hostcompile.mojo_binary()
    if not mojo or not os.path.isdir(port.python_dir):
        raise RuntimeError(f"{port.repo}: no toolchain or no python/ directory")
    env = dict(os.environ)
    env["MODULAR_HOME"] = hostcompile.modular_home()
    env["PATH"] = os.path.dirname(mojo) + os.pathsep + env.get("PATH", "")
    # Every port reads <PACKAGE>_MOJO for an override; the names differ, so
    # both that and PATH are set and whichever the port looks at works.
    env[port.package.replace("_", "").upper() + "_MOJO"] = mojo
    env[port.package.upper() + "_MOJO"] = mojo
    subprocess.run(
        [os.sys.executable, "-c",
         f"import sys; sys.path.insert(0, {port.python_dir!r}); "
         f"import importlib; m = importlib.import_module('{port.package}._lib'); m.build()"],
        capture_output=True, text=True, timeout=BUILD_TIMEOUT, check=True, env=env)


def _wishlist(names) -> None:
    """Record imports nobody has ported yet.

    This file is the port factory's queue: what real users actually import,
    in the order they hit it, rather than what a library popularity list says.
    """
    try:
        os.makedirs(DATA_ROOT, exist_ok=True)
        with open(WISHLIST, "a", encoding="utf-8") as fh:
            for name in names:
                fh.write(json.dumps({"module": name, "at": round(time.time())}) + "\n")
        with _build_lock:
            _stats["wishlisted"] += len(names)
    except OSError:
        pass


def prebuild_all(limit: int = 0) -> dict:
    """Build every port that has not been built. Used by deploy, not by a run."""
    built, failed = [], []
    for i, port in enumerate(PORTS):
        if limit and i >= limit:
            break
        if is_built(port):
            continue
        try:
            _build(port)
            built.append(port.repo)
        except Exception as exc:  # noqa: BLE001
            failed.append((port.repo, str(exc)[:200]))
    return {"built": built, "failed": failed}
