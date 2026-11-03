# Foundry Numeric Baseline

Open `/studio?example=foundry-numeric&kind=tool`, or publish with the installed SDK:

```sh
mojojojo project "Foundry Numeric Baseline" --runtime app --category tool
mojojojo publish PROJECT_ID examples/foundry-numeric
```

`POST https://mojojojo.cc/p/PROJECT_ID/api/model` with an API key, JSON body and
`operation: train` returns a downloadable JSON model artifact. See `index.html`
for the complete request. `operation: infer` accepts the artifact's `model`
field and numeric rows. The authenticated caller pays through existing billing;
the owner pays for Studio previews.

This stdlib centroid baseline demonstrates the paid function/artifact contract
without installing or downloading anything. It does not replace the Foundry's
PyTorch references or trained sklearn ensembles, and it is not zero-shot.
There is no asynchronous job persistence, artifact registry or held-out quality
claim. Caller-provided license metadata is a declaration, not legal verification.
Do not use it for medical, credit, hiring, robotics or control decisions.
