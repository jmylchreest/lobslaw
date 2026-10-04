/** Per-bot identity.
 *
 * The single most useful thing this console does visually. A team
 * rendered as rows of text is a list you have to read; the same team
 * with a colour each is one you recognise — the same orange in the
 * sidebar, on the avatar, and down the edge of its queue rows.
 *
 * That only works if the colours are actually distinguishable, and the
 * first version was not. It multiplied a hash by the golden angle,
 * which reads like spreading and is nothing of the sort: golden-angle
 * stepping spreads consecutive INDICES, so applying it to an already
 * scattered hash just scatters it again. "coordinator" landed on hue 5
 * and "engineering" on hue 10 — the two bots you most need to tell
 * apart rendered the same orange, and the roster looked broken.
 *
 * So: quantise to twelve slots 30° apart, take the hash as a
 * PREFERENCE, and resolve collisions across the actual roster. A bot
 * keeps its colour unless another bot genuinely wants the same slot,
 * and in that case distinct beats stable — two identical oranges are
 * worse than one bot shifting a notch.
 */

const SLOTS = 12;
const STEP = 360 / SLOTS;

function hash(id: string): number {
  let h = 0;
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
  return h;
}

/** The slot an id would like, before anyone else is considered. */
function preferred(id: string): number {
  return hash(id) % SLOTS;
}

let assigned = new Map<string, number>();

/** Assign every bot a distinct, well-separated slot.
 *
 * Distinct is not enough. Avoiding exact collisions left "coordinator"
 * and "engineering" one slot apart — two blues you can tell apart if
 * you compare them and not if you glance, which is the only way anyone
 * reads a roster. So the target is a MINIMUM SEPARATION scaled to the
 * roster: five bots in twelve slots have no reason to sit adjacent.
 *
 * Sorted first so the result depends only on WHICH bots exist, never
 * on the order the API happened to return them — otherwise the roster
 * would recolour itself between two loads of the same page.
 */
export function setRoster(ids: string[]): void {
  const sorted = [...ids].sort();
  const next = new Map<string, number>();
  const taken: number[] = [];

  // The gap we can afford. Relaxed below when the roster outgrows it,
  // down to 1 (distinct) and finally 0 (slots genuinely exhausted past
  // twelve bots — at that size the mascot and the name do the work,
  // and pretending hue still identifies anyone would be a lie).
  const ideal = Math.max(1, Math.floor(SLOTS / Math.max(sorted.length, 1)));

  const distance = (a: number, b: number) => {
    const d = Math.abs(a - b) % SLOTS;
    return Math.min(d, SLOTS - d);
  };

  for (const id of sorted) {
    const want = preferred(id);
    let slot = -1;
    for (let gap = ideal; gap >= 0 && slot < 0; gap--) {
      // Probe outward from the preference so a displaced bot lands as
      // near as it can to where it wanted to be, deterministically.
      for (let d = 0; d < SLOTS; d++) {
        const cand = (want + d) % SLOTS;
        if (taken.every((t) => distance(t, cand) >= gap)) { slot = cand; break; }
      }
    }
    next.set(id, slot < 0 ? want : slot);
    taken.push(next.get(id)!);
  }
  assigned = next;
}

/** A bot's hue in degrees.
 *
 * Falls back to the raw preference for an id the roster has not seen —
 * a bot created thirty seconds ago still renders with an identity
 * rather than a placeholder grey.
 */
export function botHue(id: string): number {
  const slot = assigned.get(id) ?? preferred(id);
  // The half-step offset keeps slot 0 off pure red, which reads as an
  // error state next to the failure colour.
  return Math.round(slot * STEP + STEP / 2);
}

/** CSS custom properties a bot's elements read from.
 *
 * Saturation and lightness are fixed so every bot reads at the same
 * weight against the dark canvas — a hue landing on yellow must not
 * shout louder than one landing on blue.
 */
export function botVars(id: string): React.CSSProperties {
  const h = botHue(id);
  return {
    "--bot": `hsl(${h} 55% 45%)`,
    "--bot-fg": `hsl(${h} 75% 74%)`,
    "--bot-bg": `hsl(${h} 42% 13%)`,
    "--bot-br": `hsl(${h} 40% 30%)`,
    "--botfg": `hsl(${h} 75% 74%)`,
  } as React.CSSProperties;
}

/** Two characters at most: three is a word, and a word needs reading
 * rather than recognising. */
export function initials(name: string): string {
  const words = name.trim().split(/[\s\-_]+/).filter(Boolean);
  if (words.length === 0) return "?";
  if (words.length === 1) return words[0].slice(0, 2).toUpperCase();
  return (words[0][0] + words[1][0]).toUpperCase();
}
