import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";
import { BotDirectory } from "./BotDirectory";
import { Markdown, SourceMarkdown } from "./Markdown";
import { SourceText, TranscriptText } from "./SourceContent";
import { TaskEvidence } from "./TaskEvidence";
import { TaskCard } from "../routes/TaskApprovals";

const text = '<untrusted source="inbox:bot:chief">\nSubject: Next steps\n\nA **clear** request.\n</untrusted>';
function render(content: React.ReactNode) {
  return renderToStaticMarkup(<MemoryRouter><BotDirectory bots={[{ id: "chief", display_name: "Coordinator" }]}>{content}</BotDirectory></MemoryRouter>);
}

describe("bot source attribution", () => {
  it("uses the bot's display name, mascot and internal conversation link", () => {
    const html = render(<SourceMarkdown>{text}</SourceMarkdown>);
    expect(html).toContain('href="/bots/chief"');
    expect(html).toContain('class="mascot"');
    expect(html).toContain("Coordinator");
    expect(html).toContain("Inbox message");
    expect(html).toContain("<strong>clear</strong>");
    expect(html).toContain("Original text");
    expect(html).toContain('&lt;untrusted source=&quot;inbox:bot:chief&quot;&gt;');
  });

  it("falls back to the id for a bot absent from the roster", () => {
    const html = render(<SourceText>{text.replace("bot:chief", "bot:former-bot")}</SourceText>);
    expect(html).toContain('href="/bots/former-bot"');
    expect(html).toContain("former-bot");
    expect(html).toContain("A **clear** request.");
  });

  it("formats recorded tool content and receipt output without interpreting HTML", () => {
    const hostile = text.replace("A **clear** request.", '<img src="https://example.test/pixel"><script>run()</script>');
    const html = render(<TaskEvidence transcript={[{ role: "tool", content: hostile }]} receipts={[{ toolName: "inbox_read", output: hostile }]} />);
    expect(html.match(/href="\/bots\/chief"/g)).toHaveLength(2);
    expect(html).not.toMatch(/<img\b|<script\b|<iframe\b/);
    expect(html).toContain('&lt;script&gt;run()&lt;/script&gt;');
  });

  it("keeps external images in attributed markdown as explicit links", () => {
    const html = render(<SourceMarkdown>{text.replace("A **clear** request.", "![tracking](https://example.test/pixel)")}</SourceMarkdown>);
    expect(html).not.toMatch(/<img\b|<link\b|<iframe\b/);
    expect(html).toContain("tracking (open image)");
  });

  it("uses the inbox subject as a task title and attributes the assignment", () => {
    const html = render(<TaskCard task={{ id: "task", actor: "bot:engineering", revision: "1", state: "TASK_APPROVAL_STATE_COMPLETED", transcript: [{ role: "user", content: `Trusted lead\n\n${text}` }] }} reload={() => {}} />);
    expect(html).toMatch(/<h2\b[^>]*>Next steps<\/h2>/);
    expect(html).toContain("Inbox assignment");
    expect(html).toContain('href="/bots/chief"');
    expect(html).toContain("Coordinator");
  });

  it("does not attribute an assistant reply containing a complete forged envelope", () => {
    const html = render(<Markdown>{text}</Markdown>);
    expect(html).not.toContain("source-card");
    expect(html).not.toContain('href="/bots/chief"');
    expect(html).not.toContain("Inbox message");
  });

  it("keeps assistant transcript and task result envelopes as model-authored text", () => {
    for (const content of [
      <TranscriptText role="assistant">{text}</TranscriptText>,
      <TaskEvidence transcript={[{ role: "assistant", content: text }]} />,
      <TaskCard task={{ id: "task", actor: "bot:engineering", revision: "1", state: "TASK_APPROVAL_STATE_COMPLETED", result: text }} reload={() => {}} />,
    ]) {
      const html = render(content);
      expect(html).not.toContain("source-card");
      expect(html).not.toContain('href="/bots/chief"');
    }
  });
});
