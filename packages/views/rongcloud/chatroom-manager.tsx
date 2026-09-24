"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Plus, Trash2 } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
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
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentMember } from "@multica/core/permissions";
import {
  rongcloudChatroomsOptions,
  rongcloudNodesOptions,
  rongcloudKeys,
} from "@multica/core/rongcloud";
import { api } from "@multica/core/api";
import { useT } from "../i18n";

export function ChatroomManager() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const { member } = useCurrentMember(wsId);
  const canManage = member?.role === "owner" || member?.role === "admin";

  const { data: chatrooms = [], isLoading } = useQuery({
    ...rongcloudChatroomsOptions(wsId),
    enabled: !!wsId,
  });
  const { data: nodes = [] } = useQuery({
    ...rongcloudNodesOptions(wsId),
    enabled: !!wsId,
  });

  const [showCreate, setShowCreate] = useState(false);
  const [createForm, setCreateForm] = useState({
    rongcloud_chatroom_id: "",
    max_rounds: 3,
    conversation_kind: "discussion",
  });
  const [creating, setCreating] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);

  async function handleCreate() {
    if (creating) return;
    setCreating(true);
    try {
      await api.createRongCloudChatroom(wsId, {
        rongcloud_chatroom_id: createForm.rongcloud_chatroom_id,
        max_rounds: createForm.max_rounds,
        conversation_kind: createForm.conversation_kind,
      });
      await qc.invalidateQueries({ queryKey: rongcloudKeys.chatrooms(wsId) });
      toast.success(t(($) => $.rongcloud.toast_chatroom_created));
      setShowCreate(false);
      setCreateForm({
        rongcloud_chatroom_id: "",
        max_rounds: 3,
        conversation_kind: "discussion",
      });
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.rongcloud.toast_create_failed),
      );
    } finally {
      setCreating(false);
    }
  }

  async function handleDelete() {
    if (!deleteTarget || deleting) return;
    setDeleting(true);
    try {
      await api.deleteRongCloudChatroom(wsId, deleteTarget);
      await qc.invalidateQueries({ queryKey: rongcloudKeys.chatrooms(wsId) });
      toast.success(t(($) => $.rongcloud.toast_chatroom_deleted));
      setDeleteTarget(null);
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.rongcloud.toast_delete_failed),
      );
    } finally {
      setDeleting(false);
    }
  }

  if (isLoading) {
    return (
      <Card>
        <CardContent>
          <p className="text-body text-muted-foreground">
            {t(($) => $.rongcloud.loading)}
          </p>
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="space-y-4">
      {canManage && (
        <div className="flex justify-end">
          <Button size="sm" onClick={() => setShowCreate(true)}>
            <Plus className="size-4" />
            {t(($) => $.rongcloud.create_chatroom)}
          </Button>
        </div>
      )}

      {chatrooms.length === 0 ? (
        <Card>
          <CardContent>
            <p className="text-caption text-muted-foreground">
              {t(($) => $.rongcloud.no_chatrooms)}
            </p>
          </CardContent>
        </Card>
      ) : (
        <div className="space-y-2">
          {chatrooms.map((chatroom) => (
            <Card key={chatroom.id}>
              <CardContent className="flex items-center justify-between gap-4 py-4">
                <div className="min-w-0 flex-1 space-y-1">
                  <p className="text-body font-medium text-foreground">
                    {chatroom.rongcloud_chatroom_id}
                  </p>
                  <p className="text-caption text-muted-foreground">
                    {t(($) => $.rongcloud.max_rounds, { rounds: chatroom.max_rounds })}
                    {" · "}
                    {t(($) => $.rongcloud.conversation_kind)}: {chatroom.conversation_kind}
                  </p>
                </div>
                {canManage && (
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => setDeleteTarget(chatroom.id)}
                  >
                    <Trash2 className="size-3.5 text-destructive" />
                  </Button>
                )}
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      {nodes.length > 0 && (
        <div className="space-y-1">
          <p className="text-caption font-medium text-muted-foreground">
            {t(($) => $.rongcloud.registered_nodes)}
          </p>
          <div className="flex flex-wrap gap-2">
            {nodes.map((node) => (
              <span
                key={node.id}
                className="rounded-md border border-surface-border px-2 py-1 text-micro text-muted-foreground"
              >
                {node.node_id}
                {node.ai_type && ` (${node.ai_type})`}
              </span>
            ))}
          </div>
        </div>
      )}

      <Dialog open={showCreate} onOpenChange={setShowCreate}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {t(($) => $.rongcloud.create_chatroom_title)}
            </DialogTitle>
          </DialogHeader>
          <div className="space-y-4 py-4">
            <div className="space-y-2">
              <Label>{t(($) => $.rongcloud.chatroom_id_label)}</Label>
              <Input
                value={createForm.rongcloud_chatroom_id}
                onChange={(e) =>
                  setCreateForm((f) => ({
                    ...f,
                    rongcloud_chatroom_id: e.target.value,
                  }))
                }
                placeholder="rc_xxx" // eslint-disable-line no-restricted-syntax -- technical placeholder example
              />
            </div>
            <div className="space-y-2">
              <Label>{t(($) => $.rongcloud.max_rounds_label)}</Label>
              <Input
                type="number"
                min={1}
                max={10}
                value={createForm.max_rounds}
                onChange={(e) =>
                  setCreateForm((f) => ({
                    ...f,
                    max_rounds: parseInt(e.target.value, 10) || 1,
                  }))
                }
              />
            </div>
            <div className="space-y-2">
              <Label>{t(($) => $.rongcloud.conversation_kind_label)}</Label>
              <Input
                value={createForm.conversation_kind}
                onChange={(e) =>
                  setCreateForm((f) => ({
                    ...f,
                    conversation_kind: e.target.value,
                  }))
                }
                placeholder="discussion" // eslint-disable-line no-restricted-syntax -- technical placeholder example
              />
            </div>
          </div>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => setShowCreate(false)}
              disabled={creating}
            >
              {t(($) => $.rongcloud.cancel)}
            </Button>
            <Button
              onClick={handleCreate}
              disabled={creating || !createForm.rongcloud_chatroom_id}
            >
              {creating
                ? t(($) => $.rongcloud.creating)
                : t(($) => $.rongcloud.create)}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.rongcloud.delete_confirm_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.rongcloud.delete_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>
              {t(($) => $.rongcloud.cancel)}
            </AlertDialogCancel>
            <AlertDialogAction
              onClick={handleDelete}
              disabled={deleting}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {deleting
                ? t(($) => $.rongcloud.deleting)
                : t(($) => $.rongcloud.delete)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
