import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { botReply, streamBotChat, type TaskMessage, type ToolReceipt } from "../api";
import { useFeedback } from "./Motion";

export interface ChatMessage {
  kind: "said"; id: string; from: "me" | "bot"; text: string; at: number;
  notice?: boolean; animate?: boolean; pending?: boolean;
  tools?: string[]; attempts?: string[]; tokens?: number; cost?: number; sessionId?: string;
  transcript?: TaskMessage[]; receipts?: ToolReceipt[];
}
interface ChatSession {
  messages: ChatMessage[]; historyLoaded: boolean; busy: boolean; working: boolean; draft: string;
  partial: string; liveReply: { id: string; at: number } | null;
  notice: string; error: Error | null;
  ask: { id: string; reason: string; action?: string; resource?: string } | null;
}
const empty: ChatSession = { messages: [], historyLoaded: false, busy: false, working: false, draft: "", partial: "", liveReply: null, notice: "", error: null, ask: null };
const ChatContext = createContext<{
  sessions: Record<string, ChatSession>;
  send: (botId: string, text: string, name: string) => Promise<void>;
  stop: (botId: string) => void;
  history: (botId: string, messages: ChatMessage[]) => void;
  answered: (botId: string) => void;
  draft: (botId: string, value: string) => void;
} | null>(null);

/** Requests belong to the signed-in console, not to whichever route is visible. */
export function ChatSessionsProvider({ children }: { children: ReactNode }) {
  const [sessions, setSessions] = useState<Record<string, ChatSession>>({});
  const controllers = useRef(new Map<string, AbortController>());
  const sequence = useRef(0);
  const mounted = useRef(true);
  const notify = useFeedback();
  const update = useCallback((id: string, fn: (session: ChatSession) => ChatSession) => {
    if (mounted.current) setSessions((current) => ({ ...current, [id]: fn(current[id] ?? empty) }));
  }, []);
  useEffect(() => {
    mounted.current = true;
    const active = controllers.current;
    return () => { mounted.current = false; for (const controller of active.values()) controller.abort(); active.clear(); };
  }, []);

  const send = useCallback(async (botId: string, text: string, name: string) => {
    if (!text.trim() || controllers.current.has(botId)) return;
    const controller = new AbortController();
    controllers.current.set(botId, controller);
    const id = ++sequence.current;
    const at = Date.now();
    const replyId = `reply-${id}`;
    update(botId, (s) => ({ ...s, busy: true, working: true, draft: "", partial: "", ask: null, error: null, notice: "Sending your message",
      liveReply: { id: replyId, at: at + 1 }, messages: [...s.messages, { kind: "said", id: `message-${id}`, from: "me", text, at, animate: true }] }));
    let terminal = false;
    try {
      await streamBotChat(botId, text, (event, data) => {
        if (!mounted.current || controllers.current.get(botId) !== controller) return;
        if (event === "accepted") {
          terminal = true;
          const message = String(data.message ?? "Your message joined an active turn. Refresh the conversation to see its reply.");
          update(botId, (s) => ({ ...s, notice: message, messages: [...s.messages, { kind: "said", id: replyId, from: "bot", text: message, at: at + 1, notice: true }] }));
        }
        if (event === "start" || event === "working") update(botId, (s) => ({ ...s, working: true, notice: `${name} is working on your reply` }));
        if (event === "delta") update(botId, (s) => ({ ...s, working: false, partial: s.partial + String(data.text ?? "") }));
        if (event === "needs_confirmation") {
          update(botId, (s) => ({ ...s, notice: `${name} needs your approval to continue.`, ask: {
            id: String(data.prompt_id ?? data.promptId ?? ""), reason: String(data.reason ?? ""), action: data.action ? String(data.action) : undefined, resource: data.resource ? String(data.resource) : undefined,
          } }));
          if (window.location.pathname !== `/bots/${encodeURIComponent(botId)}`) notify(`${name} needs your approval`);
        }
        if (event === "error") { terminal = true; throw new Error(String(data.message ?? data.error ?? "The turn failed")); }
        if (event === "reply") {
          terminal = true;
          const reply = botReply(data);
          if (!reply.text?.trim()) throw new Error("The node finished without a reply. Refresh the conversation or try again.");
          update(botId, (s) => ({ ...s, working: false, partial: "", ask: null, notice: `${name} replied.`, messages: [...s.messages, {
            kind: "said", id: replyId, from: "bot", text: reply.text!, at: at + 1, animate: true,
            tools: reply.toolsUsed ?? [], attempts: reply.toolsAttempted ?? [], tokens: Number(reply.tokensUsed ?? 0), cost: Number(reply.costUsd ?? 0),
            sessionId: reply.sessionId, transcript: reply.transcript, receipts: reply.receipts,
          }] }));
          if (window.location.pathname !== `/bots/${encodeURIComponent(botId)}`) notify(`${name} replied`);
        }
      }, controller.signal);
      if (!terminal) throw new Error("The connection closed before a reply arrived. Your message is still in this conversation; you can try again.");
    } catch (error) {
      if (!mounted.current) return;
      if (controller.signal.aborted) {
        update(botId, (s) => ({ ...s, notice: "Response stopped.", messages: [...s.messages, { kind: "said", id: replyId, from: "bot", text: "Response stopped.", notice: true, at: at + 1 }] }));
      } else {
        update(botId, (s) => ({ ...s, error: error as Error, notice: "The reply was interrupted." }));
        if (window.location.pathname !== `/bots/${encodeURIComponent(botId)}`) notify(`${name}'s reply was interrupted`);
      }
    } finally {
      if (controllers.current.get(botId) === controller) controllers.current.delete(botId);
      update(botId, (s) => ({ ...s, busy: false, working: false, partial: "", ask: null, liveReply: null }));
      if (mounted.current) window.dispatchEvent(new CustomEvent("lobslaw:chat-completed", { detail: botId }));
    }
  }, [update, notify]);
  const history = useCallback((botId: string, messages: ChatMessage[]) => update(botId, (s) => {
    if (s.historyLoaded) return s;
    return { ...s, historyLoaded: true, messages: [...messages, ...s.messages] };
  }), [update]);
  const answered = useCallback((botId: string) => update(botId, (s) => ({ ...s, ask: null })), [update]);
  const stop = useCallback((botId: string) => controllers.current.get(botId)?.abort(), []);
  const draft = useCallback((botId: string, value: string) => update(botId, (s) => ({ ...s, draft: value })), [update]);
  return <ChatContext.Provider value={{ sessions, send, stop, history, answered, draft }}>{children}</ChatContext.Provider>;
}

export function useChatSessions() {
  const value = useContext(ChatContext);
  if (!value) throw new Error("ChatSessionsProvider is required");
  return value;
}
export function useBotChat(botId: string) {
  const value = useChatSessions();
  return { ...value, session: value.sessions[botId] ?? empty };
}
