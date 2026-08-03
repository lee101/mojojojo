const encoder = new TextEncoder();
const decoder = new TextDecoder();

function toBase64URL(bytes) {
  let binary = "";
  for (let start = 0; start < bytes.length; start += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(start, start + 0x8000));
  }
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

function fromBase64URL(value) {
  const base64 = value.replaceAll("-", "+").replaceAll("_", "/") +
    "=".repeat((4 - value.length % 4) % 4);
  const binary = atob(base64);
  return Uint8Array.from(binary, (char) => char.charCodeAt(0));
}

async function transform(bytes, kind) {
  const Stream = kind === "compress" ? globalThis.CompressionStream : globalThis.DecompressionStream;
  if (!Stream) throw new Error(`${kind}ion streams unavailable`);
  const stream = new Blob([bytes]).stream().pipeThrough(new Stream("deflate"));
  return new Uint8Array(await new Response(stream).arrayBuffer());
}

export async function encodeShare(payload) {
  const raw = encoder.encode(JSON.stringify({ v: 1, ...payload }));
  try {
    return `z=${toBase64URL(await transform(raw, "compress"))}`;
  } catch {
    return `r=${toBase64URL(raw)}`;
  }
}

export async function decodeShare(hash = location.hash) {
  const match = String(hash).replace(/^#/, "").match(/^(z|r)=([A-Za-z0-9_-]+)$/);
  if (!match) return null;
  try {
    let bytes = fromBase64URL(match[2]);
    if (match[1] === "z") bytes = await transform(bytes, "decompress");
    const payload = JSON.parse(decoder.decode(bytes));
    if (payload?.v !== 1 || typeof payload.code !== "string") return null;
    return payload;
  } catch {
    return null;
  }
}

export async function shareURL(payload, base = location.href) {
  const url = new URL(base);
  url.hash = await encodeShare(payload);
  return url.toString();
}
