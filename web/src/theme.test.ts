import { describe, expect, it } from "vitest";
import { botHue, initials, setRoster } from "./theme";

/** Shortest distance between two hues on the colour wheel. */
function apart(a: number, b: number): number {
  const d = Math.abs(a - b) % 360;
  return Math.min(d, 360 - d);
}

describe("bot colours", () => {
  // The first version multiplied a hash by the golden angle, which
  // reads like spreading and is nothing of the sort: golden-angle
  // stepping spreads consecutive INDICES. "coordinator" landed on hue
  // 5 and "engineering" on 10 — the two bots you most need to tell
  // apart rendered the same orange.
  //
  // This was checked once with a throwaway script. It should have been
  // this.
  it("gives a real roster visibly different colours", () => {
    const roster = ["coordinator", "devops", "engineering", "marketing", "research"];
    setRoster(roster);
    const hues = roster.map(botHue);

    for (let i = 0; i < hues.length; i++) {
      for (let j = i + 1; j < hues.length; j++) {
        expect(
          apart(hues[i], hues[j]),
          `${roster[i]} and ${roster[j]} are ${apart(hues[i], hues[j])}° apart`,
        ).toBeGreaterThanOrEqual(30);
      }
    }
  });

  it("never gives two bots the same colour, up to the number of slots", () => {
    for (let n = 1; n <= 12; n++) {
      const ids = Array.from({ length: n }, (_, i) => `bot-${i}-team`);
      setRoster(ids);
      const hues = ids.map(botHue);
      expect(new Set(hues).size, `roster of ${n} produced a collision`).toBe(n);
    }
  });

  // Past twelve the slots genuinely run out. Asserted so the limit is
  // a known property rather than a surprise: at that size the mascot
  // and the name do the work, and pretending otherwise would be a lie.
  it("runs out of distinct colours past twelve, rather than misbehaving", () => {
    const ids = Array.from({ length: 16 }, (_, i) => `bot-${i}-team`);
    setRoster(ids);
    const hues = ids.map(botHue);
    expect(new Set(hues).size).toBeLessThan(ids.length);
    hues.forEach((h) => {
      expect(h).toBeGreaterThanOrEqual(0);
      expect(h).toBeLessThan(360);
    });
  });

  it("is stable across calls, so a bot does not change colour on a reload", () => {
    const roster = ["alpha", "beta", "gamma"];
    setRoster(roster);
    const first = roster.map(botHue);
    setRoster(roster);
    expect(roster.map(botHue)).toEqual(first);
  });

  // The assignment sorts internally, so it depends only on WHICH bots
  // exist — not on the order the API happened to return them. Without
  // that the roster recolours itself between two loads of one page.
  it("does not depend on the order the roster arrives in", () => {
    setRoster(["alpha", "beta", "gamma"]);
    const forward = ["alpha", "beta", "gamma"].map(botHue);
    setRoster(["gamma", "alpha", "beta"]);
    expect(["alpha", "beta", "gamma"].map(botHue)).toEqual(forward);
  });

  it("gives an unknown bot a colour rather than a placeholder", () => {
    setRoster(["alpha"]);
    // A bot created thirty seconds ago is not in the roster yet.
    const h = botHue("brand-new");
    expect(Number.isFinite(h)).toBe(true);
    expect(h).toBeGreaterThanOrEqual(0);
    expect(h).toBeLessThan(360);
  });
});

describe("initials", () => {
  it("takes at most two characters", () => {
    // Three is a word, and a word needs reading rather than recognising.
    expect(initials("Engineering")).toBe("EN");
    expect(initials("Dev Ops")).toBe("DO");
    expect(initials("site-reliability-engineering")).toBe("SR");
  });

  it("does not fall over on nothing", () => {
    expect(initials("")).toBe("?");
    expect(initials("   ")).toBe("?");
  });
});
