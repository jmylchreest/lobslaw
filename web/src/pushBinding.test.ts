import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { IDBFactory } from "fake-indexeddb";
import { clearLocalPush, pushBindingEpoch, savePushBinding } from "./pushBinding";

beforeEach(() => {
  vi.stubGlobal("indexedDB", new IDBFactory());
  vi.stubGlobal("navigator", {});
});
afterEach(() => vi.unstubAllGlobals());

async function binding() {
  const db = await new Promise<IDBDatabase>((resolve, reject) => {
    const request = indexedDB.open("lobslaw-push", 1);
    request.onsuccess = () => resolve(request.result); request.onerror = () => reject(request.error);
  });
  try {
    return await new Promise<{ id: string; epoch: number }>((resolve, reject) => {
      const request = db.transaction("state", "readonly").objectStore("state").get("audience");
      request.onsuccess = () => resolve(request.result); request.onerror = () => reject(request.error);
    });
  } finally { db.close(); }
}

describe("push audience isolation", () => {
  it("rejects an old tab's delayed renewal after an account switch", async () => {
    const expires = new Date(Date.now() + 3600000).toISOString();
    const oldEpoch = await pushBindingEpoch();
    expect(await savePushBinding("alice", expires, oldEpoch)).toBe(true);
    await clearLocalPush();
    expect(await savePushBinding("bob", expires, await pushBindingEpoch())).toBe(true);
    expect(await savePushBinding("alice-late-response", expires, oldEpoch)).toBe(false);
    expect((await binding()).id).toBe("bob");
  });

  it("leaves no audience after a renewal and logout race", async () => {
    const epoch = await pushBindingEpoch();
    await Promise.all([
      savePushBinding("late", new Date(Date.now() + 3600000).toISOString(), epoch),
      clearLocalPush(),
    ]);
    expect((await binding()).id).toBe("");
  });

  it("clears the audience even when browser unsubscription fails", async () => {
    const close = vi.fn();
    vi.stubGlobal("navigator", { serviceWorker: { getRegistration: async () => ({
      getNotifications: async () => [{ close }],
      pushManager: { getSubscription: async () => ({ unsubscribe: async () => { throw new Error("offline"); } }) },
    }) } });
    await savePushBinding("alice", new Date(Date.now() + 3600000).toISOString(), await pushBindingEpoch());
    await clearLocalPush();
    expect((await binding()).id).toBe("");
    expect(close).toHaveBeenCalledOnce();
  });
});
