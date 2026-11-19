import "#test-env";
import assert from "node:assert/strict";
import test from "node:test";
import { createJiti } from "jiti";

const jiti = createJiti(import.meta.url, {
  alias: { "@": process.cwd() },
  interopDefault: true,
  moduleCache: false,
});
const { equivalentModelCandidates, pickRotatedModel } = await jiti.import("./provider-rotation.ts");

function model(provider, id, { api = "openai-completions", baseUrl = "https://pool.example/v1" } = {}) {
  return { provider, id, api, baseUrl };
}

/** resolveApiKey stub: the key each provider answers with, and which ones fail to resolve. */
function keys(byProvider, { failing = [] } = {}) {
  return (provider) => {
    if (failing.includes(provider)) return Promise.reject(new Error("no auth"));
    return Promise.resolve(byProvider[provider]);
  };
}

test("a model nothing else offers is not rotated", async () => {
  const pool = [model("llm-center", "glm-5.3-mb", { baseUrl: "https://single.example/v1" })];
  assert.equal(await pickRotatedModel(pool[0], pool, keys({ "llm-center": "k1" })), undefined);
  assert.equal(await pickRotatedModel(undefined, pool, keys({})), undefined);
});

test("candidates need the same id, endpoint and API", () => {
  const base = model("pool", "m");
  const visible = [
    base,
    model("pool", "m", { baseUrl: "https://other.example/v1" }),
    model("pool", "m", { api: "anthropic-messages" }),
    model("pool", "other-id"),
    model("other-provider", "m"),
  ];
  assert.deepEqual(equivalentModelCandidates(visible, base).map((m) => m.provider), ["pool", "other-provider"]);
});

test("new sessions cycle the distinct keys round-robin", async () => {
  const pool = [model("p1", "m", { baseUrl: "https://cycle.example/v1" }), model("p2", "m", { baseUrl: "https://cycle.example/v1" }), model("p3", "m", { baseUrl: "https://cycle.example/v1" })];
  const resolve = keys({ p1: "k1", p2: "k2", p3: "k3" });
  const picks = [];
  for (let i = 0; i < 4; i++) picks.push(await pickRotatedModel(pool[0], pool, resolve));
  assert.deepEqual(picks.map((pick) => pick.provider), ["p1", "p2", "p3", "p1"]);
});

test("entries duplicated with one key share a single turn", async () => {
  // pool and pool-copy answer with the same key: the keys, not the entries, are spread.
  const pool = [model("pool", "m", { baseUrl: "https://dup.example/v1" }), model("pool-copy", "m", { baseUrl: "https://dup.example/v1" }), model("pool-2", "m", { baseUrl: "https://dup.example/v1" })];
  const resolve = keys({ pool: "k1", "pool-copy": "k1", "pool-2": "k2" });
  const picks = [];
  for (let i = 0; i < 3; i++) picks.push(await pickRotatedModel(pool[0], pool, resolve));
  assert.deepEqual(picks.map((pick) => pick.provider), ["pool", "pool-2", "pool"]);
});

test("candidates that all share one key are not rotated", async () => {
  const pool = [model("pool", "m", { baseUrl: "https://one.example/v1" }), model("pool-copy", "m", { baseUrl: "https://one.example/v1" })];
  assert.equal(await pickRotatedModel(pool[0], pool, keys({ pool: "k1", "pool-copy": "k1" })), undefined);
});

test("a key that fails to resolve leaves its provider bucketing alone", async () => {
  const pool = [model("p1", "m", { baseUrl: "https://failing.example/v1" }), model("p2", "m", { baseUrl: "https://failing.example/v1" })];
  const resolve = keys({ p1: "k1", p2: "k2" }, { failing: ["p2"] });
  const picks = [];
  for (let i = 0; i < 2; i++) picks.push(await pickRotatedModel(pool[0], pool, resolve));
  assert.deepEqual(picks.map((pick) => pick.provider), ["p1", "p2"]);
});

test("providers without an API key (OAuth) bucket alone", async () => {
  const pool = [model("oauth-a", "m", { baseUrl: "https://oauth.example/v1" }), model("oauth-b", "m", { baseUrl: "https://oauth.example/v1" })];
  const resolve = keys({ "oauth-a": undefined, "oauth-b": undefined });
  const picks = [];
  for (let i = 0; i < 2; i++) picks.push(await pickRotatedModel(pool[0], pool, resolve));
  assert.deepEqual(picks.map((pick) => pick.provider), ["oauth-a", "oauth-b"]);
});

test("each model identity keeps its own turns", async () => {
  const glm = [model("p1", "glm", { baseUrl: "https://glm.example/v1" }), model("p2", "glm", { baseUrl: "https://glm.example/v1" })];
  const opus = [model("q1", "opus", { baseUrl: "https://opus.example/v1" }), model("q2", "opus", { baseUrl: "https://opus.example/v1" })];
  const resolve = keys({ p1: "g1", p2: "g2", q1: "o1", q2: "o2" });
  assert.equal((await pickRotatedModel(glm[0], glm, resolve)).provider, "p1");
  // Rotating the opus pool does not advance the glm pool's turn.
  assert.equal((await pickRotatedModel(opus[0], opus, resolve)).provider, "q1");
  assert.equal((await pickRotatedModel(glm[0], glm, resolve)).provider, "p2");
});

test("two sessions starting at once take different turns", async () => {
  const pool = [model("p1", "m", { baseUrl: "https://racing.example/v1" }), model("p2", "m", { baseUrl: "https://racing.example/v1" }), model("p3", "m", { baseUrl: "https://racing.example/v1" })];
  const resolve = keys({ p1: "k1", p2: "k2", p3: "k3" });
  // Not awaited in turn: both read the counter before either key resolves.
  const [a, b] = await Promise.all([
    pickRotatedModel(pool[0], pool, resolve),
    pickRotatedModel(pool[0], pool, resolve),
  ]);
  assert.notEqual(a.provider, b.provider);
});
