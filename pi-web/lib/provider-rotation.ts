import type { Api, Model } from "@earendil-works/pi-ai";

/**
 * Round-robin spread of new sessions over equivalent providers.
 *
 * Some setups configure one upstream endpoint several times with different API keys (a key
 * pool: `llm-center`, `llm-center-2`, …) to raise the rate-limit ceiling. pi keeps one
 * provider per model id, so every session that does not name one lands on the first entry
 * and exhausts its key's requests-per-minute while the other keys sit idle. These helpers
 * spread those sessions over the distinct credentials instead.
 */

/** A provider/model pair to switch a session to. */
export interface ProviderRotationPick {
  provider: string;
  modelId: string;
}

/** The resolved API key of a provider; undefined when it has none (OAuth, no key). */
export type ResolveApiKey = (provider: string) => Promise<string | undefined>;

// Next.js can load this module in more than one module graph; the turns must be shared.
declare global {
  var __piWebProviderRotationCounters: Map<string, number> | undefined;
}

function rotationCounters(): Map<string, number> {
  globalThis.__piWebProviderRotationCounters ??= new Map<string, number>();
  return globalThis.__piWebProviderRotationCounters;
}

/** One counter per model identity: rotating one pool must not consume another pool's turn. */
export function providerRotationKey(model: Pick<Model<Api>, "api" | "baseUrl" | "id">): string {
  return `${model.api}\n${model.baseUrl}\n${model.id}`;
}

/** Providers offering the same model on the same endpoint and API: switching between them
 *  changes only which key answers. */
export function equivalentModelCandidates(visible: readonly Model<Api>[], base: Model<Api>): Model<Api>[] {
  return visible.filter((model) =>
    model.id === base.id && model.api === base.api && model.baseUrl === base.baseUrl);
}

/** Grouping key of a candidate: providers that answer with the same key share one turn, so a
 *  duplicate entry (a renamed copy, or one key in two entries) does not get two. */
async function credentialBucketKey(model: Model<Api>, resolveApiKey: ResolveApiKey): Promise<string> {
  try {
    const apiKey = await resolveApiKey(model.provider);
    if (apiKey) return `key:${apiKey}`;
  } catch {
    // The key could not be resolved: this provider buckets alone.
  }
  return `provider:${model.provider}`;
}

/**
 * The provider the next session without a chosen model should start on when the model pi picks
 * is offered by several equivalent providers: a round-robin turn over the distinct
 * credentials, so parallel sessions spread their requests over the keys. Undefined when there
 * is nothing to rotate — a single candidate, or several that all share one key.
 */
export async function pickRotatedModel(
  base: Model<Api> | undefined,
  visible: readonly Model<Api>[],
  resolveApiKey: ResolveApiKey,
): Promise<ProviderRotationPick | undefined> {
  if (!base) return undefined;
  const candidates = equivalentModelCandidates(visible, base);
  if (candidates.length < 2) return undefined;
  // Take the turn before the first await: two sessions starting at once must not read the
  // same counter and land on the same key.
  const counters = rotationCounters();
  const key = providerRotationKey(base);
  const turn = counters.get(key) ?? 0;
  counters.set(key, turn + 1);
  const byCredential = new Map<string, Model<Api>>();
  for (const candidate of candidates) {
    const bucket = await credentialBucketKey(candidate, resolveApiKey);
    if (!byCredential.has(bucket)) byCredential.set(bucket, candidate);
  }
  const buckets = [...byCredential.values()];
  if (buckets.length < 2) return undefined;
  const picked = buckets[turn % buckets.length];
  return { provider: picked.provider, modelId: picked.id };
}
