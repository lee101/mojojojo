# JSON to CSV

A complete mojojojo app: ordinary HTML plus one Python function. Open this
directory in `/studio`, or publish it from the CLI:

```sh
mojojojo project "JSON to CSV" --runtime app --category tool
mojojojo publish PROJECT_ID examples/json-to-csv
mojojojo submit PROJECT_ID
```

Python functions export `handler(request)`. The request contains `method`,
`path`, `query`, `headers`, and `body`; return a value or an envelope with
`status`, `headers`, `body`, and optional `is_base64`.
