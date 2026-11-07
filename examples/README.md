# Deployable mojojojo examples

Small enough to read in one sitting, complete enough to publish. Each app has
a `mojojojo.json` manifest, a browser frontend, and—where useful—a metered
Python function that upgrades to an adjacent Mojo port when the worker is warm.

| Example | Why it is useful | Start |
| --- | --- | --- |
| [QR Forge](qr-forge) | Downloadable SVG QR codes via `mojo-qrcode` | [Deploy](https://mojojojo.cc/studio?example=qr-forge&kind=tool) |
| [Cron Lens](cron-lens) | Twelve real cron runs across time zones via `mojo-croniter` | [Deploy](https://mojojojo.cc/studio?example=cron-lens&kind=tool) |
| [Phone Passport](phone-passport) | E.164 cleanup and validation via `mojo-phonenumbers` | [Deploy](https://mojojojo.cc/studio?example=phone-passport&kind=tool) |
| [Photo Twin](photo-twin) | Perceptual duplicate detection via `mojo-imagehash` | [Deploy](https://mojojojo.cc/studio?example=photo-twin&kind=tool) |
| [Trendfolio](trendfolio) | Macrotrends-style price stats; kernel compiles via mojosub | [Deploy](https://mojojojo.cc/studio?example=trendfolio&kind=tool) |
| [Worldline](worldline) | OWID-style country explorer; growth/CAGR kernels compile via mojosub | [Deploy](https://mojojojo.cc/studio?example=worldline&kind=tool) |
| [Stat Surge](stat-surge) | CSV stats + SVG share card; histogram/percentile kernels compile via mojosub | [Deploy](https://mojojojo.cc/studio?example=stat-surge&kind=tool) |
| [JSON → CSV](json-to-csv) | Minimal Python serverless function | [Guide](json-to-csv/README.md) |
| [PNG → WebP](png-to-webp) | Private browser-first conversion | [Guide](png-to-webp/README.md) |
| [Mojo serverless](mojo-serverless) | Smallest compiled backend | [Guide](mojo-serverless/README.md) |
| [Fractal explorer](mojojojo-fractal) | Multi-file Mojo plus interactive Canvas | [Guide](mojojojo-fractal/README.md) |
| [Tiny style transfer](mojojojo-style-transfer) | Source-generated ONNX model in the browser | [Guide](mojojojo-style-transfer/README.md) |
| [Multi-Mojo libraries](multi-mojo-libs) | Vendored pure-Mojo dependency pattern | [Guide](multi-mojo-libs/README.md) |

The Deploy links fetch the files from these directories into a browser draft;
there is no copied template to drift. Sign-in is requested only when you save
or publish.

For the CLI workflow:

```sh
mojojojo project "QR Forge" --runtime app --category tool
mojojojo publish PROJECT_ID examples/qr-forge
mojojojo submit PROJECT_ID
```

All first-party examples are MIT licensed. Third-party models, libraries, and
ports retain their own licenses where noted.
