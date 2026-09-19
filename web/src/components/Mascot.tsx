import { botHue } from "../theme";

/** A bot's face.
 *
 * Initials in a circle are a label; a character is something you
 * recognise across a room. That difference is most of what separated
 * this console from looking like a product — a roster of "EN" "MA"
 * "DE" reads as a table of abbreviations, and the same roster as
 * little creatures reads as a team.
 *
 * Generated from the id rather than stored: shape and colour both fall
 * out of the same hash, so a bot created thirty seconds ago already
 * has a face, and there is no asset to upload, migrate or leave unset.
 */

// Six silhouettes on a 100×100 grid. Deliberately simple and very
// different from each other — at 26px in a sidebar, subtle variation
// is no variation, and two bots that look alike defeat the point.
const SHAPES = [
  // droplet
  "M50 4 C50 4 88 46 88 66 A38 38 0 0 1 12 66 C12 46 50 4 50 4 Z",
  // squircle
  "M50 6 C82 6 94 18 94 50 C94 82 82 94 50 94 C18 94 6 82 6 50 C6 18 18 6 50 6 Z",
  // arch — flat base, domed top
  "M12 92 L12 50 A38 38 0 0 1 88 50 L88 92 A6 6 0 0 1 82 98 L18 98 A6 6 0 0 1 12 92 Z",
  // rounded triangle
  "M50 8 C56 8 60 12 62 16 L92 74 C96 82 92 92 82 92 L18 92 C8 92 4 82 8 74 L38 16 C40 12 44 8 50 8 Z",
  // hexagon
  "M50 4 L88 26 L88 74 L50 96 L12 74 L12 26 Z",
  // blob — the least regular of the set
  "M50 6 C74 2 96 20 94 46 C92 72 76 96 50 96 C24 96 6 74 8 48 C10 22 26 10 50 6 Z",
];

function pick(id: string): number {
  let h = 0;
  for (let i = 0; i < id.length; i++) h = (h * 33 + id.charCodeAt(i)) >>> 0;
  return h % SHAPES.length;
}

export function Mascot({ id, size = 30, dim }: { id: string; size?: number; dim?: boolean }) {
  const hue = botHue(id);
  const shape = SHAPES[pick(id)];
  // Eyes sit low and wide. High, close-set eyes read as anxious; low
  // and wide reads as calm, which is what you want looking back at you
  // from a list of things doing work on your behalf.
  const eye = Math.max(2.2, size * 0.075);

  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 100 100"
      style={{ flexShrink: 0, opacity: dim ? 0.38 : 1, display: "block" }}
      aria-hidden="true"
    >
      <defs>
        <linearGradient id={`g-${id}`} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor={`hsl(${hue} 85% 66%)`} />
          <stop offset="100%" stopColor={`hsl(${hue} 78% 52%)`} />
        </linearGradient>
      </defs>
      <path d={shape} fill={`url(#g-${id})`} />
      {/* Pupils only, no whites: at sidebar size the white ring closes
          up into a grey smudge and the face stops reading as a face. */}
      <ellipse cx={38} cy={58} rx={eye * 100 / size} ry={eye * 130 / size} fill="#0b0b0d" />
      <ellipse cx={62} cy={58} rx={eye * 100 / size} ry={eye * 130 / size} fill="#0b0b0d" />
    </svg>
  );
}
