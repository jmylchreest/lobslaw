export interface UploadPolicy { enabled: boolean; max_bytes: number; max_files: number; media_types: string[] }
export interface UploadedFile { upload_id: string; filename: string; mime_type: string; size: number; expires_at: string }
export interface SelectedFile {
  id: string; file: File; mime: string; progress: number;
  state: "queued" | "uploading" | "ready" | "failed";
  uploaded?: UploadedFile; error?: string;
}
export interface MessageFile { name: string; size: number; mime: string; file?: File }

const extensions: Record<string, string> = {
  png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", gif: "image/gif", webp: "image/webp",
  ogg: "audio/ogg", webm: "audio/webm", mp3: "audio/mpeg", m4a: "audio/mp4", wav: "audio/wav", flac: "audio/flac",
  pdf: "application/pdf", txt: "text/plain", log: "text/plain", md: "text/markdown", csv: "text/csv", json: "application/json", xml: "application/xml", yaml: "application/yaml", yml: "application/yaml",
};
export function fileMediaType(file: Pick<File, "type" | "name">, policy: UploadPolicy): string | null {
  const supplied = file.type.split(";")[0];
  const type = ({ "text/xml": "application/xml", "text/yaml": "application/yaml", "audio/x-wav": "audio/wav" } as Record<string, string>)[supplied] || supplied;
  if (policy.media_types.includes(type)) return type;
  const fallback = extensions[file.name.split(".").at(-1)?.toLowerCase() ?? ""];
  return (!type || type === "application/octet-stream") && policy.media_types.includes(fallback) ? fallback : null;
}
export function formatFileSize(bytes: number) {
  return bytes >= 1024 * 1024 ? `${(bytes / (1024 * 1024)).toFixed(1)} MB` : bytes >= 1024 ? `${Math.ceil(bytes / 1024)} KB` : `${bytes} B`;
}

/** Raw, same-origin upload: browser progress reports actual transferred bytes. */
export function uploadFile(file: File, mime: string, signal: AbortSignal, progress: (percent: number) => void): Promise<UploadedFile> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    const abort = () => xhr.abort();
    const finish = () => signal.removeEventListener("abort", abort);
    xhr.open("POST", "/v1/uploads"); xhr.withCredentials = true; xhr.timeout = 120000;
    xhr.setRequestHeader("Content-Type", mime); xhr.setRequestHeader("X-Upload-Name", encodeURIComponent(file.name));
    xhr.upload.onprogress = (event) => { if (event.lengthComputable) progress(Math.min(99, Math.round(event.loaded / event.total * 100))); };
    xhr.onload = () => {
      finish();
      let body: UploadedFile & { error?: string };
      try { body = JSON.parse(xhr.responseText); } catch { reject(new Error("The node returned an invalid upload response.")); return; }
      if (xhr.status === 401) window.dispatchEvent(new Event("lobslaw:unauthorized"));
      if (xhr.status < 200 || xhr.status >= 300) { reject(new Error(body.error || `Upload failed (${xhr.status})`)); return; }
      if (!body.upload_id || !body.expires_at) { reject(new Error("Upload response did not include a usable file reference.")); return; }
      progress(100); resolve(body);
    };
    xhr.onerror = () => { finish(); reject(new Error("Upload interrupted. Check your connection and retry.")); };
    xhr.ontimeout = () => { finish(); reject(new Error("Upload timed out. Try again.")); };
    xhr.onabort = () => { finish(); reject(new DOMException("Upload cancelled", "AbortError")); };
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) { finish(); reject(new DOMException("Upload cancelled", "AbortError")); return; }
    xhr.send(file);
  });
}
