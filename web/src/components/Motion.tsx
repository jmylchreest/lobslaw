import { createContext, useCallback, useContext, useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode, type RefObject } from "react";
import { flushSync } from "react-dom";

const motionQuery = "(prefers-reduced-motion: reduce)";

/** CSS handles loops; this also stops imperative motion if the preference changes. */
export function useReducedMotion() {
  const [reduced, setReduced] = useState(() => typeof window !== "undefined" && window.matchMedia(motionQuery).matches);
  useEffect(() => {
    const query = window.matchMedia(motionQuery);
    const update = () => setReduced(query.matches);
    update(); query.addEventListener("change", update);
    return () => query.removeEventListener("change", update);
  }, []);
  return reduced;
}

const frames = {
  status: [{ opacity: 0.45, transform: "translateY(3px)" }, { opacity: 1, transform: "translateY(0)" }],
  complete: [{ transform: "scale(1)" }, { transform: "scale(1.3)", offset: 0.4 }, { transform: "scale(1)" }],
  mascot: [{ transform: "translateY(0) rotate(0)" }, { transform: "translateY(-3px) rotate(-5deg)", offset: 0.4 }, { transform: "translateY(0) rotate(0)" }],
  settle: [{ transform: "translateY(-1px) rotate(2deg) scale(1.02)" }, { transform: "translateY(0) rotate(0) scale(1)" }],
};

/** A changed value animates once. Equal poll results and initial history stay still. */
export function useChangeMotion<T extends Element>(value: unknown, preset: keyof typeof frames = "status") {
  const ref = useRef<T>(null);
  const previous = useRef(value);
  const reduced = useReducedMotion();
  useLayoutEffect(() => {
    const changed = previous.current !== value;
    previous.current = value;
    if (!changed || reduced || !ref.current?.animate) return;
    const animation = ref.current.animate(frames[preset], { duration: preset === "status" ? 220 : 360, easing: "cubic-bezier(0.22, 1, 0.36, 1)" });
    return () => animation.cancel();
  }, [value, preset, reduced]);
  return ref;
}

export function StatusMark({ status, className = "" }: { status: string; className?: string }) {
  const done = status === "done" || status === "completed";
  const ref = useChangeMotion<HTMLSpanElement>(status, done ? "complete" : "status");
  return <span ref={ref} className={`status-mark ${className}`} data-status={status} aria-hidden="true">
    {done && <svg viewBox="0 0 12 12"><path d="m3 6 2 2 4-4" /></svg>}
  </span>;
}

export function StateText({ value, className = "", children }: { value: string; className?: string; children: ReactNode }) {
  const ref = useChangeMotion<HTMLSpanElement>(value);
  return <span ref={ref} className={`state-text ${className}`}>{children}</span>;
}

export function CheckIcon() {
  return <svg className="success-check" viewBox="0 0 16 16" aria-hidden="true" focusable="false"><path d="m3 8 3 3 7-7" /></svg>;
}

export function Chevron({ className = "" }: { className?: string }) {
  return <svg className={`disclosure-chevron ${className}`} viewBox="0 0 12 12" aria-hidden="true" focusable="false"><path d="m4 2 4 4-4 4" /></svg>;
}

/** Establish a baseline before highlighting arrivals, and reset it on team switches. */
export function useNewItems(items: { id: string }[] | null, scope: string) {
  const seen = useRef<{ scope: string; ids: Set<string> } | null>(null);
  const [arrivals, setArrivals] = useState<{ scope: string; ids: Set<string> }>({ scope, ids: new Set() });
  useEffect(() => {
    if (!items) return;
    if (!seen.current || seen.current.scope !== scope) {
      seen.current = { scope, ids: new Set(items.map((item) => item.id)) };
      setArrivals({ scope, ids: new Set() });
      return;
    }
    const ids = new Set(items.filter((item) => !seen.current!.ids.has(item.id)).map((item) => item.id));
    for (const item of items) seen.current.ids.add(item.id);
    if (!ids.size) return;
    setArrivals({ scope, ids });
  }, [items, scope]);
  useEffect(() => {
    if (!arrivals.ids.size) return;
    const timer = window.setTimeout(() => setArrivals({ scope: arrivals.scope, ids: new Set() }), 700);
    return () => window.clearTimeout(timer);
  }, [arrivals]);
  return arrivals.scope === scope ? arrivals.ids : new Set<string>();
}

/** Grid interpolation keeps natural content height and makes closing content inert. */
export function Collapse({ open, id, className = "", children }: { open: boolean; id?: string; className?: string; children: ReactNode }) {
  return <div id={id} className={`collapse${open ? " is-open" : ""} ${className}`} inert={!open} aria-hidden={!open}>
    <div className="collapse-inner">{children}</div>
  </div>;
}

export function Disclosure({ title, className = "", children }: {
  title: ReactNode; className?: string; children: ReactNode | ((hasOpened: boolean) => ReactNode);
}) {
  const [open, setOpen] = useState(false);
  const [hasOpened, setHasOpened] = useState(false);
  const id = useId();
  return <div className={`disclosure ${className}`}>
    <button className="disclosure-toggle" type="button" aria-expanded={open} aria-controls={id} onClick={() => {
      setOpen(!open); setHasOpened(true);
    }}><Chevron /><span>{title}</span></button>
    <Collapse open={open} id={id}>{typeof children === "function" ? children(hasOpened) : children}</Collapse>
  </div>;
}

/** Named snapshots crossfade teams without remounting stateful conversations. */
export function switchView(update: () => void) {
  if (window.matchMedia(motionQuery).matches || !document.startViewTransition) { update(); return; }
  const transition = document.startViewTransition(() => flushSync(update));
  // A superseded transition is normal when switching again quickly.
  void transition.ready.catch(() => {});
  void transition.finished.catch(() => {});
}

export function NavigationMarker({ container, location, layoutKey }: {
  container: RefObject<HTMLElement | null>; location: string; layoutKey: string;
}) {
  const [position, setPosition] = useState<{ top: number; height: number; visible: boolean } | null>(null);
  useLayoutEffect(() => {
    const side = container.current;
    if (!side) return;
    const measure = () => {
      const active = side.querySelector<HTMLElement>('a[aria-current="page"]');
      if (!active) { setPosition(null); return; }
      const bounds = active.getBoundingClientRect();
      const parent = (active.closest(".roster") ?? side).getBoundingClientRect();
      setPosition({ top: bounds.top - side.getBoundingClientRect().top + side.scrollTop + 8,
        height: Math.max(8, bounds.height - 16), visible: bounds.bottom > parent.top && bounds.top < parent.bottom });
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(side);
    for (const child of side.querySelectorAll(".roster, .side-foot")) observer.observe(child);
    side.addEventListener("scroll", measure, true);
    window.addEventListener("resize", measure);
    return () => { observer.disconnect(); side.removeEventListener("scroll", measure, true); window.removeEventListener("resize", measure); };
  }, [container, location, layoutKey]);
  return position && <span className="nav-active-marker" aria-hidden="true" style={{
    transform: `translateY(${position.top}px)`, height: position.height, opacity: position.visible ? 1 : 0,
  }} />;
}

const FeedbackContext = createContext<(message: string) => void>(() => {});
export const useFeedback = () => useContext(FeedbackContext);

export function FeedbackProvider({ children }: { children: ReactNode }) {
  const [feedback, setFeedback] = useState<{ message: string; id: number } | null>(null);
  const sequence = useRef(0);
  const notify = useCallback((message: string) => setFeedback({ message, id: ++sequence.current }), []);
  useEffect(() => {
    if (!feedback) return;
    const timer = window.setTimeout(() => setFeedback(null), 3000);
    return () => window.clearTimeout(timer);
  }, [feedback]);
  useEffect(() => {
    const clear = () => setFeedback(null);
    window.addEventListener("lobslaw:unauthorized", clear);
    return () => window.removeEventListener("lobslaw:unauthorized", clear);
  }, []);
  return <FeedbackContext.Provider value={notify}>
    {children}
    <div className="feedback-region" role="status" aria-live="polite">
      {feedback && <div className="action-feedback" key={feedback.id}>
        <CheckIcon /><span>{feedback.message}</span>
        <button className="btn ghost sm" aria-label="Dismiss confirmation" onClick={() => setFeedback(null)}>×</button>
      </div>}
    </div>
  </FeedbackContext.Provider>;
}
