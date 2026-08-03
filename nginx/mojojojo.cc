# mojojojo.cc — the sole account, API, and billing origin.
#
# Cloudflare sits in front (proxied A record, SSL mode "Full"), so the
# certificate below is a self-signed origin cert — it secures CF-to-origin and
# is never seen by a browser. Same arrangement as codex-infinity.com.
#
# `mojojojo_backend` is declared once, in nginx/mojojojo.app.nz; deploy.sh
# installs both files together and nginx would reject a second declaration.

# Cloudflare terminates TLS and forwards over plain HTTP, so $scheme is "http"
# on every proxied request. Trust CF's X-Forwarded-Proto when it is present and
# fall back to $scheme for a request that reached the origin directly.
map $http_x_forwarded_proto $mojojojo_cc_proto {
    default $http_x_forwarded_proto;
    ""      $scheme;
}

server {
    listen 80;
    listen 443 ssl;
    server_name www.mojojojo.cc;

    ssl_certificate /etc/ssl/certs/mojojojo.cc.crt;
    ssl_certificate_key /etc/ssl/private/mojojojo.cc.key;

    # One canonical host keeps host-scoped sessions on one origin.
    return 301 https://mojojojo.cc$request_uri;
}

server {
    listen 80;
    listen 443 ssl;
    server_name mojojojo.cc;

    ssl_certificate /etc/ssl/certs/mojojojo.cc.crt;
    ssl_certificate_key /etc/ssl/private/mojojojo.cc.key;

    # A run may legitimately take five minutes; nothing here may cut it short
    # before the runner's own deadline does.
    proxy_connect_timeout 60s;
    proxy_send_timeout 900s;
    proxy_read_timeout 900s;
    send_timeout 900s;

    client_max_body_size 8M;

    gzip on;
    gzip_types text/plain text/css application/json application/javascript text/xml application/xml image/svg+xml;
    gzip_min_length 1000;

    add_header X-Content-Type-Options "nosniff" always;

    location / {
        proxy_pass http://mojojojo_backend;
        proxy_http_version 1.1;
        # Streaming responses are not buffered into a file first: the run API
        # returns one JSON body, but a slow run must not look like a stall.
        proxy_buffering off;

        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $mojojojo_cc_proto;
    }

    access_log /var/log/nginx/mojojojo.cc.access.log;
    error_log /var/log/nginx/mojojojo.cc.error.log;
}
