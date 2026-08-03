package scale

import (
	"fmt"
	"strings"
)

// BootstrapScript is what a rented machine runs to become a mojojojo worker.
//
// It pulls the worker from the control plane rather than baking an image,
// because the worker is ~40KB of Python and an image build is a release
// process nobody wants between them and a scale-up. The tarball contains no
// secrets — the shared token arrives through the environment, and the worker
// refuses to serve without it.
//
// Rented workers run the same sandbox with one difference, which is stated in
// the docs and not hidden: they have no `mojojail`, because a fresh cloud VM
// is already a single-tenant machine. The jail's job is keeping tenants apart
// on a shared box; on a machine rented for one overflow burst there is nothing
// to keep apart, and the VM boundary is stronger than the namespace one.
func BootstrapScript(controlPlane, token string, kind Kind) string {
	gpuBits := ""
	if kind == GPU {
		gpuBits = "export MOJOJOJO_GPU=1\n"
	}
	return fmt.Sprintf(`#!/bin/bash
set -euxo pipefail

# Everything below is idempotent: cloud-init retries, and so do we.
export DEBIAN_FRONTEND=noninteractive
command -v curl >/dev/null || (apt-get update -y && apt-get install -y curl ca-certificates)

install -d -m 0755 /opt/mojojojo
cd /opt/mojojojo
curl -fsSL --retry 5 --retry-delay 3 %s/worker.tar.gz -o worker.tar.gz
tar xzf worker.tar.gz

# uv brings its own Python, so the machine's distro version is irrelevant.
curl -LsSf https://astral.sh/uv/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
uv venv --python 3.13 /opt/mojojojo/.venv

id mojorun >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin mojorun
install -d -m 0733 -o root /var/lib/mojojojo/runs
install -d -m 0755 /var/lib/mojojojo/envs /var/lib/mojojojo/uv-cache

cat >/etc/systemd/system/mojojojo-runner.service <<'UNIT'
[Unit]
Description=mojojojo rented worker
After=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/mojojojo/runner
ExecStart=/opt/mojojojo/.venv/bin/python runner.py
Environment=MOJOJOJO_RUNNER_HOST=0.0.0.0
Environment=MOJOJOJO_RUNNER_PORT=4342
Environment=MOJOJOJO_DATA=/var/lib/mojojojo
Environment=UV_CACHE_DIR=/var/lib/mojojojo/uv-cache
Environment=MOJOJOJO_RUNNER_TOKEN=%s
%sRestart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now mojojojo-runner
`, strings.TrimRight(controlPlane, "/"), token, gpuBits)
}

// VectorBootstrapScript installs a provider-neutral retrieval worker. Both
// providers expose port 4342 on rented nodes, while the local worker uses 4344.
// The bundle carries the model, Mojo ABI library, and only its two small
// runtime dependencies, so a node does not need a Mojo toolchain.
func VectorBootstrapScript(controlPlane, token string, kind Kind) string {
	gpu := "0"
	if kind == GPU {
		gpu = "1"
	}
	return fmt.Sprintf(`#!/bin/bash
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive
command -v curl >/dev/null || (apt-get update -y && apt-get install -y curl ca-certificates)
install -d -m 0755 /opt/mojojojo-vector /var/lib/mojojojo-vector
cd /opt/mojojojo-vector
curl -fsSL --retry 5 --retry-delay 3 %s/vector-worker.tar.gz -o worker.tar.gz
tar xzf worker.tar.gz
curl -LsSf https://astral.sh/uv/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
uv venv --python 3.13 /opt/mojojojo-vector/.venv
uv pip install --python /opt/mojojojo-vector/.venv/bin/python numpy
if [ "%s" = "1" ]; then
  uv pip install --python /opt/mojojojo-vector/.venv/bin/python \
    cupy-cuda12x cuvs-cu12 --extra-index-url https://pypi.nvidia.com
fi
cat >/etc/systemd/system/mojojojo-vector.service <<'UNIT'
[Unit]
Description=mojojojo rented vector worker
After=network-online.target
[Service]
Type=simple
WorkingDirectory=/opt/mojojojo-vector
ExecStart=/opt/mojojojo-vector/.venv/bin/python -m vectorworker.server
Environment=VECTOR_LISTEN=0.0.0.0
Environment=VECTOR_PORT=4342
Environment=VECTOR_DATA_DIR=/var/lib/mojojojo-vector
Environment=VECTOR_MODEL_PATH=/opt/mojojojo-vector/model/modelint8_512dim.safetensors
Environment=VECTOR_TOKENIZER_PATH=/opt/mojojojo-vector/model/tokenizer.json
Environment=MOJO_EMBED_LIBRARY=/opt/mojojojo-vector/lib/libmojo_embed.so
Environment=MOJO_RUNTIME_DIR=/opt/mojojojo-vector/runtime
Environment=VECTOR_WORKER_TOKEN=%s
Environment=VECTOR_GPU_REQUESTED=%s
Restart=always
RestartSec=3
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now mojojojo-vector
`, strings.TrimRight(controlPlane, "/"), gpu, token, gpu)
}
