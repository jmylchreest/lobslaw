import { describe, expect, it } from "vitest";
import { fileMediaType, formatFileSize } from "./uploads";

const policy = { enabled: true, max_bytes: 33554432, max_files: 16, media_types: ["image/png", "image/jpeg", "application/pdf", "text/plain", "text/markdown", "text/csv", "application/json"] };
describe("attachment file validation", () => {
  it("uses a supported MIME type and infers missing generic types from known extensions", () => {
    expect(fileMediaType({ name: "photo.png", type: "image/png" }, policy)).toBe("image/png");
    expect(fileMediaType({ name: "REPORT.PDF", type: "application/octet-stream" }, policy)).toBe("application/pdf");
    expect(fileMediaType({ name: "notes.md", type: "" }, policy)).toBe("text/markdown");
    expect(fileMediaType({ name: "data.csv", type: "text/csv;charset=utf-8" }, policy)).toBe("text/csv");
  });
  it("rejects unsupported active formats rather than trusting a renamed extension", () => {
    for (const file of [{ name: "tracking.svg", type: "image/svg+xml" }, { name: "page.txt", type: "text/html" }, { name: "program.exe", type: "application/octet-stream" }, { name: "unknown.bin", type: "" }]) expect(fileMediaType(file, policy)).toBeNull();
    expect(fileMediaType({ name: "photo.webp", type: "image/webp" }, policy)).toBeNull();
  });
  it("formats bounded file sizes for the preview", () => {
    expect(formatFileSize(32)).toBe("32 B");
    expect(formatFileSize(2048)).toBe("2 KB");
    expect(formatFileSize(32 * 1024 * 1024)).toBe("32.0 MB");
  });
});
