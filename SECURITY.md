# Secret scanning

Install Gitleaks and run `./scripts/install-gitleaks-hook.sh`. CI and the
pre-push hook scan with `.gitleaks.toml`. New Mojojojo keys use `sk-mj-`. Never commit `.env` files
or live credentials; revoke any credential found in Git.

