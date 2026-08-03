import test from "node:test";
import assert from "node:assert/strict";
import { decodeShare, encodeShare, shareURL } from "../static/share.js";

test("share payloads round-trip unicode code and settings", async () => {
  const input = {
    name: "π.py",
    code: "print('hello λ')\n",
    where: "browser",
    gpu: false,
    timeout: 12,
  };
  const encoded = await encodeShare(input);
  const decoded = await decodeShare(`#${encoded}`);
  assert.deepEqual(decoded, { v: 1, ...input });
  assert.ok(encoded.length < 180);
});

test("share URLs replace an existing fragment", async () => {
  const url = await shareURL({ name: "a.py", code: "print(1)" }, "https://example.test/editor#old");
  assert.match(url, /^https:\/\/example\.test\/editor#[zr]=/);
  assert.equal((await decodeShare(new URL(url).hash)).code, "print(1)");
});

test("invalid or unrelated fragments are ignored", async () => {
  assert.equal(await decodeShare("#pricing"), null);
  assert.equal(await decodeShare("#z=broken"), null);
});
