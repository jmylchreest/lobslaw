import { useState } from "react";

export interface MSOption {
  value: string;
  /** Shown in the list. Falls back to the value. */
  label?: string;
  /** Secondary line: a tool's description, a bot's display name. */
  hint?: string;
}

/** A searchable multi-select.
 *
 * Type to filter, Enter adds the first match, Backspace removes the
 * last chip. Hand-written rather than pulled in: the console ships no
 * component library, and a native <select multiple> cannot be searched,
 * which is the whole point when there are sixty tools.
 */
export function MultiSelect({ label, options, value, onChange, placeholder, hint, exclude }: {
  label: string;
  options: MSOption[];
  value: string[];
  onChange: (v: string[]) => void;
  placeholder?: string;
  hint?: string;
  /** Values offered elsewhere but not here — a bot cannot message itself. */
  exclude?: string[];
}) {
  const [q, setQ] = useState("");
  const [open, setOpen] = useState(false);

  const chosen = new Set(value);
  const banned = new Set(exclude ?? []);
  const pool = options.filter((o) => !banned.has(o.value));
  const byValue = new Map(pool.map((o) => [o.value, o]));
  const needle = q.trim().toLowerCase();
  const matches = needle === ""
    ? pool.filter((o) => !chosen.has(o.value))
    : pool.filter((o) => !chosen.has(o.value) &&
        (o.value.toLowerCase().includes(needle) ||
         (o.label ?? "").toLowerCase().includes(needle)));

  function add(v: string) { onChange([...value, v]); setQ(""); }
  function remove(v: string) { onChange(value.filter((x) => x !== v)); }

  return (
    <div className="field">
      <label>{label}</label>

      {value.length > 0 && (
        <div className="ms-chips">
          {value.map((v) => (
            <span key={v} className="ms-chip">
              {byValue.get(v)?.label ?? v}
              <button type="button" aria-label={`Remove ${v}`} onClick={() => remove(v)}>×</button>
            </span>
          ))}
        </div>
      )}

      <div className="ms">
        <input
          className="in"
          value={q}
          placeholder={placeholder ?? "Search…"}
          role="combobox"
          aria-expanded={open}
          aria-autocomplete="list"
          onChange={(e) => { setQ(e.target.value); setOpen(true); }}
          onFocus={() => setOpen(true)}
          onBlur={() => window.setTimeout(() => setOpen(false), 150)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && matches[0]) { e.preventDefault(); add(matches[0].value); }
            else if (e.key === "Escape") setOpen(false);
            else if (e.key === "Backspace" && q === "" && value.length) remove(value[value.length - 1]);
          }}
        />
        {open && (
          <div className="ms-menu" role="listbox">
            {matches.length === 0
              ? <div className="ms-none">{needle ? "No match" : "Nothing left to add"}</div>
              : matches.slice(0, 60).map((o) => (
                  <button
                    type="button"
                    key={o.value}
                    className="ms-item"
                    role="option"
                    aria-selected={false}
                    onMouseDown={(e) => e.preventDefault()}
                    onClick={() => add(o.value)}
                  >
                    <span className="ms-val">{o.label ?? o.value}</span>
                    {o.hint && <span className="ms-hint">{o.hint}</span>}
                  </button>
                ))}
          </div>
        )}
      </div>

      {hint && <div className="hint">{hint}</div>}
    </div>
  );
}