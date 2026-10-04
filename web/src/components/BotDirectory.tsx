import { createContext, useContext, useMemo, type ReactNode } from "react";

const Names = createContext<ReadonlyMap<string, string>>(new Map());

export function BotDirectory({ bots, children }: { bots: { id: string; display_name: string }[] | null; children: ReactNode }) {
  const names = useMemo(() => new Map((bots ?? []).map((bot) => [bot.id, bot.display_name || bot.id])), [bots]);
  return <Names.Provider value={names}>{children}</Names.Provider>;
}

export function useBotName(id: string) { return useContext(Names).get(id) || id; }
