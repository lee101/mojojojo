# Mojo serverless starter

Mojo functions read the request envelope as JSON on stdin and write either a
plain response or a JSON response envelope on stdout. This smallest example
does not need to parse the request; real functions can pull in sibling Mojo
modules using the same multi-file project layout as `../multi-mojo-libs`.
