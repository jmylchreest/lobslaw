import { useMemo, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { inboxBotId, sourceBlocks } from "../sourceBlocks";
import { botVars } from "../theme";
import { useBotName } from "./BotDirectory";
import { Mascot } from "./Mascot";
import { Disclosure } from "./Motion";

export function BotSource({ id }: { id: string }) {
  const name = useBotName(id);
  return <Link className="bot-source-link" to={`/bots/${encodeURIComponent(id)}`} style={botVars(id)} title={`Open ${name}'s conversation`}>
    <Mascot id={id} size={24} /><span>{name}</span><svg className="source-arrow" viewBox="0 0 12 12" aria-hidden="true"><path d="M2 6h8M7 3l3 3-3 3" /></svg>
  </Link>;
}

export function SourceContent({ text, render }: { text: string; render: (text: string) => ReactNode }) {
  const blocks = useMemo(() => sourceBlocks(text), [text]);
  if (!blocks.some((block) => block.kind === "source")) return <>{render(text)}</>;
  return <div className="source-content">{blocks.map((block, index) => {
    if (block.kind === "text") return block.text.trim() ? <div key={index}>{render(block.text)}</div> : null;
    const botId = inboxBotId(block.source);
    const tool = /^tool:([^:]+)(?::(.*))?$/.exec(block.source);
    return <section key={index} className={`source-card${botId ? " inbox-source" : ""}`} style={botId ? botVars(botId) : undefined} data-source={block.source}>
      <div className="source-heading">{botId ? <><span className="source-caption">From</span><BotSource id={botId} /><span className="source-kind">Inbox message</span></>
        : <><span className="source-caption">{tool ? "Tool output" : "Quoted content"}</span><span className="source-name">{tool ? tool[1] : block.source}</span></>}</div>
      <div className="source-body">{render(block.text)}</div>
      <Disclosure className="source-original" title="Original text"><pre>{block.original}</pre></Disclosure>
    </section>;
  })}</div>;
}

export function SourceText({ children }: { children: string }) {
  return <SourceContent text={children} render={(text) => <pre className="source-literal">{text}</pre>} />;
}

export function TranscriptText({ role, children }: { role?: string; children: string }) {
  return role === "user" || role === "tool" || role === "system"
    ? <SourceText>{children}</SourceText>
    : <pre className="source-literal">{children}</pre>;
}
