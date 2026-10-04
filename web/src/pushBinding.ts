interface Binding { epoch: number; id: string; expires: string }

function openBindings(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open("lobslaw-push", 1);
    request.onupgradeneeded = () => { request.result.createObjectStore("state"); };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}

// A read/write transaction serializes logout and late renewal callbacks across
// tabs. A component-local cancellation flag alone cannot protect another tab.
async function bindingTransaction(update?: (value: Binding) => Binding | undefined): Promise<Binding> {
  const db = await openBindings();
  try {
    return await new Promise((resolve, reject) => {
      const tx = db.transaction("state", update ? "readwrite" : "readonly");
      const store = tx.objectStore("state");
      const request = store.get("audience");
      let value: Binding = { epoch: 0, id: "", expires: "" };
      request.onsuccess = () => {
        value = request.result || value;
        const next = update?.(value);
        if (next) { value = next; store.put(value, "audience"); }
      };
      tx.oncomplete = () => resolve(value);
      tx.onabort = tx.onerror = () => reject(tx.error || new Error("Notification storage unavailable"));
    });
  } finally { db.close(); }
}

export async function pushBindingEpoch() { return (await bindingTransaction()).epoch; }

export async function savePushBinding(id: string, expires: string, epoch: number): Promise<boolean> {
  if (!id || !Number.isFinite(Date.parse(expires))) throw new Error("Invalid notification binding");
  const value = await bindingTransaction((current) => current.epoch === epoch ? { id, expires, epoch } : undefined);
  return value.epoch === epoch && value.id === id;
}

/** Clear the audience before logout/account changes, including while offline. */
export async function clearLocalPush() {
  if (typeof navigator !== "undefined" && navigator.locks) {
    await navigator.locks.request("lobslaw-push-audience", clearPushState);
  } else await clearPushState();
}

async function clearPushState() {
  if (typeof indexedDB !== "undefined") await bindingTransaction((value) => ({ epoch: value.epoch + 1, id: "", expires: "" }));
  if (typeof localStorage !== "undefined") localStorage.removeItem("lobslaw:push-owner");
  if (typeof navigator === "undefined" || !("serviceWorker" in navigator)) return;
  let reg;
  try { reg = await navigator.serviceWorker.getRegistration(); } catch { return; }
  if (!reg) return;
  try { for (const notice of await reg.getNotifications()) notice.close(); } catch { /* permission may have been revoked */ }
  try { await (await reg.pushManager.getSubscription())?.unsubscribe(); } catch { /* audience already cleared */ }
}
