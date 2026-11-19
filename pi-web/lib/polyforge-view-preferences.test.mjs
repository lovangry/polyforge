import "#test-env";
import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import { createJiti } from "jiti";

const jiti = createJiti(import.meta.url);
const prefs = await jiti.import("./polyforge-view-preferences.ts");
const { POLYFORGE_QUEUE_VIEW_KEY: QKEY, POLYFORGE_PROGRESS_ORDER_KEY: OKEY } = prefs;

function withWindow(storage, fn) {
  const events = [];
  const previous = globalThis.window;
  globalThis.window = { localStorage: storage, dispatchEvent: (event) => { events.push(event.type); return true; } };
  try { return fn(events); } finally { globalThis.window = previous; }
}
const memoryStorage = (initial = {}) => {
  const data = new Map(Object.entries(initial));
  return { getItem: (k) => (data.has(k) ? data.get(k) : null), setItem: (k, v) => data.set(k, String(v)), data };
};
const throwing = { getItem() { throw new Error("denied"); }, setItem() { throw new Error("denied"); } };

test("keys and defaults are the documented ones (card grid, newest first)", () => {
  assert.equal(QKEY, "pi-web:polyforge-queue-view");
  assert.equal(OKEY, "pi-web:polyforge-progress-order");
  assert.deepEqual([prefs.POLYFORGE_QUEUE_VIEW_DEFAULT, prefs.POLYFORGE_PROGRESS_ORDER_DEFAULT], ["card", "desc"], "the queue defaults to the card grid, the progress to newest first");
  assert.equal(prefs.POLYFORGE_QUEUE_VIEW_EVENT, "pi-web:polyforge-queue-view-changed");
  assert.equal(prefs.POLYFORGE_PROGRESS_ORDER_EVENT, "pi-web:polyforge-progress-order-changed");
});

test("absent, corrupt and wrong-typed values parse to the defaults", () => {
  for (const raw of [null, undefined, "", "bogus", "CARDS", "ascending", 1, {}, "card ", "list\n"]) {
    assert.equal(prefs.parsePolyforgeQueueView(raw), "card", String(raw));
    assert.equal(prefs.parsePolyforgeProgressOrder(raw), "desc", String(raw));
  }
  assert.equal(prefs.parsePolyforgeQueueView("list"), "list");
  assert.equal(prefs.parsePolyforgeProgressOrder("asc"), "asc");
});

test("reads go through storage and survive a throwing storage", () => {
  withWindow(memoryStorage({ [QKEY]: "list", [OKEY]: "desc" }), () => {
    assert.equal(prefs.readPolyforgeQueueView(), "list");
    assert.equal(prefs.readPolyforgeProgressOrder(), "desc");
  });
  withWindow(memoryStorage({ [QKEY]: "nope", [OKEY]: "" }), () => {
    assert.equal(prefs.readPolyforgeQueueView(), "card");
    assert.equal(prefs.readPolyforgeProgressOrder(), "desc");
  });
  withWindow(throwing, () => {
    assert.equal(prefs.readPolyforgeQueueView(), "card");
    assert.equal(prefs.readPolyforgeProgressOrder(), "desc");
  });
});

test("a write stores the value and announces it; a refused write still takes effect for the page", () => {
  const storage = memoryStorage();
  withWindow(storage, (events) => {
    prefs.writePolyforgeQueueView("list");
    assert.equal(storage.data.get(QKEY), "list");
    prefs.writePolyforgeProgressOrder("asc");
    assert.equal(storage.data.get(OKEY), "asc");
    assert.deepEqual(events, ["pi-web:polyforge-queue-view-changed", "pi-web:polyforge-progress-order-changed"]);
  });
  // A refused write keeps the choice in the session fallback, not storage.
  withWindow(throwing, (events) => {
    prefs.writePolyforgeQueueView("card");
    assert.equal(prefs.readPolyforgeQueueView(), "card");
    prefs.writePolyforgeProgressOrder("desc");
    assert.equal(prefs.readPolyforgeProgressOrder(), "desc");
    assert.deepEqual(events, ["pi-web:polyforge-queue-view-changed", "pi-web:polyforge-progress-order-changed"]);
  });
});

test("the module and the hook stay browser-only: no fetch, no server write, all events heard", async () => {
  const lib = await readFile(new URL("./polyforge-view-preferences.ts", import.meta.url), "utf8");
  const hook = await readFile(new URL("../hooks/usePolyforgeViewPreferences.ts", import.meta.url), "utf8");
  for (const source of [lib, hook]) {
    assert.doesNotMatch(source, /\bfetch\(|from\s+["']node:|\/api\//);
  }
  for (const event of ["storage", "POLYFORGE_QUEUE_VIEW_EVENT", "POLYFORGE_PROGRESS_ORDER_EVENT"]) {
    assert.match(hook, new RegExp(`addEventListener\\(${event === "storage" ? `"${event}"` : event}, onChange\\)`), `${event} is heard`);
  }
  assert.match(hook, /useSyncExternalStore\(subscribe, readQueueViewSnapshot, serverQueueView\)/, "the server snapshot is the default card grid");
  assert.match(hook, /useSyncExternalStore\(subscribe, readProgressOrderSnapshot, serverProgressOrder\)/, "the server snapshot is the default newest first");
});
