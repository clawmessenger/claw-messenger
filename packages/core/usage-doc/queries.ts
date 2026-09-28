import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";
import type { UsageDoc } from "../types";

export const usageDocKeys = {
  all: (wsId: string) => ["usage-docs", wsId] as const,
  list: (wsId: string, includeDrafts: boolean) =>
    [...usageDocKeys.all(wsId), "list", includeDrafts] as const,
  doc: (wsId: string, slug: string) =>
    [...usageDocKeys.all(wsId), "doc", slug] as const,
};

export const usageDocsOptions = (wsId: string, includeDrafts = false) =>
  queryOptions({
    queryKey: usageDocKeys.list(wsId, includeDrafts),
    queryFn: () => api.listUsageDocs(wsId, includeDrafts),
    enabled: !!wsId,
  });

export const usageDocOptions = (wsId: string, slug: string) =>
  queryOptions({
    queryKey: usageDocKeys.doc(wsId, slug),
    queryFn: () => api.getUsageDoc(wsId, slug),
    enabled: !!wsId && !!slug,
  });

/** Group docs by category, preserving category first-seen order. */
export function groupUsageDocs(docs: UsageDoc[]): Array<[string, UsageDoc[]]> {
  const groups = new Map<string, UsageDoc[]>();
  for (const doc of docs) {
    const key = doc.category || "general";
    const list = groups.get(key);
    if (list) {
      list.push(doc);
    } else {
      groups.set(key, [doc]);
    }
  }
  return Array.from(groups.entries());
}
