#!/bin/bash
# Run a benchmark with the same toolchain the runner uses.
#
# MODULAR_HOME has to match the resolved `mojo`, or the compiler starts and then
# fails with "unable to locate module 'std'" — an inherited one from an old
# standalone Modular install is worse than none.
set -euo pipefail
cd "$(dirname "$0")/.."

MOJOSUB=${MOJOJOJO_MOJOSUB:-/nvme0n1-disk/code/mojosub}
MOJO=${MOJOSUB_MOJO:-$MOJOSUB/.pixi/envs/default/bin/mojo}

export PYTHONPATH="$MOJOSUB:$PWD:$PWD/runner"
export MOJOSUB_MOJO="$MOJO"
export MODULAR_HOME="$(dirname "$(dirname "$MOJO")")/share/max"
# A scratch cache by default: a study that measures compile cost must not be
# reading the production cache's answers, and must not be writing to it either.
export MOJOSUB_CACHE=${MOJOSUB_CACHE:-/nvme0n1-disk/tmp/mojosub-bench-cache}

exec .venv/bin/python "$@"
