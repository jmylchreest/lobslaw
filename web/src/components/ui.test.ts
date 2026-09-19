import { afterEach, describe, expect, it, vi } from "vitest";
import { when } from "./ui";

afterEach(() => vi.useRealTimers());

/** Freeze the clock so "ago" means something exact. */
function at(now: string) {
  vi.useFakeTimers();
  vi.setSystemTime(new Date(now));
}

describe("when", () => {
  it("does not label a future routine as just now", () => {
    at("2026-09-17T12:00:00Z");
    expect(when("2026-09-18T12:00:00Z")).not.toBe("just now");
    expect(when("2026-09-18T12:00:00Z")).not.toContain("ago");
  });
  it("answers the question a queue actually asks", () => {
    at("2026-09-17T12:00:00Z");
    expect(when("2026-09-17T11:59:57Z")).toBe("just now");
    expect(when("2026-09-17T11:59:30Z")).toBe("30s ago");
    expect(when("2026-09-17T11:56:00Z")).toBe("4m ago");
    expect(when("2026-09-17T09:00:00Z")).toBe("3h ago");
    expect(when("2026-09-15T12:00:00Z")).toBe("2d ago");
  });

  it("falls back to a date once 'ago' stops being useful", () => {
    at("2026-09-17T12:00:00Z");
    // A fortnight ago as a count of days is a number nobody reads.
    expect(when("2026-09-01T12:00:00Z")).toMatch(/\d/);
    expect(when("2026-09-01T12:00:00Z")).not.toContain("ago");
  });

  it("shows nothing for a missing timestamp rather than 'Invalid Date'", () => {
    at("2026-09-17T12:00:00Z");
    expect(when(undefined)).toBe("");
    expect(when("")).toBe("");
  });

  // An unparseable value comes back verbatim. A record with an odd
  // timestamp should show what it holds, not a formatter's opinion of
  // it — "Invalid Date" tells you nothing about what is in the field.
  it("passes an unparseable timestamp through unchanged", () => {
    at("2026-09-17T12:00:00Z");
    expect(when("not-a-date")).toBe("not-a-date");
  });
});
