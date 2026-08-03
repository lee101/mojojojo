# Photo Twin

Drop two photos and get a perceptual-hash distance: zero means visually
identical, small numbers usually mean resized/recompressed versions, and large
numbers mean different images. The browser shrinks both files before upload;
the backend prefers [`mojo-imagehash`](https://github.com/lee101/mojo-imagehash)
and falls back to `ImageHash` while the Mojo library warms.

[![Deploy to mojojojo](https://mojojojo.cc/static/deploy-button.svg)](https://mojojojo.cc/studio?example=photo-twin&kind=tool)

Only two 128×128 JPEG previews cross the function boundary, keeping requests
small. The function does not persist them. This makes a useful seed for media
deduplication, moderation queues, and “same image?” checks. License: MIT.
