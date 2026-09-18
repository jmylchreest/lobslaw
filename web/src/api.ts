export type BotStatus = "pending" | "claimed" | "done" | "failed" | "cancelled";

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

export interface CapabilityFlags {
  enabled: boolean;
  authorised: boolean;
  configured: boolean;
  available: boolean;
}

export interface Capabilities {
  compute: CapabilityFlags;
  "compute-teams": CapabilityFlags;
  "ui-web": CapabilityFlags;
}

export interface SessionInfo {
  user_id: string;
}

export interface Group {
  id: string;
  name: string;
  mine?: boolean;
}

export const consoleSessionID = "console";

const httpUnavailable = 503;
const httpNotFound = 404;

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
    // A protected call lost its session (node restart, expired or
    // dropped cookie). Tell the gate to show sign-in again rather
    // than rendering "missing bearer token" at the user.
    window.dispatchEvent(new Event("lobslaw:unauthorized"));
  }
  const text = await res.text();
  if (!res.ok) {
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
  session: () => request<SessionInfo>("/v1/session"),

  login: (token: string) =>
    request<SessionInfo>("/v1/session", {
      method: "POST",
      headers: { Authorization: `Bearer ${token}` },
    }),

  loginCode: (code: string) =>
    request<SessionInfo>("/v1/session", {
      method: "POST",
      body: JSON.stringify({ code }),
    }),

  loginLoopback: () =>
    request<SessionInfo>("/v1/session", {
      method: "POST",
      body: JSON.stringify({ loopback: true }),
    }),

  logout: () => request<{ status: string }>("/v1/session", { method: "DELETE" }),

  capabilities: () => request<Capabilities>("/v1/capabilities"),

  listGroups: async (): Promise<Group[]> => {
    try {
      const r = await request<{ groups: Group[] }>("/v1/groups");
      return r.groups ?? [];
    } catch (err) {
      if (err instanceof ApiError && err.status === httpNotFound) return [];
      throw err;
    }
  },
};

export async function streamChat(
  message: string,
  onEvent: (event: string, data: Record<string, unknown>) => void,
): Promise<void> {
  let res: Response;
  try {
    res = await fetch("/v1/messages", {
      method: "POST",
      credentials: "include",
      headers: {
        "Content-Type": "application/json",
        Accept: "text/event-stream",
      },
      body: JSON.stringify({ message, session_id: consoleSessionID }),
    });
  } catch (err) {
    throw err instanceof Error ? err : new TypeError("network error");
  }
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
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    const frames = buffer.split("\n\n");
    buffer = frames.pop() ?? "";
    for (const frame of frames) {
      let event = "message";
      let data = "{}";
      for (const line of frame.split("\n")) {
        if (line.startsWith("event: ")) event = line.slice(7).trim();
        if (line.startsWith("data: ")) data = line.slice(6);
      }
      try {
        onEvent(event, JSON.parse(data) as Record<string, unknown>);
      } catch {
        /* a frame we cannot parse is one we cannot act on */
      }
    }
  }
}
