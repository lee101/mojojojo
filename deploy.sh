#!/bin/bash
# Deploy mojojojo.cc. Idempotent: safe on every change.
set -e

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'
cd /nvme0n1-disk/code/mojojojo

DATA=/nvme0n1-disk/data/mojojojo

echo -e "${YELLOW}Building...${NC}"
go build -o mojojojo .
[ -x ../mojojojo-agent/.venv/bin/python ] || (cd ../mojojojo-agent && uv sync --extra server)
(cd ../mojojojo-agent && uv sync --extra server --frozen)
[ -x .venv/bin/python ] || uv venv --python 3.13 .venv
.venv/bin/python -c 'import numpy' 2>/dev/null || uv pip install --python .venv/bin/python numpy
if [ ! -f ../mojosub/.pixi/envs/default/lib/libKGENCompilerRTShared.so ] || \
   [ ! -f ../mojosub/.pixi/envs/default/lib/libAsyncRTMojoBindings.so ]; then
    echo -e "${YELLOW}Installing the Mojo vector runtime...${NC}"
    (cd ../mojosub && pixi install)
fi
# The local retrieval worker uses the host GPU when CUDA is present. Keep the
# optional packages out of CPU-only installs, and avoid touching the environment
# on normal deploys once the verified backend is already installed.
if command -v nvidia-smi >/dev/null 2>&1 && ! .venv/bin/python -c 'import cupy, cuvs' 2>/dev/null; then
    echo -e "${YELLOW}Installing the GPU vector backend...${NC}"
    UV_HTTP_TIMEOUT=300 uv pip install --python .venv/bin/python cupy-cuda12x cuvs-cu12 \
        --extra-index-url https://pypi.nvidia.com
fi
echo -e "${GREEN}Built${NC}"

mkdir -p "$DATA/runs" "$DATA/envs" "$DATA/uv-cache" "$DATA/python" "$DATA/vector"

# The web tier owns one PostgreSQL database. This deliberately creates no
# foreign tables, FDWs, or credentials for app.nz.
if ! psql -d postgres -Atqc "SELECT 1 FROM pg_database WHERE datname='mojojojo'" | grep -qx 1; then
    echo -e "${YELLOW}Creating mojojojo PostgreSQL database...${NC}"
    createdb mojojojo
fi
# The sandbox user writes its own scratch directory and the compiled-unit
# cache, and reads the venvs. The cache is owned by the sandbox user rather
# than made world-writable, so nothing outside the jail can plant a .so in it.
chmod 0755 "$DATA" "$DATA/envs" "$DATA/python"
# Writable and traversable by the sandbox user, but deliberately NOT listable:
# run directories are named with 32 random hex digits, which is what stops one
# sandboxed run from finding another's.
chmod 0733 "$DATA/runs"
mkdir -p "$DATA/pool" && chmod 0733 "$DATA/pool"

# --- the sandbox user and the one thing it is allowed to do ---
if ! id mojorun >/dev/null 2>&1; then
    echo -e "${YELLOW}Creating the mojorun sandbox user...${NC}"
    sudo useradd --system --no-create-home --shell /usr/sbin/nologin mojorun
fi

# The compile cache is written only by the service user (see
# runner/hostcompile.py); the sandbox reads it through symlinks and can never
# write it. If an older deploy left it owned by mojorun, take it back.
# Optional R2, Runpod, Hetzner and internal-agent credentials. The file belongs
# to mojojojo; deploy no longer copies identity or secrets out of app-site.
SECRETS="$DATA/secrets.env"
if [ ! -f "$SECRETS" ]; then
	install -m 0600 /dev/null "$SECRETS"
	echo -e "${YELLOW}Created $SECRETS; add provider/service credentials there when needed.${NC}"
fi
if ! grep -q '^MJJ_AGENT_TOKEN=' "$SECRETS"; then
	printf '\nMJJ_AGENT_TOKEN=%s\n' "$(openssl rand -hex 32)" >> "$SECRETS"
	chmod 0600 "$SECRETS"
	echo -e "${YELLOW}Created the internal agent service credential.${NC}"
fi
mkdir -p /sdb-disk/mojojojo/cache 2>/dev/null || true

mkdir -p "$DATA/mojosub-cache"
if [ "$(stat -c %U "$DATA/mojosub-cache")" != "$(id -un)" ]; then
    sudo chown -R "$(id -un)":"$(id -gn)" "$DATA/mojosub-cache"
fi
chmod 0755 "$DATA/mojosub-cache"

if ! cmp -s runner/mojojail /usr/local/bin/mojojail 2>/dev/null; then
    echo -e "${YELLOW}Installing /usr/local/bin/mojojail...${NC}"
    # Root-owned and outside the repo on purpose: a root-run script that the
    # service user can edit is a root shell.
    sudo install -o root -g root -m 0755 runner/mojojail /usr/local/bin/mojojail
fi

SUDOERS=/etc/sudoers.d/mojojojo
if [ ! -f "$SUDOERS" ]; then
    echo -e "${YELLOW}Installing the sudoers rule...${NC}"
    echo 'administrator ALL=(root) NOPASSWD: /usr/local/bin/mojojail' | \
        sudo tee "$SUDOERS" >/dev/null
    sudo chmod 0440 "$SUDOERS"
    sudo visudo -c -f "$SUDOERS"
fi

# --- nginx vhosts ---
# mojojojo.cc is the canonical site and mojojojo.app.nz is only a redirect. Both are
# installed here because without a matching server_name nginx falls through to
# whichever block declared `default_server` — which is how mojojojo.cc spent a
# while serving another site entirely.
NGINX_RELOAD=
for site in mojojojo.app.nz mojojojo.cc; do
    if ! cmp -s "nginx/$site" "/etc/nginx/sites-available/$site" 2>/dev/null; then
        echo -e "${YELLOW}Installing nginx/$site...${NC}"
        sudo cp "nginx/$site" "/etc/nginx/sites-available/$site"
        NGINX_RELOAD=1
    fi
    [ -L "/etc/nginx/sites-enabled/$site" ] || {
        sudo ln -sfn "/etc/nginx/sites-available/$site" "/etc/nginx/sites-enabled/$site"
        NGINX_RELOAD=1
    }
done
# Cloudflare fronts mojojojo.cc with SSL mode "Full", so this certificate is a
# self-signed origin cert: it secures CF-to-origin and no browser ever sees it.
if [ ! -f /etc/ssl/certs/mojojojo.cc.crt ]; then
    echo -e "${YELLOW}Issuing the mojojojo.cc origin certificate...${NC}"
    tmp=$(mktemp -d)
    openssl req -x509 -nodes -newkey rsa:2048 -days 3650 \
        -keyout "$tmp/k" -out "$tmp/c" -subj "/CN=mojojojo.cc" \
        -addext "subjectAltName=DNS:mojojojo.cc,DNS:www.mojojojo.cc" 2>/dev/null
    sudo install -o root -g root -m 0644 "$tmp/c" /etc/ssl/certs/mojojojo.cc.crt
    sudo install -o root -g root -m 0600 "$tmp/k" /etc/ssl/private/mojojojo.cc.key
    rm -rf "$tmp"
    NGINX_RELOAD=1
fi
if [ -n "$NGINX_RELOAD" ]; then
    # `nginx -t` in a pipeline hides its exit code behind the pager's, which is
    # how a broken config once got as far as `systemctl reload`.
    if sudo nginx -t 2>&1 | grep -qE "test is successful"; then
        sudo systemctl reload nginx
        echo -e "${GREEN}nginx reloaded${NC}"
    else
        echo -e "${RED}nginx config rejected — not reloading${NC}"
        sudo nginx -t 2>&1 | grep -v "protocol options redefined" | tail -5
        exit 1
    fi
fi

# --- units ---
for unit in mojojojo-runner mojojojo-vector mojojojo-agent mojojojo; do
    if ! cmp -s "systemd/$unit.service" "/etc/systemd/system/$unit.service" 2>/dev/null; then
        echo -e "${YELLOW}Installing $unit.service...${NC}"
        sudo cp "systemd/$unit.service" "/etc/systemd/system/$unit.service"
        NEED_RELOAD=1
    fi
done
if [ -n "$NEED_RELOAD" ]; then
    sudo systemctl daemon-reload
    sudo systemctl enable mojojojo-runner.service mojojojo-vector.service mojojojo-agent.service mojojojo.service
fi

echo -e "${YELLOW}Restarting services...${NC}"
sudo systemctl restart mojojojo-runner.service
sudo systemctl restart mojojojo-vector.service
sudo systemctl restart mojojojo.service
sudo systemctl restart mojojojo-agent.service

for i in $(seq 1 30); do
    if curl -sf http://127.0.0.1:4342/health >/dev/null 2>&1; then break; fi
    [ "$i" -eq 30 ] && { echo -e "${RED}runner unhealthy${NC}"
        journalctl -u mojojojo-runner.service --no-pager -n 30; exit 1; }
    sleep 1
done
echo -e "${GREEN}runner healthy${NC}"

for i in $(seq 1 60); do
    if curl -sf http://127.0.0.1:4344/health 2>/dev/null | grep -q '"gpu":true'; then break; fi
    [ "$i" -eq 60 ] && { echo -e "${RED}GPU vector worker unhealthy${NC}"
        journalctl -u mojojojo-vector.service --no-pager -n 30; exit 1; }
    sleep 1
done
set -a
. "$SECRETS"
set +a
VECTOR_AUTH=()
[ -n "${VECTOR_WORKER_TOKEN:-}" ] && VECTOR_AUTH=(-H "Authorization: Bearer $VECTOR_WORKER_TOKEN")
if ! curl -sf "${VECTOR_AUTH[@]}" -H 'Content-Type: application/json' \
    -d '{"vectors":10000,"iterations":3}' http://127.0.0.1:4344/benchmark | \
    grep -q '"backend":"cuvs-cagra-gpu"'; then
    echo -e "${RED}GPU vector search probe failed${NC}"
    exit 1
fi
echo -e "${GREEN}GPU vector search healthy${NC}"

for i in $(seq 1 30); do
    if curl -sf http://127.0.0.1:4343/health >/dev/null 2>&1; then break; fi
    [ "$i" -eq 30 ] && { echo -e "${RED}agent backend unhealthy${NC}"
        journalctl -u mojojojo-agent.service --no-pager -n 30; exit 1; }
    sleep 1
done
echo -e "${GREEN}agent backend healthy${NC}"

for i in $(seq 1 20); do
    if curl -sf http://127.0.0.1:4341/health >/dev/null 2>&1; then
        echo -e "${GREEN}Health OK — https://mojojojo.cc${NC}"
        curl -s http://127.0.0.1:4341/health | head -c 400; echo
        scripts/smoke-routes.sh https://mojojojo.cc
        exit 0
    fi
    [ "$i" -eq 20 ] && { echo -e "${RED}web tier unhealthy${NC}"
        journalctl -u mojojojo.service --no-pager -n 30; exit 1; }
    sleep 1
done
