const canvas = document.querySelector('#fractal');
const ctx = canvas.getContext('2d');
const label = document.querySelector('#where');
let view = { x: -0.65, y: 0, scale: 3.2 };
function render() {
  const image = ctx.createImageData(canvas.width, canvas.height);
  for (let py = 0; py < canvas.height; py++) for (let px = 0; px < canvas.width; px++) {
    const cx = view.x + (px / canvas.width - .5) * view.scale;
    const cy = view.y + (py / canvas.height - .5) * view.scale * canvas.height / canvas.width;
    let x = 0, y = 0, n = 0;
    while (x*x + y*y <= 4 && n < 160) { const next = x*x - y*y + cx; y = 2*x*y + cy; x = next; n++; }
    const i = (py * canvas.width + px) * 4, t = n / 160;
    image.data[i] = n === 160 ? 14 : 45 + 210*t;
    image.data[i+1] = n === 160 ? 13 : 70 + 170*(1-t);
    image.data[i+2] = n === 160 ? 21 : 150 + 100*Math.sin(t*6);
    image.data[i+3] = 255;
  }
  ctx.putImageData(image, 0, 0);
  label.textContent = `${view.x.toFixed(6)}, ${view.y.toFixed(6)} · ${view.scale.toExponential(2)}`;
}
canvas.addEventListener('click', event => {
  const box = canvas.getBoundingClientRect();
  view.x += (event.clientX - box.left) / box.width * view.scale - view.scale / 2;
  view.y += ((event.clientY - box.top) / box.height - .5) * view.scale * canvas.height / canvas.width;
  view.scale *= .42; render();
});
document.querySelector('#reset').addEventListener('click', () => { view = { x: -.65, y: 0, scale: 3.2 }; render(); });
render();
