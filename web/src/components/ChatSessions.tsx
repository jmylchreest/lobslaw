import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { ApiError, api, botReply, type ChatTurn, type TaskMessage, type ToolReceipt } from "../api";
import { useFeedback } from "./Motion";
import type { MessageFile } from "../uploads";
import { useUploadAcceptance } from "./Uploads";

export interface ChatMessage {
  kind: "said"; id: string; from: "me" | "bot"; text: string; at: number;
  notice?: boolean; animate?: boolean; pending?: boolean; recovered?: boolean;
  tools?: string[]; attempts?: string[]; tokens?: number; cost?: number; sessionId?: string;
  transcript?: TaskMessage[]; receipts?: ToolReceipt[];
  files?: MessageFile[];
}
interface ChatSession {
  messages: ChatMessage[]; historyLoaded: boolean; busy: boolean; working: boolean; draft: string;
  partial: string; liveReply: { id: string; at: number } | null; turnId: string | null;
  notice: string; error: Error | null; reconnecting: boolean;
  ask: { id: string; reason: string; action?: string; resource?: string } | null;
}
const empty: ChatSession = { messages: [], historyLoaded: false, busy: false, working: false, draft: "", partial: "", liveReply: null, turnId: null, notice: "", error: null, ask: null, reconnecting: false };
const ChatContext = createContext<{
  sessions: Record<string, ChatSession>;
  send: (botId: string, text: string, name: string, uploadIDs?: string[], files?: MessageFile[]) => Promise<void>;
  stop: (botId: string) => void;
  recover: (botId: string, name: string) => Promise<void>;
  history: (botId: string, messages: ChatMessage[]) => void;
  answered: (botId: string) => void;
  draft: (botId: string, value: string) => void;
} | null>(null);

function requestID() {
  const bytes = new Uint8Array(16); crypto.getRandomValues(bytes);
  return `chat-${Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}
const pause = (signal: AbortSignal) => new Promise<void>((resolve) => {
  const done = () => { clearTimeout(timer); signal.removeEventListener("abort", done); resolve(); };
  const timer = window.setTimeout(done, 1200); signal.addEventListener("abort", done, { once: true });
});

/** Browser connections observe server-owned turns. Unmounting only stops polling. */
export function ChatSessionsProvider({ children }: { children: ReactNode }) {
  const [sessions, setSessions] = useState<Record<string, ChatSession>>({});
  const sessionsRef = useRef(sessions); sessionsRef.current = sessions;
  const watchers = useRef(new Map<string, AbortController>());
  const discovered = useRef(new Set<string>());
  const mounted = useRef(true);
  const notify = useFeedback();
  const acceptUploads = useUploadAcceptance();
  const update = useCallback((id: string, fn: (session: ChatSession) => ChatSession) => {
    if (mounted.current) setSessions((current) => ({ ...current, [id]: fn(current[id] ?? empty) }));
  }, []);
  useEffect(() => {
    mounted.current = true;
    const active = watchers.current;
    return () => { mounted.current = false; for (const controller of active.values()) controller.abort(); active.clear(); };
  }, []);

  const apply = useCallback((botId: string, turn: ChatTurn, recovered: boolean) => {
    const at = new Date(turn.created_at).getTime();
    const replyId = `reply-${turn.id}`;
    update(botId, (s) => {
      let messages = s.messages;
      const data = turn.data ?? {};
      const recordedReply = String(data.text ?? data.reply ?? "");
      const recordedPair = recovered && s.historyLoaded && turn.state === "completed" && messages.at(-2)?.from === "me" && messages.at(-2)?.text === turn.message && messages.at(-1)?.from === "bot" && messages.at(-1)?.text === recordedReply;
      if (!messages.some((message) => message.id === `message-${turn.id}`) && !recordedPair) {
        messages = [...messages, { kind: "said", id: `message-${turn.id}`, from: "me", text: turn.message, at, recovered, animate: !recovered, files: turn.files }];
      }
      const active = turn.state === "running" || turn.state === "waiting";
      const common = { ...s, messages, turnId: turn.id, busy: active, working: active, reconnecting: false, error: null,
        liveReply: active ? { id: replyId, at: at + 1 } : null };
      if (turn.event === "needs_confirmation" && active) return { ...common, notice: "Your approval is needed to continue.", ask: {
        id: String(data.promptId ?? data.prompt_id ?? ""), reason: String(data.reason ?? ""), action: String(data.action ?? ""), resource: String(data.resource ?? ""),
      } };
      if (active) return { ...common, ask: null, partial: turn.event === "delta" ? String(data.text ?? "") : s.partial, notice: "Waiting for your reply. You can safely close this tab." };
      if (turn.event === "reply" || turn.event === "final") {
        const reply = botReply(data);
        const text = String(reply.text ?? data.reply ?? "");
        if (!text.trim()) return { ...common, ask: null, error: new Error("The node finished without a reply. Inspect the conversation before sending again.") };
        const last = messages.at(-1);
        if (!messages.some((m) => m.id === replyId) && !(recordedPair && last?.text === text)) {
          messages = [...messages, { kind: "said", id: replyId, from: "bot", text, at: at + 1, recovered, animate: !recovered,
            tools: reply.toolsUsed, attempts: reply.toolsAttempted, tokens: Number(reply.tokensUsed ?? 0), cost: Number(reply.costUsd ?? 0),
            sessionId: reply.sessionId, transcript: reply.transcript, receipts: reply.receipts }];
        }
        return { ...common, messages, partial: "", ask: null, notice: "Reply received." };
      }
      if (turn.event === "accepted") return { ...common, ask: null, notice: String(data.message ?? "Message joined an active turn."), messages: [...messages.filter((m) => m.id !== replyId), { kind: "said", id: replyId, from: "bot", text: String(data.message ?? "Message accepted."), at: at + 1, notice: true }] };
      return { ...common, ask: null, notice: "Reply interrupted.", error: new Error(String(data.message ?? "The reply was interrupted. Review the conversation before sending again.")) };
    });
  }, [update]);

  const watch = useCallback(async (botId: string, initial: ChatTurn, name: string, controller: AbortController, recovered: boolean) => {
    let turn = initial;
    let displayed = "";
    while (!controller.signal.aborted && mounted.current) {
      const version = `${turn.updated_at}:${turn.state}`;
      if (displayed !== version) { apply(botId, turn, recovered); displayed = version; }
      if (!["running", "waiting"].includes(turn.state)) {
        if (!recovered && window.location.pathname !== `/bots/${encodeURIComponent(botId)}`) notify(turn.state === "completed" ? `${name} replied` : `${name}'s reply was interrupted`);
        window.dispatchEvent(new CustomEvent("lobslaw:chat-completed", { detail: botId }));
        break;
      }
      await pause(controller.signal);
      if (controller.signal.aborted) break;
      try { turn = await api.chatTurn(turn.id); update(botId, (s) => s.reconnecting ? { ...s, reconnecting: false } : s); }
      catch (error) {
        if (!mounted.current) break;
        if (error instanceof ApiError && [401, 403, 404].includes(error.status)) { update(botId, (s) => ({ ...s, busy: false, liveReply: null, error, reconnecting: false })); break; }
        update(botId, (s) => ({ ...s, reconnecting: true, notice: "Reconnecting… your reply continues on the server." }));
      }
    }
    if (watchers.current.get(botId) === controller) watchers.current.delete(botId);
  }, [apply, notify, update]);

  const recover = useCallback(async (botId: string, name: string) => {
    if (discovered.current.has(botId) || watchers.current.has(botId)) return;
    discovered.current.add(botId);
    try {
      const { turn } = await api.latestChatTurn(botId);
      if (!turn || !mounted.current || watchers.current.has(botId)) return;
      const controller = new AbortController(); watchers.current.set(botId, controller);
      await watch(botId, turn, name, controller, true);
    } catch { discovered.current.delete(botId); }
  }, [watch]);

  const send = useCallback(async (botId: string, text: string, name: string, uploadIDs: string[] = [], files: MessageFile[] = []) => {
    if ((!text.trim() && !uploadIDs.length) || watchers.current.has(botId)) return;
    const controller = new AbortController(); watchers.current.set(botId, controller);
    update(botId, (s) => ({ ...s, busy: true, working: true, error: null, notice: "Sending your message…" }));
    const id = requestID();
    try {
      // Retrying the same id recovers a lost creation response, never a second turn.
      let turn: ChatTurn;
      try { turn = await api.createChatTurn(id, botId, text, undefined, uploadIDs); }
      catch (error) {
        if (error instanceof ApiError && error.status === 409) {
          const latest = await api.latestChatTurn(botId);
          if (!latest.turn || !["running", "waiting"].includes(latest.turn.state)) throw error;
          await watch(botId, latest.turn, name, controller, true); return;
        }
        if (error instanceof ApiError) throw error;
        try { turn = await api.createChatTurn(id, botId, text, undefined, uploadIDs); }
        catch { turn = await api.chatTurn(id); }
      }
      if (!mounted.current) return;
      acceptUploads(botId ? `bot:${botId}` : "assistant", uploadIDs);
      update(botId, (s) => ({ ...s, draft: "", messages: [...s.messages, { kind: "said", id: `message-${turn.id}`, from: "me", text, at: Date.parse(turn.created_at), animate: true, files }] }));
      await watch(botId, turn, name, controller, false);
    } catch (error) {
      if (mounted.current) update(botId, (s) => ({ ...s, busy: false, liveReply: null, error: error as Error }));
    } finally { if (watchers.current.get(botId) === controller) watchers.current.delete(botId); }
  }, [update, watch, acceptUploads]);

  const stop = useCallback((botId: string) => {
    const id = sessionsRef.current[botId]?.turnId;
    if (!id) return;
    void api.stopChatTurn(id).then((turn) => apply(botId, turn, false)).catch((error: Error) => update(botId, (s) => ({ ...s, error })));
  }, [apply, update]);
  const history = useCallback((botId: string, messages: ChatMessage[]) => update(botId, (s) => {
    if (s.historyLoaded) return s;
    const recovered = s.messages.filter((message) => message.recovered);
    const recorded = recovered.length === 2 && messages.at(-2)?.from === "me" && messages.at(-2)?.text === recovered[0].text && messages.at(-1)?.from === "bot" && messages.at(-1)?.text === recovered[1].text;
    return { ...s, historyLoaded: true, messages: [...messages, ...s.messages.filter((message) => !recorded || !message.recovered)] };
  }), [update]);
  const answered = useCallback((botId: string) => update(botId, (s) => ({ ...s, ask: null })), [update]);
  const draft = useCallback((botId: string, value: string) => update(botId, (s) => ({ ...s, draft: value })), [update]);
  return <ChatContext.Provider value={{ sessions, send, stop, recover, history, answered, draft }}>{children}</ChatContext.Provider>;
}

export function useChatSessions() {
  const value = useContext(ChatContext);
  if (!value) throw new Error("ChatSessionsProvider is required");
  return value;
}
export function useBotChat(botId: string) {
  const value = useChatSessions();
  useEffect(() => { void value.recover(botId, botId); }, [botId, value.recover]);
  return { ...value, session: value.sessions[botId] ?? empty };
}
