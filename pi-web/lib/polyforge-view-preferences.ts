/**
 * The Polyforge panel's inline view choices — the Jobs queue's card/list
 * presentation and the execution-progress order — each with its own
 * namespaced key in this browser's localStorage: never pf config, never a
 * server file, never a request. The queue defaults to the card stream, the
 * progress to newest first (倒序: the newest block on top). An absent,
 * blocked, unreadable or wrong-typed value reads as the default. A write
 * storage refuses still takes effect for this page's lifetime.
 * Client-safe: no Node import.
 *
 * Restored from the retired 0.1.x fork's `pi-web:polyforge-queue-view` /
 * `pi-web:polyforge-progress-order` keys, so a browser that stored a choice
 * on the fork keeps it.
 */

/** The Jobs queue's presentation: the horizontally scrolling card stream or
 *  the compact one-line-per-job list. */
export type PolyforgeQueueView = "card" | "list";
/** The execution-progress order: newest first (desc) or oldest first. */
export type PolyforgeProgressOrder = "desc" | "asc";
export const POLYFORGE_QUEUE_VIEW_KEY = "pi-web:polyforge-queue-view";
export const POLYFORGE_QUEUE_VIEW_EVENT = "pi-web:polyforge-queue-view-changed";
export const POLYFORGE_QUEUE_VIEW_DEFAULT: PolyforgeQueueView = "card";

export const POLYFORGE_PROGRESS_ORDER_KEY = "pi-web:polyforge-progress-order";
export const POLYFORGE_PROGRESS_ORDER_EVENT = "pi-web:polyforge-progress-order-changed";
export const POLYFORGE_PROGRESS_ORDER_DEFAULT: PolyforgeProgressOrder = "desc";

/** A stored string → the queue view (default when not one of the two).
 *  Pure; never throws. */
export function parsePolyforgeQueueView(raw: unknown): PolyforgeQueueView {
  return raw === "list" ? "list" : "card";
}

/** A stored string → the progress order (default when not one of the two).
 *  Pure; never throws. */
export function parsePolyforgeProgressOrder(raw: unknown): PolyforgeProgressOrder {
  return raw === "asc" ? "asc" : "desc";
}

function readStored(key: string): string | null {
  try {
    return window.localStorage.getItem(key);
  } catch {
    return null;
  }
}

// A refused write's value for this page's lifetime (blocked storage): the
// toggle still works in-memory. Null while storage holds the truth.
let sessionQueueView: PolyforgeQueueView | null = null;
let sessionProgressOrder: PolyforgeProgressOrder | null = null;
/** The queue view in effect; the same primitive until the state changes, so
 *  it serves as a useSyncExternalStore snapshot. */
export function readPolyforgeQueueView(): PolyforgeQueueView {
  if (sessionQueueView) return sessionQueueView;
  return parsePolyforgeQueueView(readStored(POLYFORGE_QUEUE_VIEW_KEY));
}

/** The progress order in effect; the same primitive until the state
 *  changes, so it serves as a useSyncExternalStore snapshot. */
export function readPolyforgeProgressOrder(): PolyforgeProgressOrder {
  if (sessionProgressOrder) return sessionProgressOrder;
  return parsePolyforgeProgressOrder(readStored(POLYFORGE_PROGRESS_ORDER_KEY));
}

function writeStored(key: string, value: string, event: string): boolean {
  try {
    window.localStorage.setItem(key, value);
    return true;
  } catch {
    return false;
  } finally {
    try {
      window.dispatchEvent(new Event(event));
    } catch {
      // no window event target: the next read still sees the new state
    }
  }
}

/** Choose the queue view, store it when storage allows (a write storage
 *  refused stays in effect for this page's lifetime), and tell the open
 *  page either way. Never throws. */
export function writePolyforgeQueueView(view: PolyforgeQueueView): void {
  sessionQueueView = writeStored(POLYFORGE_QUEUE_VIEW_KEY, view, POLYFORGE_QUEUE_VIEW_EVENT) ? null : view;
}

/** Choose the progress order, store it when storage allows (a write storage
 *  refused stays in effect for this page's lifetime), and tell the open
 *  page either way. Never throws. */
export function writePolyforgeProgressOrder(order: PolyforgeProgressOrder): void {
  sessionProgressOrder = writeStored(POLYFORGE_PROGRESS_ORDER_KEY, order, POLYFORGE_PROGRESS_ORDER_EVENT) ? null : order;
}
