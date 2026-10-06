import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { api } from "../api";
import { fileMediaType, formatFileSize, uploadFile, type MessageFile, type SelectedFile, type UploadPolicy } from "../uploads";

const UploadContext = createContext<{
  files: Record<string, SelectedFile[]>; policy: UploadPolicy | null; error: string;
  add: (key: string, files: File[]) => void; remove: (key: string, id: string) => void;
  retry: (key: string, id: string) => void; sent: (key: string, ids: string[]) => void;
} | null>(null);

/** Draft files and in-flight uploads survive in-app navigation, not account logout. */
export function UploadsProvider({ children }: { children: ReactNode }) {
  const [files, setFiles] = useState<Record<string, SelectedFile[]>>({});
  const ref = useRef(files); ref.current = files;
  const [policy, setPolicy] = useState<UploadPolicy | null>(null);
  const [error, setError] = useState("");
  const controllers = useRef(new Map<string, AbortController>());
  const sequence = useRef(0);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    void api.uploadPolicy().then((value) => { if (alive.current) setPolicy(value); }).catch(() => { if (alive.current) setError("File uploads are unavailable on this gateway."); });
    const active = controllers.current;
    return () => { alive.current = false; for (const controller of active.values()) controller.abort(); active.clear(); };
  }, []);
  const update = useCallback((key: string, id: string, patch: Partial<SelectedFile>) => {
    if (alive.current) setFiles((current) => ({ ...current, [key]: (current[key] ?? []).map((entry) => entry.id === id ? { ...entry, ...patch } : entry) }));
  }, []);
  // Serial admission respects the owner's upload reservation budget.
  useEffect(() => {
    if (controllers.current.size) return;
    for (const [key, entries] of Object.entries(files)) {
      const entry = entries.find((file) => file.state === "queued");
      if (!entry) continue;
      const controller = new AbortController(); controllers.current.set(entry.id, controller);
      update(key, entry.id, { state: "uploading" });
      void uploadFile(entry.file, entry.mime, controller.signal, (progress) => update(key, entry.id, { progress })).then((uploaded) => {
        if (controller.signal.aborted) { void api.removeUpload(uploaded.upload_id).catch(() => {}); return; }
        update(key, entry.id, { state: "ready", uploaded, progress: 100 });
      }).catch((failure: Error) => {
        if (!controller.signal.aborted) update(key, entry.id, { state: "failed", error: failure.message });
      }).finally(() => { controllers.current.delete(entry.id); if (alive.current) setFiles((current) => ({ ...current })); });
      break;
    }
  }, [files, update]);
  const add = useCallback((key: string, incoming: File[]) => {
    if (!policy?.enabled) return;
    let count = ref.current[key]?.length ?? 0;
    const added: SelectedFile[] = [];
    const rejected: string[] = [];
    for (const file of incoming) {
      const mime = fileMediaType(file, policy);
      if (count >= policy.max_files) { rejected.push(`Up to ${policy.max_files} files per message.`); break; }
      if (!mime) { rejected.push(`${file.name}: unsupported file type.`); continue; }
      if (!file.size || file.size > policy.max_bytes) { rejected.push(`${file.name}: files must be non-empty and at most ${formatFileSize(policy.max_bytes)}.`); continue; }
      added.push({ id: `file-${++sequence.current}`, file, mime, state: "queued", progress: 0 }); count++;
    }
    setError(rejected.join(" "));
    setFiles((current) => ({ ...current, [key]: [...(current[key] ?? []), ...added] }));
  }, [policy]);
  const remove = useCallback((key: string, id: string) => {
    const entry = ref.current[key]?.find((file) => file.id === id);
    controllers.current.get(id)?.abort(); controllers.current.delete(id);
    if (entry?.uploaded) void api.removeUpload(entry.uploaded.upload_id).catch(() => {});
    setFiles((current) => ({ ...current, [key]: (current[key] ?? []).filter((file) => file.id !== id) }));
  }, []);
  const retry = useCallback((key: string, id: string) => {
    const entry = ref.current[key]?.find((file) => file.id === id);
    if (entry?.uploaded) void api.removeUpload(entry.uploaded.upload_id).catch(() => {});
    update(key, id, { state: "queued", uploaded: undefined, error: undefined, progress: 0 });
  }, [update]);
  const sent = useCallback((key: string, ids: string[]) => {
    setFiles((current) => ({ ...current, [key]: (current[key] ?? []).filter((file) => !ids.includes(file.uploaded?.upload_id ?? "")) }));
  }, []);
  return <UploadContext.Provider value={{ files, policy, error, add, remove, retry, sent }}>{children}</UploadContext.Provider>;
}

export function useUploads(key: string) {
  const context = useContext(UploadContext);
  if (!context) throw new Error("UploadsProvider is required");
  return { ...context, selected: context.files[key] ?? [], addFiles: (files: File[]) => context.add(key, files), removeFile: (id: string) => context.remove(key, id), retryFile: (id: string) => context.retry(key, id) };
}

export function useUploadAcceptance() {
  const context = useContext(UploadContext);
  if (!context) throw new Error("UploadsProvider is required");
  return context.sent;
}

export function FilePreview({ file, mime }: { file?: File; mime: string }) {
  const [url, setURL] = useState<string>();
  useEffect(() => {
    if (!file || !mime.startsWith("image/")) return;
    const object = URL.createObjectURL(file); setURL(object);
    return () => { URL.revokeObjectURL(object); setURL(undefined); };
  }, [file, mime]);
  return url ? <img className="file-preview" src={url} alt="Selected image preview" /> : <svg className="file-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M6 3h8l4 4v14H6zM14 3v5h4M9 12h6M9 16h6" /></svg>;
}

export function MessageFiles({ files }: { files?: MessageFile[] }) {
  if (!files?.length) return null;
  return <div className="message-files">{files.map((entry, index) => <div className="message-file" key={index}>
    <FilePreview file={entry.file} mime={entry.mime} /><span><b>{entry.name}</b><small>{formatFileSize(entry.size)}</small></span>
  </div>)}</div>;
}
