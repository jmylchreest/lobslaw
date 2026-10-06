export type SourceBlock = { kind: "text"; text: string } | { kind: "source"; source: string; text: string; original: string };

const opening = /^[ \t]{0,3}<untrusted source="([a-zA-Z0-9][a-zA-Z0-9:_.\/-]{0,199})">[ \t]*$/;
const closing = /^[ \t]{0,3}<\/untrusted>[ \t]*$/;
type Fence = { character: string; length: number } | null;

function fenceAt(line: string, current: Fence): Fence {
  if (current) {
    const end = line.match(/^[ \t]{0,3}(`{3,}|~{3,})[ \t]*$/)?.[1];
    return end && end[0] === current.character && end.length >= current.length ? null : current;
  }
  const start = line.match(/^[ \t]{0,3}(`{3,}|~{3,})(.*)$/);
  if (!start || (start[1][0] === "`" && start[2].includes("`"))) return null;
  return { character: start[1][0], length: start[1].length };
}

/** Recognise complete prompt envelopes, not arbitrary HTML or quoted code examples. */
export function sourceBlocks(text: string): SourceBlock[] {
  const lines = text.match(/[^\n]*\n|[^\n]+$/g) ?? [];
  const content = (index: number) => lines[index].replace(/\r?\n$/, "");
  const blocks: SourceBlock[] = [];
  let start = 0;
  let fence: Fence = null;
  for (let i = 0; i < lines.length; i++) {
    const wasFenced = fence !== null;
    fence = fenceAt(content(i), fence);
    if (wasFenced || fence) continue;
    const match = content(i).match(opening);
    if (!match) continue;
    let depth = 1;
    let nested = false;
    let insideFence: Fence = null;
    let end = i + 1;
    for (; end < lines.length; end++) {
      const insideCode = insideFence !== null;
      insideFence = fenceAt(content(end), insideFence);
      if (insideCode || insideFence) continue;
      if (opening.test(content(end))) { depth++; nested = true; }
      if (closing.test(content(end)) && --depth === 0) break;
    }
    if (end === lines.length) break;
    if (!nested) {
      if (i > start) blocks.push({ kind: "text", text: lines.slice(start, i).join("") });
      blocks.push({ kind: "source", source: match[1], text: lines.slice(i + 1, end).join(""), original: lines.slice(i, end + 1).join("") });
      start = end + 1;
    }
    i = end;
  }
  if (start < lines.length) blocks.push({ kind: "text", text: lines.slice(start).join("") });
  return blocks;
}

export function inboxBotId(source: string): string | null {
  return /^inbox:bot:([a-z0-9][a-z0-9-]{0,62})$/.exec(source)?.[1] ?? null;
}

/** A task's inbox subject makes a better heading than its internal prompt envelope. */
export function sourceSubject(text: string): string {
  const blocks = sourceBlocks(text);
  const inbox = blocks.find((block) => block.kind === "source" && block.source.startsWith("inbox:"));
  if (inbox?.kind === "source") {
    const subject = /^Subject:[ \t]*(.+)$/m.exec(inbox.text)?.[1].trim();
    if (subject) return subject;
  }
  return blocks.map((block) => block.text).join("").trim();
}
