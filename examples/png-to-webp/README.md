# PNG to WebP

A zero-compute-cost hosted tool. Conversion happens through `createImageBitmap`
and Canvas in the visitor's browser. It is a useful pattern for converters:
keep private/local work local, and add a serverless function only where the
browser cannot do the job.
