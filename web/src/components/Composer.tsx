import { useId, useLayoutEffect, useRef } from "react";

export function Composer({ draft, onChange, onSend, busy, onStop, disabled, placeholder = "Message your assistant…" }: {
  draft: string; onChange: (value: string) => void; onSend: () => void;
  busy: boolean; onStop?: () => void; disabled?: boolean; placeholder?: string;
}) {
  const id = useId();
  const input = useRef<HTMLTextAreaElement>(null);
  const form = useRef<HTMLFormElement>(null);
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
  return <form ref={form} className="composer" onSubmit={(event) => { event.preventDefault(); if (!busy && !disabled && draft.trim()) onSend(); }}>
    <div className="composer-in">
      <div className="composer-field grow">
        <label className="sr-only" htmlFor={id}>Message</label>
        <textarea id={id} ref={input} className="ta" rows={1} value={draft} placeholder={placeholder} disabled={disabled}
          enterKeyHint="enter" onChange={(event) => onChange(event.target.value)} onKeyDown={(event) => {
            if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing && !window.matchMedia("(pointer: coarse)").matches) {
              event.preventDefault(); if (!busy && !disabled && draft.trim()) onSend();
            }
          }} />
      </div>
      {busy && onStop ? <button className="btn stop-response" type="button" onClick={onStop} aria-label="Stop response"><span className="stop-icon" aria-hidden="true" />Stop</button>
        : <button className="btn primary" type="submit" disabled={disabled || busy || !draft.trim()}>{busy ? "Sending…" : "Send"}</button>}
    </div>
    <div className="composer-hint">{busy ? (onStop ? "Your reply continues if you close this tab. Reopen to reconnect." : "Waiting for the assistant…") : "Enter to send · Shift + Enter for a new line"}</div>
  </form>;
}
