import "#test-env";
import assert from "node:assert/strict";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { createJiti } from "jiti";

const jiti = createJiti(import.meta.url, { interopDefault: true });

function modelsJson(providers) {
  return JSON.stringify({ providers: Object.fromEntries(providers.map(([id, key, baseUrl, models]) => [
    id,
    {
      baseUrl,
      apiKey: key,
      api: "openai-completions",
      models: models.map((modelId) => ({
        id: modelId,
        name: modelId,
        reasoning: false,
        input: ["text"],
      })),
    },
  ])) });
}

async function withAgentHome(t, providers) {
  const dir = await mkdtemp(join(tmpdir(), "pi-web-rotation-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  await writeFile(join(dir, "models.json"), modelsJson(providers));
  const previousDir = process.env.PI_CODING_AGENT_DIR;
  process.env.PI_CODING_AGENT_DIR = dir;
  t.after(() => {
    if (previousDir === undefined) delete process.env.PI_CODING_AGENT_DIR;
    else process.env.PI_CODING_AGENT_DIR = previousDir;
  });
  const { startRpcSession } = await jiti.import("./rpc-manager.ts");
  return { dir, startRpcSession };
}

async function startModelOf(startRpcSession, dir, id, options = {}) {
  const { session } = await startRpcSession(id, "", dir, options);
  try {
    const state = await session.send({ type: "get_state" });
    return state.model ? `${state.model.provider}/${state.model.id}` : null;
  } finally {
    await session.shutdown();
  }
}

test("new sessions spread over an equivalent-provider key pool", async (t) => {
  const { dir, startRpcSession } = await withAgentHome(t, [
    ["pool", "k1", "https://pool.example/v1", ["m"]],
    ["pool-2", "k2", "https://pool.example/v1", ["m"]],
    // Same key pool, another model: without rotation this single provider would
    // take every session while its key's rate limit is exhausted first.
    ["unrelated", "k9", "https://unrelated.example/v1", ["other"]],
  ]);
  const picks = [];
  for (let i = 0; i < 3; i++) picks.push(await startModelOf(startRpcSession, dir, `__new__spread-${i}`));
  // pi starts each session on the first entry; the spread moves every second one over.
  assert.deepEqual(picks, ["pool/m", "pool-2/m", "pool/m"]);
});

test("a model the browser chose is not spread away", async (t) => {
  const { dir, startRpcSession } = await withAgentHome(t, [
    ["pool", "k1", "https://chosen.example/v1", ["m"]],
    ["pool-2", "k2", "https://chosen.example/v1", ["m"]],
  ]);
  const picks = [];
  for (let i = 0; i < 2; i++) {
    picks.push(await startModelOf(startRpcSession, dir, `__new__chosen-${i}`, {
      initialModel: { provider: "pool-2", modelId: "m" },
    }));
  }
  assert.deepEqual(picks, ["pool-2/m", "pool-2/m"]);
});

test("entries duplicated with one key share the spread's turns", async (t) => {
  const { dir, startRpcSession } = await withAgentHome(t, [
    ["pool3", "k1", "https://duplicate.example/v1", ["m"]],
    ["pool3-copy", "k1", "https://duplicate.example/v1", ["m"]],
    ["pool3-2", "k2", "https://duplicate.example/v1", ["m"]],
  ]);
  const picks = [];
  for (let i = 0; i < 3; i++) picks.push(await startModelOf(startRpcSession, dir, `__new__dup-${i}`));
  assert.deepEqual(picks, ["pool3/m", "pool3-2/m", "pool3/m"]);
});
