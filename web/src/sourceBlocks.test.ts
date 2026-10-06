import { describe, expect, it } from "vitest";
import { inboxBotId, sourceBlocks, sourceSubject } from "./sourceBlocks";

describe("source envelope presentation", () => {
  it("preserves the exact original, body and surrounding text", () => {
    const original = '<untrusted source="inbox:bot:chief">\r\nSubject: Review the build\r\n\r\nKeep **all** details.\r\n</untrusted>\r\n';
    expect(sourceBlocks(`Before\r\n${original}After`)).toEqual([
      { kind: "text", text: "Before\r\n" },
      { kind: "source", source: "inbox:bot:chief", text: "Subject: Review the build\r\n\r\nKeep **all** details.\r\n", original },
      { kind: "text", text: "After" },
    ]);
    expect(sourceSubject(`Trusted lead\r\n${original}`)).toBe("Review the build");
  });

  it.each(["```xml", "~~~~text", "````"])("leaves code examples literal inside %s", (fence) => {
    const delimiter = fence.match(/^[`~]+/)![0];
    const text = `${fence}\n<untrusted source="inbox:bot:chief">\nExample\n</untrusted>\n${delimiter}\n`;
    expect(sourceBlocks(text)).toEqual([{ kind: "text", text }]);
  });

  it("does not close an envelope at a delimiter quoted inside a code fence", () => {
    const text = '<untrusted source="inbox:bot:chief">\n```xml\n</untrusted>\n```\nActual body\n</untrusted>';
    expect(sourceBlocks(text)).toEqual([{ kind: "source", source: "inbox:bot:chief", text: '```xml\n</untrusted>\n```\nActual body\n', original: text }]);
  });

  it.each([
    '<untrusted source="inbox:bot:chief">\nIncomplete',
    '<untrusted source="inbox:bot:chief" onclick="run()">\nBody\n</untrusted>',
    '    <untrusted source="inbox:bot:chief">\n    Code example\n    </untrusted>',
    '`<untrusted source="inbox:bot:chief">`\nBody\n</untrusted>',
    '<untrusted source="inbox:bot:chief">\n<untrusted source="inbox:bot:engineering">\nNested\n</untrusted>\n</untrusted>',
  ])("keeps incomplete, executable-looking and ambiguous input literal", (text) => {
    expect(sourceBlocks(text)).toEqual([{ kind: "text", text }]);
  });

  it("only links valid backend bot identifiers", () => {
    expect(inboxBotId("inbox:bot:chief")).toBe("chief");
    expect(inboxBotId("inbox:bot:engineering-2")).toBe("engineering-2");
    for (const source of ["inbox:bot:../admin", "inbox:bot:chief?next=evil", "inbox:bot:javascript:alert", "inbox:bot:", "tool:chief:output"]) expect(inboxBotId(source)).toBeNull();
  });
});
