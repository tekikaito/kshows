// Run with: node --test web/static/
import test from "node:test";
import assert from "node:assert/strict";
import { squarify, splitFree } from "./treemap.js";

const area = (r) => r.w * r.h;

test("squarify preserves total area and stays in bounds", () => {
  const items = [5, 3, 2, 2, 1, 1, 0.5].map((value, i) => ({ value, id: i }));
  const rects = squarify(items, 0, 0, 300, 200);
  assert.equal(rects.length, items.length);

  const total = rects.reduce((s, r) => s + area(r), 0);
  assert.ok(Math.abs(total - 300 * 200) < 1e-6, `area ${total} != 60000`);

  for (const r of rects) {
    assert.ok(r.x >= -1e-9 && r.y >= -1e-9);
    assert.ok(r.x + r.w <= 300 + 1e-9);
    assert.ok(r.y + r.h <= 200 + 1e-9);
  }
});

test("squarify areas are proportional to values", () => {
  const items = [{ value: 6, id: "a" }, { value: 3, id: "b" }, { value: 1, id: "c" }];
  const rects = squarify(items, 0, 0, 100, 100);
  const byId = Object.fromEntries(rects.map((r) => [r.item.id, area(r)]));
  assert.ok(Math.abs(byId.a - 6000) < 1e-6);
  assert.ok(Math.abs(byId.b - 3000) < 1e-6);
  assert.ok(Math.abs(byId.c - 1000) < 1e-6);
});

test("squarify keeps aspect ratios sane for equal values", () => {
  const items = Array.from({ length: 8 }, (_, i) => ({ value: 1, id: i }));
  for (const r of squarify(items, 0, 0, 400, 300)) {
    const ratio = Math.max(r.w / r.h, r.h / r.w);
    assert.ok(ratio < 2.5, `tile ${r.item.id} aspect ${ratio}`);
  }
});

test("squarify handles empty and zero inputs", () => {
  assert.deepEqual(squarify([], 0, 0, 100, 100), []);
  assert.deepEqual(squarify([{ value: 0 }], 0, 0, 100, 100), []);
});

test("splitFree divides along the longer side with exact areas", () => {
  const { packed, free } = splitFree(0, 0, 300, 200, 0.25);
  assert.ok(packed && free);
  assert.ok(Math.abs(area(free) - 300 * 200 * 0.25) < 1e-6);
  assert.equal(free.h, 200); // wide rect → vertical slice, free on the right
  assert.ok(Math.abs(packed.w + free.w - 300) < 1e-9);
});

test("splitFree collapses at the extremes", () => {
  assert.equal(splitFree(0, 0, 100, 100, 0).free, null);
  assert.equal(splitFree(0, 0, 100, 100, 1).packed, null);
});
