"use client";

import { useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { FileText, Plus, Trash2 } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { Markdown } from "@multica/ui/markdown";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { memberListOptions } from "@multica/core/workspace/queries";
import { api } from "@multica/core/api";
import {
  usageDocKeys,
  usageDocOptions,
  usageDocsOptions,
  groupUsageDocs,
} from "@multica/core/usage-doc";
import type { UsageDoc } from "@multica/core/types";
import { useT } from "../i18n";

export function UsageDocsPage() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const user = useAuthStore((s) => s.user);
  const [selectedSlug, setSelectedSlug] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<UsageDoc | null>(null);

  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canManage = currentMember?.role === "owner" || currentMember?.role === "admin";

  const { data: docs, isLoading, isError } = useQuery({
    ...usageDocsOptions(wsId, canManage),
    enabled: !!wsId,
  });

  const grouped = useMemo(() => groupUsageDocs(docs ?? []), [docs]);

  useEffect(() => {
    if (!selectedSlug && docs && docs.length > 0) {
      setSelectedSlug(docs[0]?.slug ?? null);
    }
  }, [docs, selectedSlug]);

  const { data: detail } = useQuery({
    ...usageDocOptions(wsId, selectedSlug ?? ""),
    enabled: !!wsId && !!selectedSlug,
  });

  return (
    <div className="flex h-full min-h-0">
      <aside className="hidden w-64 shrink-0 border-r md:block overflow-y-auto p-4">
        {canManage && (
          <Button
            variant="outline"
            size="sm"
            className="mb-3 w-full justify-start gap-2"
            onClick={() => setCreating(true)}
          >
            <Plus className="size-4" />
            {t(($) => $.usage_docs.new_doc)}
          </Button>
        )}
        {isLoading ? (
          <p className="text-sm text-muted-foreground">{t(($) => $.usage_docs.loading)}</p>
        ) : grouped.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t(($) => $.usage_docs.empty)}</p>
        ) : (
          grouped.map(([category, items]) => (
            <div key={category} className="mb-4">
              <p className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">
                {category}
              </p>
              <ul className="space-y-0.5">
                {items.map((doc) => (
                  <li key={doc.slug}>
                    <button
                      type="button"
                      onClick={() => setSelectedSlug(doc.slug)}
                      className={`flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm ${
                        selectedSlug === doc.slug
                          ? "bg-sidebar-accent text-sidebar-accent-foreground"
                          : "text-muted-foreground hover:bg-sidebar-accent/70"
                      }`}
                    >
                      <FileText className="size-3.5 shrink-0" />
                      <span className="truncate">{doc.title}</span>
                    </button>
                  </li>
                ))}
              </ul>
            </div>
          ))
        )}
      </aside>

      <main className="min-w-0 flex-1 overflow-y-auto p-6">
        {isError ? (
          <Card>
            <CardContent className="py-10 text-center text-sm text-muted-foreground">
              {t(($) => $.usage_docs.load_error)}
            </CardContent>
          </Card>
        ) : !detail ? (
          <Card>
            <CardContent className="py-10 text-center text-sm text-muted-foreground">
              {t(($) => $.usage_docs.no_doc_selected)}
            </CardContent>
          </Card>
        ) : (
          <article className="mx-auto max-w-3xl">
            <header className="mb-6 flex items-start justify-between gap-4">
              <div>
                <h1 className="text-title">{detail.title}</h1>
                <p className="mt-1 text-caption text-muted-foreground">
                  {detail.category} · {detail.status}
                  {detail.updated_at
                    ? ` · ${t(($) => $.usage_docs.updated_at)} ${new Date(detail.updated_at).toLocaleString()}`
                    : ""}
                </p>
              </div>
              {canManage && detail.workspace_id && (
                <div className="flex shrink-0 gap-2">
                  <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
                    {t(($) => $.usage_docs.edit)}
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    className="text-destructive"
                    onClick={() => setDeleting(detail)}
                  >
                    <Trash2 className="size-4" />
                  </Button>
                </div>
              )}
            </header>
            <Markdown mode="full">{detail.content}</Markdown>
          </article>
        )}
      </main>

      {creating && (
        <UsageDocDialog
          open
          onOpenChange={(v) => !v && setCreating(false)}
          onSave={async (input) => {
            try {
              await api.createUsageDoc(wsId, input);
              await qc.invalidateQueries({ queryKey: usageDocKeys.list(wsId, true) });
              toast.success(t(($) => $.usage_docs.toast_created));
              setCreating(false);
            } catch (err) {
              toast.error(t(($) => $.usage_docs.toast_save_failed));
            }
          }}
        />
      )}

      {editing && detail && (
        <UsageDocDialog
          open
          initial={detail}
          onOpenChange={(v) => !v && setEditing(false)}
          onSave={async (input) => {
            try {
              await api.updateUsageDoc(wsId, detail.id, input);
              await qc.invalidateQueries({ queryKey: usageDocKeys.list(wsId, true) });
              await qc.invalidateQueries({
                queryKey: usageDocKeys.doc(wsId, detail.slug),
              });
              toast.success(t(($) => $.usage_docs.toast_saved));
              setEditing(false);
            } catch (err) {
              toast.error(t(($) => $.usage_docs.toast_save_failed));
            }
          }}
        />
      )}

      <AlertDialog open={!!deleting} onOpenChange={(v) => !v && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.usage_docs.delete_confirm_title)}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.usage_docs.delete_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.usage_docs.cancel)}</AlertDialogCancel>
            <AlertDialogAction
              onClick={async () => {
                if (!deleting) return;
                try {
                  await api.deleteUsageDoc(wsId, deleting.id);
                  await qc.invalidateQueries({ queryKey: usageDocKeys.list(wsId, true) });
                  if (selectedSlug === deleting.slug) setSelectedSlug(null);
                  toast.success(t(($) => $.usage_docs.toast_deleted));
                } catch (err) {
                  toast.error(t(($) => $.usage_docs.toast_delete_failed));
                } finally {
                  setDeleting(null);
                }
              }}
            >
              {t(($) => $.usage_docs.confirm)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function UsageDocDialog({
  open,
  initial,
  onOpenChange,
  onSave,
}: {
  open: boolean;
  initial?: UsageDoc | null;
  onOpenChange: (open: boolean) => void;
  onSave: (input: {
    slug: string;
    title: string;
    category: string;
    content: string;
    sort_order: number;
    status: string;
  }) => Promise<void>;
}) {
  const { t } = useT("settings");
  const [slug, setSlug] = useState(initial?.slug ?? "");
  const [title, setTitle] = useState(initial?.title ?? "");
  const [category, setCategory] = useState(initial?.category ?? "general");
  const [content, setContent] = useState(initial?.content ?? "");
  const [status, setStatus] = useState(initial?.status ?? "draft");
  const [saving, setSaving] = useState(false);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>
            {initial ? t(($) => $.usage_docs.edit) : t(($) => $.usage_docs.new_doc)}
          </DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-2">
          <div className="grid grid-cols-2 gap-4">
            <div className="grid gap-2">
              <Label htmlFor="doc-slug">{t(($) => $.usage_docs.slug_label)}</Label>
              <Input
                id="doc-slug"
                value={slug}
                onChange={(e) => setSlug(e.target.value)}
                placeholder="opencode-guide" // eslint-disable-line no-restricted-syntax -- technical slug example
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="doc-title">{t(($) => $.usage_docs.title_label)}</Label>
              <Input
                id="doc-title"
                value={title}
                onChange={(e) => setTitle(e.target.value)}
                placeholder={t(($) => $.usage_docs.title_placeholder)}
              />
            </div>
          </div>
          <div className="grid grid-cols-2 gap-4">
            <div className="grid gap-2">
              <Label htmlFor="doc-category">{t(($) => $.usage_docs.category_label)}</Label>
              <Input
                id="doc-category"
                value={category}
                onChange={(e) => setCategory(e.target.value)}
                placeholder="general" // eslint-disable-line no-restricted-syntax -- technical category default
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="doc-status">{t(($) => $.usage_docs.status_label)}</Label>
              <select
                id="doc-status"
                value={status}
                onChange={(e) => setStatus(e.target.value)}
                className="h-9 rounded-md border border-input bg-transparent px-3 text-sm"
              >
                <option value="draft">{"draft"}</option>
                <option value="published">{"published"}</option>
              </select>
            </div>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="doc-content">{t(($) => $.usage_docs.content_label)}</Label>
            <Textarea
              id="doc-content"
              value={content}
              onChange={(e) => setContent(e.target.value)}
              rows={16}
              className="font-mono text-sm"
              placeholder={t(($) => $.usage_docs.content_placeholder)}
            />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t(($) => $.usage_docs.cancel)}
          </Button>
          <Button
            disabled={saving || !slug.trim() || !title.trim()}
            onClick={async () => {
              setSaving(true);
              try {
                await onSave({
                  slug: slug.trim(),
                  title: title.trim(),
                  category: category.trim() || "general",
                  content,
                  sort_order: initial?.sort_order ?? 0,
                  status,
                });
              } finally {
                setSaving(false);
              }
            }}
          >
            {saving ? t(($) => $.usage_docs.saving) : t(($) => $.usage_docs.save)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
