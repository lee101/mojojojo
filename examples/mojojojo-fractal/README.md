# mojojojo fractal

`index.html` and `app.js` are a static, interactive Mandelbrot explorer. The
`src/` directory is a genuinely multi-file Mojo program demonstrating the same
palette math through `POST /v1/run`.

```bash
zip -r fractal.zip mojojojo.json index.html app.js src
```

Run the Mojo project with the API (paths map directly to project files):

```json
{
  "language": "mojo",
  "entrypoint": "main.mojo",
  "files": {
    "main.mojo": "from palette import shade\n...",
    "palette.mojo": "def shade(iterations: Int) -> Int: ..."
  }
}
```

The compiler runs outside the sandbox and is not billed. Only the resulting
binary enters the normal isolated runtime. License: MIT.
