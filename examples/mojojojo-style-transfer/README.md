# mojojojo style transfer

A deliberately tiny ONNX image transform. `build_model.py` creates the model
from six constants, so the example has no opaque downloaded weights or model
license. The browser runs it locally with ONNX Runtime Web; mojojojo object
storage only has to deliver the immutable 330-byte model and site files.

```bash
python build_model.py
zip -r style-transfer.zip mojojojo.json index.html app.js models
curl -H "X-Api-Key: $MOJOJOJO_API_KEY" \
  -F "project_id=$PROJECT_ID" -F "bundle=@style-transfer.zip" \
  https://mojojojo.cc/v1/deployments
```

The runtime is pinned to a public CDN in `index.html` to keep this repository
small. For a fully offline bundle, download that pinned JS/WASM release into
`vendor/` and update the script path before zipping.

License: MIT.
