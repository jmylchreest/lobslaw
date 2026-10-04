import { useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { FilePreview, useUploads } from "./Uploads";
import { formatFileSize, type MessageFile } from "../uploads";

export function Composer({ conversation, draft, onChange, onSend, busy, onStop, disabled, placeholder = "Message your assistant…" }: {
  conversation: string; draft: string; onChange: (value: string) => void; onSend: (ids: string[], files: MessageFile[]) => void;
  busy: boolean; onStop?: () => void; disabled?: boolean; placeholder?: string;
}) {
  const id = useId();
  const input = useRef<HTMLTextAreaElement>(null);
  const form = useRef<HTMLFormElement>(null);
  const picker = useRef<HTMLInputElement>(null);
  const [dragging, setDragging] = useState(false);
  const [, updateExpiry] = useState(0);
  const { policy, error, selected, addFiles, removeFile, retryFile } = useUploads(conversation);
  const expired = selected.some((file) => file.uploaded && Date.parse(file.uploaded.expires_at) <= Date.now());
  const ready = selected.every((file) => file.state === "ready") && !expired;
  const canSend = !busy && !disabled && ready && (!!draft.trim() || !!selected.length);
  useEffect(() => {
    const expires = selected.flatMap((file) => file.uploaded ? [Date.parse(file.uploaded.expires_at)] : []);
    if (!expires.length || Math.min(...expires) <= Date.now()) return;
    const timer = window.setTimeout(() => updateExpiry((value) => value + 1), Math.min(...expires) - Date.now() + 10);
    return () => clearTimeout(timer);
  }, [selected]);
  const submit = () => {
    if (!canSend || selected.some((file) => file.uploaded && Date.parse(file.uploaded.expires_at) <= Date.now())) return;
    const ids = selected.map((file) => file.uploaded!.upload_id);
    const files = selected.map((file) => ({ name: file.uploaded!.filename || file.file.name, size: file.file.size, mime: file.mime, file: file.file }));
    onSend(ids, files);
  };
  useLayoutEffect(() => {
    if (!form.current) return;
    const element = form.current.closest(".composer-reveal") ?? form.current;
    const update = () => document.documentElement.style.setProperty("--composer-height", `${Math.ceil(element.getBoundingClientRect().height)}px`);
    const observer = new ResizeObserver(update);
    observer.observe(element); update();
    return () => { observer.disconnect(); document.documentElement.style.removeProperty("--composer-height"); };
  }, []);
  useLayoutEffect(() => {
    if (!input.current) return;
    input.current.style.height = "auto";
    input.current.style.height = `${Math.min(136, Math.max(44, input.current.scrollHeight))}px`;
  }, [draft]);
  return <form ref={form} className={`composer${dragging ? " drop-active" : ""}`} onSubmit={(event) => { event.preventDefault(); submit(); }}
    onDragOver={(event) => { if (policy?.enabled && !busy && !disabled && event.dataTransfer.types.includes("Files")) { event.preventDefault(); setDragging(true); } }}
    onDragLeave={(event) => { if (!event.currentTarget.contains(event.relatedTarget as Node)) setDragging(false); }}
    onDrop={(event) => { event.preventDefault(); setDragging(false); if (!busy && !disabled) addFiles(Array.from(event.dataTransfer.files)); }}>
    {!!selected.length && <div className="attachment-tray" aria-label="Attached files">{selected.map((entry) => <div className={`attachment-card ${entry.state}`} key={entry.id}>
      <FilePreview file={entry.file} mime={entry.mime} /><div className="grow"><b className="attachment-name">{entry.file.name}</b><span className="attachment-meta">{formatFileSize(entry.file.size)} · {entry.state === "ready" ? "Ready" : entry.state === "failed" ? "Failed" : entry.state === "queued" ? "Waiting" : `Uploading ${entry.progress}%`}</span>
      {entry.state === "uploading" && <progress value={entry.progress} max={100} aria-label={`Uploading ${entry.file.name}`} />}
      {entry.error && <span className="attachment-error" role="alert">{entry.error}</span>}
      {(entry.state === "failed" || (entry.uploaded && Date.parse(entry.uploaded.expires_at) <= Date.now())) && <button className="btn ghost sm" type="button" disabled={busy} onClick={() => retryFile(entry.id)}>Retry upload</button>}
      </div><button className="btn ghost sm attachment-remove" type="button" aria-label={`Remove ${entry.file.name}`} disabled={busy} onClick={() => removeFile(entry.id)}>×</button>
    </div>)}</div>}
    {error && <div className="upload-error" role="alert">{error}</div>}
    {expired && <div className="upload-error" role="alert">An upload expired. Retry it before sending.</div>}
    <div className="composer-in">
      <input ref={picker} type="file" multiple className="sr-only" tabIndex={-1} aria-label="Choose attachments" accept={policy ? [...policy.media_types, ".txt", ".md", ".csv", ".json", ".pdf", ".xml", ".yaml", ".yml"].join(",") : undefined}
        onChange={(event) => { addFiles(Array.from(event.currentTarget.files ?? [])); event.currentTarget.value = ""; }} />
      <button className="btn ghost attach-button" type="button" aria-label="Attach files" disabled={!policy?.enabled || busy || disabled} title={policy?.enabled ? `Attach images, audio or documents (up to ${formatFileSize(policy.max_bytes)} each)` : "Uploads are unavailable on this gateway"} onClick={() => picker.current?.click()}>
        <svg viewBox="0 0 24 24" aria-hidden="true"><path d="m9 17 8-8a3 3 0 0 0-4-4L5 13a5 5 0 0 0 7 7l8-8M8 14l7-7" /></svg>
      </button>
      <div className="composer-field grow">
        <label className="sr-only" htmlFor={id}>Message</label>
        <textarea id={id} ref={input} className="ta" rows={1} value={draft} placeholder={placeholder} disabled={disabled}
          enterKeyHint="enter" onChange={(event) => onChange(event.target.value)} onKeyDown={(event) => {
            if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing && !window.matchMedia("(pointer: coarse)").matches) {
              event.preventDefault(); submit();
            }
          }} onPaste={(event) => {
            const files = Array.from(event.clipboardData.files);
            if (files.length && policy?.enabled && !busy && !disabled) { event.preventDefault(); addFiles(files); }
          }} />
      </div>
      {busy && onStop ? <button className="btn stop-response" type="button" onClick={onStop} aria-label="Stop response"><span className="stop-icon" aria-hidden="true" />Stop</button>
        : <button className="btn primary" type="submit" disabled={!canSend}>{busy ? "Sending…" : "Send"}</button>}
    </div>
    <div className="composer-hint">{busy ? (onStop ? "Your reply continues if you close this tab. Reopen to reconnect." : "Waiting for the assistant…") : "Enter to send · Shift + Enter for a new line"}</div>
  </form>;
}
