import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { SourceContent } from "./SourceContent";

/** Model output is markdown, and we were printing it raw.
 *
 * With a stub LLM returning one hand-written sentence nobody noticed.
 * A real model answers "who should review our indexes" with a TABLE,
 * and `white-space: pre-wrap` renders that as a wall of pipes —
 * unreadable on a desktop and considerably worse on a 390px phone.
 *
 * react-markdown builds React elements rather than setting innerHTML,
 * so model output cannot inject markup into the console. That matters
 * more than usual here: a bot's reply can contain text it fetched from
 * a web page, so this string is not necessarily the model's own words.
 */
export function Markdown({ children }: { children: string }) {
  return (
    <div className="md">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          // Even an image on our origin can be an action URL. Model-authored
          // sources are links only: rendering a reply must not make requests.
          img: ({ src, alt }) => <a href={src} target="_blank" rel="noopener noreferrer nofollow">{alt || "Image"} (open image)</a>,
          // Tables are the main offender on a phone: too wide to fit
          // and nothing sensible to wrap on. Give each its own
          // horizontal scroller so the table slides instead of the
          // whole page.
          table: ({ children }) => <div className="md-tw"><table>{children}</table></div>,
          // Anything a model links to is external and untrusted.
          a: ({ href, children }) => (
            <a href={href} target="_blank" rel="noopener noreferrer nofollow">{children}</a>
          ),
        }}
      >
        {children}
      </ReactMarkdown>
    </div>
  );
}

/** Attribution is opt-in for backend-wrapped context. Model-authored Markdown
 * must never be able to mint source cards by emitting envelope-shaped text. */
export function SourceMarkdown({ children }: { children: string }) {
  return <SourceContent text={children} render={(text) => <Markdown>{text}</Markdown>} />;
}
