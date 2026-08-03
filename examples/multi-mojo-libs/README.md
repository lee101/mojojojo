# Pulling a pure-Mojo library into a multi-file project

The simplest reproducible dependency rule is vendoring: pin a commit, put the
`.mojo` module beside your entrypoint, and send both in `files`. There is no
package install inside the execution sandbox.

```bash
curl -L https://raw.githubusercontent.com/lee101/mojo-regex/master/src/regex.mojo \
  -o regex.mojo
```

`main.mojo` imports the pure-Mojo module normally. Submit `request.json` after
replacing the abbreviated `regex.mojo` value with the downloaded file:

```bash
curl -H "X-Api-Key: $MOJOJOJO_API_KEY" -H 'Content-Type: application/json' \
  --data @request.json https://mojojojo.cc/v1/run
```

For Python callers, the converted compatibility libraries remain explicitly
opt-in with `"mojo_libs": true`; direct imports such as `import mojoregex` are
preferred because partial ports are never silently substituted.

License: MIT. `mojo-regex` retains its own repository license.
