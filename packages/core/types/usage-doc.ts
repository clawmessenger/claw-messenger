/** A usage doc maintained by workspace admins and shown in-product. */
export interface UsageDoc {
  id: string;
  /** Empty when the doc is global (shared across workspaces). */
  workspace_id?: string;
  slug: string;
  title: string;
  category: string;
  /** Markdown content rendered with the full markdown renderer. */
  content: string;
  sort_order: number;
  status: "draft" | "published" | string;
  created_by?: string;
  created_at: string;
  updated_at: string;
}

/** Request body for creating or updating a usage doc. */
export interface UsageDocInput {
  slug: string;
  title: string;
  category: string;
  content: string;
  sort_order: number;
  status: string;
}
