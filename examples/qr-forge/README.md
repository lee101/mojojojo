# QR Forge

Turn text, links, Wi-Fi credentials, or contact details into a crisp downloadable
SVG QR code. The backend tries the adjacent
[`mojo-qrcode`](https://github.com/lee101/mojo-qrcode) port first and falls back
to the compatible Python package while a cold worker builds the Mojo library.

[![Deploy to mojojojo](https://mojojojo.cc/static/deploy-button.svg)](https://mojojojo.cc/studio?example=qr-forge&kind=tool)

The app is intentionally small: ordinary HTML/CSS/JS, one Python function, and
no database. Deploy it from the CLI with:

```sh
mojojojo project "QR Forge" --runtime app --category tool
mojojojo publish PROJECT_ID .
mojojojo submit PROJECT_ID
```

The generated QR is SVG text, so it remains sharp at any size and no image
library is required. License: MIT.
