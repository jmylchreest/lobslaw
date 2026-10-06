// Protobuf is the source of shared contracts. REST uses snake_case for legacy
// resources and protobuf JSON for task/review evidence; adapters below describe
// those deliberate public projections without exposing private task fields.
import type {
 ConsoleBotJson, ConsoleGroupJson, ConsoleRoutineJson, ConsoleMemoryRecordJson,
 ConsoleInboxItemJson, ConsoleToolJson, ConsoleCapabilityJson, ConsoleSessionJson,
 ConsoleMessageJson, ConsoleLearnedChangeJson, ConsoleLearnedReviewJson,
 TaskApprovalRecordJson, SessionMessageJson, TurnToolInvocationJson, ConsoleBotReplyJson,
 InboxStatusJson, InboxKindJson,
} from "./gen/lobslaw/v1/lobslaw_pb";

import { clearLocalPush } from "./pushBinding";

type Snake<S extends string> = S extends `${infer H}${infer T}`
 ? `${H extends Lowercase<H> ? H : `_${Lowercase<H>}`}${Snake<T>}` : S;
type REST<T> = { [K in keyof T as K extends string ? Snake<K> : K]: T[K] };
type Require<T, K extends keyof T> = T & Required<Pick<T, K>>;
type LegacyInt64 = string | number;
type EnumSuffix<T, P extends string> = T extends `${P}${infer V}`
 ? V extends "UNSPECIFIED" ? never : Lowercase<V> : never;
export type BotStatus = EnumSuffix<InboxStatusJson, "INBOX_STATUS_">;
export type InboxKind = EnumSuffix<InboxKindJson, "INBOX_KIND_">;


// Small legacy REST revisions remain numeric; larger values arrive as decimal
// strings. Neither form is converted to a JS number before a conditional write.
export type Bot = Require<Omit<REST<ConsoleBotJson>, "revision"> & { revision: LegacyInt64 },
 "id" | "display_name" | "description" | "instructions" | "is_coordinator" |
 "group_id" | "enabled" | "tools" | "may_message">;
export type Group = Require<Omit<REST<ConsoleGroupJson>, "revision"> & { revision: LegacyInt64 },
 "id" | "name" | "is_default" | "bots">;
export type Routine = Require<REST<ConsoleRoutineJson>, "id" | "name" | "schedule" | "handler_ref" | "enabled">;
export type MemoryRecord = Require<REST<ConsoleMemoryRecordJson>, "id" | "kind" | "text">;
export type InboxItem = Require<Omit<REST<ConsoleInboxItemJson>, "revision" | "tokens_used" | "kind" | "status"> & {
 revision?: LegacyInt64; tokens_used?: LegacyInt64; kind: InboxKind; status: BotStatus;
}, "id" | "recipient" | "sender" | "subject" | "priority" | "attempts">;

export interface SessionInfo { user_id: string }
export type LearnedChange = ConsoleLearnedChangeJson;
export type LearnedReview = Require<ConsoleLearnedReviewJson, "id" | "name" | "revision" | "digest">;
export type TaskApproval = Require<Pick<TaskApprovalRecordJson,
 "id" | "actor" | "parentId" | "state" | "revision" | "expiresAt" | "result" |
 "recoverable" | "sessionId" | "coordinatorConversation" | "transcript" | "receipts" |
 "operation" | "budgetSpent" | "budgetLimits">, "id" | "actor" | "state" | "revision">;
export type TaskMessage = SessionMessageJson;
export type ToolReceipt = TurnToolInvocationJson;
export type BotReply = ConsoleBotReplyJson;

// Older, non-durable bot replies used snake-case REST counters. New evidence
// replies use generated protobuf JSON consistently, locally and over peers.
export function botReply(data: Record<string, unknown>): BotReply {
  return {
    ...data,
    toolsUsed: (data.toolsUsed ?? data.tools_used) as string[] | undefined,
    toolsAttempted: (data.toolsAttempted ?? data.tools_attempted) as string[] | undefined,
    tokensUsed: String(data.tokensUsed ?? data.tokens_used ?? "0"),
    costUsd: Number(data.costUsd ?? data.cost_usd ?? 0),
    sessionId: String(data.sessionId ?? data.session_id ?? ""),
  };
}
export type TaskChoice = "once" | "operation" | "risk_labels" | "deny" | "budget_extension";
export interface ExtraTaskBudget { tool_calls: number; spend_usd: number; egress_bytes: number }

export type ToolInfo = Require<ConsoleToolJson, "name">;
export type CapabilityFlags = Require<ConsoleCapabilityJson, "enabled" | "authorised" | "configured" | "available">;
export interface Capabilities {
 compute: CapabilityFlags;
 "compute-teams": CapabilityFlags;
 "ui-web": CapabilityFlags;
}
export type Session = Require<Omit<REST<ConsoleSessionJson>, "messages"> & { messages: LegacyInt64 },
 "id" | "channel" | "channel_id">;
export type TranscriptMessage = Require<Omit<REST<ConsoleMessageJson>, "seq"> & {seq:LegacyInt64},
 "role" | "content">;

// Configuration is an intentional operator-facing allowlist, not a storage schema.
export interface NodeConfig {
  node_id: string;
  version?: string;
  functions: string[];
  gateway: {
    enabled: boolean;
    bind_address: string;
    http_port: number;
    require_auth: boolean;
    ui_enabled: boolean;
    login_configured: boolean;
    default_timezone?: string;
    queue_mode?: string;
  };
  compute: {
    providers: { label: string; trust_tier?: string; roles?: string[] }[];
    max_tool_calls_per_turn: number;
    self_learning_mode?: string;
  };
  memory: { enabled: boolean; dream_schedule?: string; embedding_model?: string };
  bots: { max_pending: number; drain_enabled: boolean };
  channels: { type: string; enabled: boolean }[];
}

/** ApiError carries the status so a caller can tell a conflict from a
 *  typo — 409 means somebody else edited this while your form was open,
 *  and telling the user to "try again" is only useful if you know that. */
export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

export const consoleSessionID = "console";

const httpUnavailable = 503;
const httpNotFound = 404;
const httpAccepted = 202;

const activeStreams = new Set<AbortController>();

export function cancelActiveStreams(): void {
  for (const stream of activeStreams) stream.abort();
  activeStreams.clear();
}

export function isUnavailable(err: unknown): boolean {
  if (err instanceof TypeError) return true;
  if (err instanceof ApiError) {
    return err.status >= httpUnavailable || err.status === 0;
  }
  return false;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response;
  try {
    res = await fetch(path, {
      credentials: "include",
      ...init,
      headers: {
        "Content-Type": "application/json",
        ...(init?.headers ?? {}),
      },
    });
  } catch (err) {
    throw err instanceof Error ? err : new TypeError("network error");
  }
  if (res.status === 204) {
    return undefined as T;
  }
  if (res.status === 401 && path !== "/v1/session") {
    cancelActiveStreams();
    // A protected call lost its session (node restart, expired or
    // dropped cookie). Tell the gate to show sign-in again rather
    // than rendering "missing bearer token" at the user.
    window.dispatchEvent(new Event("lobslaw:unauthorized"));
  }
  const text = await res.text();
  if (!res.ok) {
    // The API's error body is {"error": "..."}. Falling back to the raw
    // text matters for the cases that do not reach the handler at all —
    // a reverse proxy's 502, say — where a JSON parse failure would
    // replace a useful message with "unexpected token".
    let message = text;
    try {
      message = (JSON.parse(text) as { error?: string }).error ?? text;
    } catch {
      /* keep the raw body */
    }
    throw new ApiError(res.status, message || res.statusText);
  }
  return text ? (JSON.parse(text) as T) : (undefined as T);
}

export const api = {
  pushConfig: () => request<{ public_key: string }>("/v1/push"),
  subscribePush: (subscription: PushSubscriptionJSON) => request<{ binding_id: string; expires_at: string; user_id: string }>("/v1/push", { method: "POST", body: JSON.stringify({ ...subscription, protocol_version: 1 }) }),
  unsubscribePush: (endpoint: string) => request("/v1/push", { method: "DELETE", body: JSON.stringify({ endpoint }) }),
  learnedReviews: () => request<{ reviews?: LearnedReview[] }>("/v1/learned-reviews").then((r) => r.reviews ?? []),
  learnedReview: (id: string) => request<LearnedReview>(`/v1/learned-reviews/${encodeURIComponent(id)}`),
  decideLearnedReview: (review: LearnedReview, approve: boolean) =>
    request<{ message: string }>(`/v1/learned-reviews/${encodeURIComponent(review.id)}/decide`, {
      method: "POST", body: JSON.stringify({ revision: review.revision, digest: review.digest, approve }),
    }),
  taskApprovals: (after = "") => request<{ records?: TaskApproval[]; nextAfterId?: string }>(`/v1/task-approvals?after=${encodeURIComponent(after)}`),
  taskApproval: (id: string) => request<{ record: TaskApproval }>(`/v1/task-approvals/${encodeURIComponent(id)}`),
  decideTask: (task: TaskApproval, choice: TaskChoice, extra_budget?: ExtraTaskBudget) =>
    request<{ record: TaskApproval }>(`/v1/task-approvals/${encodeURIComponent(task.id)}/decide`, {
      method: "POST", body: JSON.stringify({ revision: task.revision, choice, extra_budget }),
    }),
  cancelTask: (task: TaskApproval) =>
    request<{ record: TaskApproval }>(`/v1/task-approvals/${encodeURIComponent(task.id)}/cancel`, {
      method: "POST", body: JSON.stringify({ revision: task.revision }),
    }),
  recoverTask: (task: TaskApproval, acknowledgeDuplicateRisk: boolean) =>
    request<{ record: TaskApproval }>(`/v1/task-approvals/${encodeURIComponent(task.id)}/recover`, {
      method: "POST", body: JSON.stringify({ revision: task.revision, acknowledge_duplicate_risk: acknowledgeDuplicateRisk }),
    }),
  session: () => request<SessionInfo>("/v1/session"),

  login: async (token: string) => {
    await clearLocalPush();
    return request<SessionInfo>("/v1/session", {
      method: "POST",
      headers: { Authorization: `Bearer ${token}` },
    });
  },

  loginCode: async (code: string) => {
    await clearLocalPush();
    return request<SessionInfo>("/v1/session", {
      method: "POST",
      body: JSON.stringify({ code }),
    });
  },

  logout: async () => {
    cancelActiveStreams();
    await clearLocalPush();
    return request<{ status: string }>("/v1/session", { method: "DELETE" });
  },

  capabilities: () => request<Capabilities>("/v1/capabilities"),

  tools: () => request<{ tools: ToolInfo[] }>("/v1/tools").then((r) => r.tools ?? []),

  listBots: () => request<{ bots: Bot[] }>("/v1/bots").then((r) => r.bots ?? []),

  getBot: (id: string) => request<Bot>(`/v1/bots/${encodeURIComponent(id)}`),

  createBot: (bot: Partial<Bot>) =>
    request<Bot>("/v1/bots", { method: "POST", body: JSON.stringify(bot) }),

  updateBot: (id: string, patch: Partial<Bot>) =>
    request<Bot>(`/v1/bots/${encodeURIComponent(id)}`, {
      method: "PATCH",
      body: JSON.stringify(patch),
    }),

  deleteBot: (id: string) =>
    request<void>(`/v1/bots/${encodeURIComponent(id)}`, { method: "DELETE" }),

  listInbox: (botId: string, status?: string) => {
    const q = status && status !== "all" ? `?status=${status}` : "?status=all";
    return request<{ items: InboxItem[] }>(
      `/v1/bots/${encodeURIComponent(botId)}/inbox${q}`,
    ).then((r) => r.items ?? []);
  },

  assign: (botId: string, item: { subject: string; body: string; kind: InboxKind; priority: number }) =>
    request<InboxItem>(`/v1/bots/${encodeURIComponent(botId)}/inbox`, {
      method: "POST",
      body: JSON.stringify(item),
    }),

  readItem: (botId: string, itemId: string) =>
    request<InboxItem>(
      `/v1/inbox/${encodeURIComponent(botId)}/${encodeURIComponent(itemId)}`,
    ),

  actOnItem: (botId: string, itemId: string, action: "retry" | "cancel") =>
    request<InboxItem>(
      `/v1/inbox/${encodeURIComponent(botId)}/${encodeURIComponent(itemId)}`,
      { method: "PATCH", body: JSON.stringify({ action }) },
    ),

  listGroups: async (): Promise<Group[]> => {
    try {
      const r = await request<{ groups: Group[] }>("/v1/groups");
      return r.groups ?? [];
    } catch (err) {
      if (err instanceof ApiError && err.status === httpNotFound) return [];
      throw err;
    }
  },

  renameGroup: (id: string, name: string, revision: Group["revision"]) =>
    request<Group>(`/v1/groups/${encodeURIComponent(id)}`, {
      method: "PATCH",
      body: JSON.stringify({ name, revision }),
    }),

  createGroup: (id: string, name: string) =>
    request<Group>("/v1/groups", {
      method: "POST",
      body: JSON.stringify({ id, name }),
    }),

  /** A bot's scheduled work. Its own, by principal — not filterable
   *  by query, so one bot's routines cannot be listed by asking for
   *  another's. */
  routines: (botId: string) =>
    request<{ routines: Routine[] }>(`/v1/bots/${encodeURIComponent(botId)}/routines`)
      .then((r) => r.routines ?? []),

  /** What a bot remembers, read-only. */
  memory: (botId: string) =>
    request<{ records: MemoryRecord[]; total: number }>(
      `/v1/bots/${encodeURIComponent(botId)}/memory`),

  /** Answer a confirmation the turn is blocked on. The turn is
   *  waiting on the other end of the open SSE stream, so resolving
   *  here is what lets it continue. */
  resolvePrompt: (promptId: string, approve: boolean) =>
    request<{ status?: string }>(`/v1/prompts/${encodeURIComponent(promptId)}/resolve`, {
      method: "POST",
      body: JSON.stringify({ approve }),
    }),

  activity: (limit = 100) =>
    request<{ items: InboxItem[] }>(`/v1/activity?limit=${limit}`).then(
      (r) => r.items ?? [],
    ),

  sendMessage: (text: string) =>
    request<{ reply?: string; needs_confirmation?: boolean }>("/v1/messages", {
      method: "POST",
      body: JSON.stringify({ message: text }),
    }),

  config: () => request<NodeConfig>("/v1/config"),

  botSessions: (botId: string) =>
    request<{ sessions: Session[] }>(
      `/v1/bots/${encodeURIComponent(botId)}/sessions`,
    ).then((r) => r.sessions ?? []),

  transcript: (sessionId: string) =>
    request<{ messages: TranscriptMessage[] }>(
      `/v1/sessions/${encodeURIComponent(sessionId)}`,
    ).then((r) => r.messages ?? []),
};

export async function streamChat(
  message: string,
  onEvent: (event: string, data: Record<string, unknown>) => void,
  signal?: AbortSignal,
): Promise<void> {
  await streamRequest("/v1/messages", { message, session_id: consoleSessionID }, onEvent, signal);
}

/** streamBotChat talks to ONE bot over SSE.
 *
 * Hand-parsed rather than using EventSource, because EventSource
 * cannot issue a POST — and the message has to go in a body, not a
 * query string, where it would end up in every access log between here
 * and the node. */
export async function streamBotChat(
  botId: string,
  message: string,
  onEvent: (event: string, data: Record<string, unknown>) => void,
  signal?: AbortSignal,
): Promise<void> {
  await streamRequest(`/v1/bots/${encodeURIComponent(botId)}/messages`, { message }, onEvent, signal);
}

async function streamRequest(
  path: string,
  body: Record<string, unknown>,
  onEvent: (event: string, data: Record<string, unknown>) => void,
  parentSignal?: AbortSignal,
): Promise<void> {
  const controller = new AbortController();
  activeStreams.add(controller);
  const abort = () => controller.abort();
  parentSignal?.addEventListener("abort", abort, { once: true });
  if (parentSignal?.aborted) controller.abort();
  const { signal } = controller;
  try {
    const res = await fetch(path, {
      signal,
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json", Accept: "text/event-stream" },
      body: JSON.stringify(body),
    });
    if (res.status === 401) {
      cancelActiveStreams();
      window.dispatchEvent(new Event("lobslaw:unauthorized"));
    }
    if (res.status === httpAccepted) {
      const result = await res.json() as { error?: string };
      signal.throwIfAborted();
      onEvent("accepted", { message: result.error ?? "Message accepted by the active turn." });
      return;
    }
    await readSSE(res, onEvent, signal);
  } finally {
    activeStreams.delete(controller);
    parentSignal?.removeEventListener("abort", abort);
  }
}

async function readSSE(
  res: Response,
  onEvent: (event: string, data: Record<string, unknown>) => void,
  signal: AbortSignal,
): Promise<void> {
  if (!res.ok || !res.body) {
    const text = await res.text();
    let msg = text;
    try {
      msg = (JSON.parse(text) as { error?: string }).error ?? text;
    } catch {
      /* keep the raw body */
    }
    throw new ApiError(res.status, msg || res.statusText);
  }

  const reader = res.body.getReader();
  const abort = () => { void reader.cancel().catch(() => {}); };
  signal.addEventListener("abort", abort, { once: true });
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    for (;;) {
      signal.throwIfAborted();
      const { done, value } = await reader.read();
      signal.throwIfAborted();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      // Events are separated by a blank line. Anything after the last
      // one is a partial frame and stays in the buffer.
      const frames = buffer.split("\n\n");
      buffer = frames.pop() ?? "";
      for (const frame of frames) {
        signal.throwIfAborted();
        let event = "message";
        let data = "{}";
        for (const line of frame.split("\n")) {
          if (line.startsWith("event: ")) event = line.slice(7).trim();
          if (line.startsWith("data: ")) data = line.slice(6);
        }
        let parsed: Record<string, unknown>;
        try { parsed = JSON.parse(data) as Record<string, unknown>; }
        catch { continue; /* Ignore malformed frames, never swallow a consumer failure. */ }
        onEvent(event, parsed);
      }
    }
  } finally {
    signal.removeEventListener("abort", abort);
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

/** Colours for a queue item's status.
 *
 * One mapping, used by every view, so an item is the same colour in
 * the activity feed as it is on the bot's own page — a reader
 * scanning for red should not have to check which screen they are on.
 */
export const statusPalette: Record<BotStatus, string> = {
  pending: "gray",
  claimed: "blue",
  waiting: "orange",
  done: "green",
  failed: "red",
  cancelled: "orange",
};
