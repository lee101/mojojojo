const file = document.querySelector('#file');
const run = document.querySelector('#run');
const status = document.querySelector('#status');
const canvas = document.querySelector('#canvas');
const context = canvas.getContext('2d', { willReadFrequently: true });
let session;
let bitmap;

ort.InferenceSession.create('./models/tiny-style.onnx', { executionProviders: ['wasm'] })
  .then(value => { session = value; run.disabled = false; status.textContent = 'Model ready.'; })
  .catch(error => { status.textContent = `Model failed: ${error.message}`; });

file.addEventListener('change', async () => {
  if (!file.files[0]) return;
  bitmap = await createImageBitmap(file.files[0]);
  const scale = Math.min(512 / bitmap.width, 512 / bitmap.height, 1);
  canvas.width = Math.max(1, Math.round(bitmap.width * scale));
  canvas.height = Math.max(1, Math.round(bitmap.height * scale));
  context.drawImage(bitmap, 0, 0, canvas.width, canvas.height);
});

run.addEventListener('click', async () => {
  if (!session || !bitmap) return;
  status.textContent = 'Running locally…';
  const pixels = context.getImageData(0, 0, canvas.width, canvas.height);
  const plane = canvas.width * canvas.height;
  const input = new Float32Array(plane * 3);
  for (let i = 0; i < plane; i++) {
    input[i] = pixels.data[i * 4] / 255;
    input[plane + i] = pixels.data[i * 4 + 1] / 255;
    input[plane * 2 + i] = pixels.data[i * 4 + 2] / 255;
  }
  const result = await session.run({ image: new ort.Tensor('float32', input, [1, 3, canvas.height, canvas.width]) });
  const output = result.styled.data;
  for (let i = 0; i < plane; i++) {
    pixels.data[i * 4] = output[i] * 255;
    pixels.data[i * 4 + 1] = output[plane + i] * 255;
    pixels.data[i * 4 + 2] = output[plane * 2 + i] * 255;
  }
  context.putImageData(pixels, 0, 0);
  status.textContent = 'Done. No image left this browser.';
});
