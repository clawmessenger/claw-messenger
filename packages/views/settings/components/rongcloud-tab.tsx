"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Play, Square, Trash2 } from "lucide-react";
import { RongCloudMark } from "./rongcloud-mark";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
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
  rongcloudConfigOptions,
  rongcloudChatroomsOptions,
  rongcloudNodesOptions,
  rongcloudKeys,
} from "@multica/core/rongcloud";
import { api } from "@multica/core/api";
import { useT } from "../../i18n";

export function RongCloudTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();

  const { member } = useCurrentMember(wsId);
  const canManage = member?.role === "owner" || member?.role === "admin";

  const { data: config, isLoading: configLoading } = useQuery({
    ...rongcloudConfigOptions(),
  });
  const configured = config?.configured === true;

  const { data: chatrooms = [], isLoading: chatroomsLoading } = useQuery({
    ...rongcloudChatroomsOptions(wsId),
    enabled: !!wsId && configured,
  });
  const { data: nodes = [] } = useQuery({
    ...rongcloudNodesOptions(wsId),
    enabled: !!wsId && configured,
  });

  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [discussionAction, setDiscussionAction] = useState<
    { chatroomId: string; action: "start" | "stop" } | null
  >(null);
  const [discussionLoading, setDiscussionLoading] = useState(false);

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
        e instanceof Error
          ? e.message
          : t(($) => $.rongcloud.toast_delete_failed),
      );
    } finally {
      setDeleting(false);
    }
  }

  async function handleDiscussionAction() {
    if (!discussionAction || discussionLoading) return;
    setDiscussionLoading(true);
    try {
      if (discussionAction.action === "start") {
        await api.startRongCloudDiscussion(wsId, discussionAction.chatroomId);
        toast.success(t(($) => $.rongcloud.toast_discussion_started));
      } else {
        await api.stopRongCloudDiscussion(wsId, discussionAction.chatroomId);
        toast.success(t(($) => $.rongcloud.toast_discussion_stopped));
      }
      await qc.invalidateQueries({
        queryKey: rongcloudKeys.discussion(wsId, discussionAction.chatroomId),
      });
      setDiscussionAction(null);
    } catch (e) {
      toast.error(
        e instanceof Error
          ? e.message
          : t(($) => $.rongcloud.toast_discussion_failed),
      );
    } finally {
      setDiscussionLoading(false);
    }
  }

  if (configLoading) {
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

  if (!configured) {
    return (
      <Card>
        <CardContent className="space-y-2">
          <p className="text-body font-medium">
            {t(($) => $.rongcloud.not_enabled_title)}
          </p>
          <p className="text-caption text-muted-foreground">
            {t(($) => $.rongcloud.not_enabled_description_prefix)}{" "}
            <code className="rounded-xs bg-muted px-1 py-0.5 text-micro">
              MULTICA_RONGCLOUD_SECRET_KEY
            </code>{" "}
            {t(($) => $.rongcloud.not_enabled_description_suffix)}
          </p>
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="space-y-8">
      <section className="space-y-3">
        <div className="flex items-center gap-2">
          <RongCloudMark className="size-5 text-success" />
          <h2 className="text-body font-semibold">
            {t(($) => $.rongcloud.configured_title)}
          </h2>
        </div>
        <p className="text-caption text-muted-foreground">
          {t(($) => $.rongcloud.configured_description, {
            nodes: nodes.length,
          })}
        </p>
      </section>

      <section className="space-y-3">
        <h2 className="text-body font-semibold">
          {t(($) => $.rongcloud.chatrooms_title)}
        </h2>
        {chatroomsLoading ? (
          <Card>
            <CardContent>
              <p className="text-body text-muted-foreground">
                {t(($) => $.rongcloud.loading)}
              </p>
            </CardContent>
          </Card>
        ) : chatrooms.length === 0 ? (
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
                      {t(($) => $.rongcloud.max_rounds, {
                        rounds: chatroom.max_rounds,
                      })}
                    </p>
                  </div>
                  {canManage && (
                    <div className="flex items-center gap-2">
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() =>
                          setDiscussionAction({
                            chatroomId: chatroom.id,
                            action: "start",
                          })
                        }
                      >
                        <Play className="size-3.5" />
                        {t(($) => $.rongcloud.start_discussion)}
                      </Button>
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() =>
                          setDiscussionAction({
                            chatroomId: chatroom.id,
                            action: "stop",
                          })
                        }
                      >
                        <Square className="size-3.5" />
                        {t(($) => $.rongcloud.stop_discussion)}
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => setDeleteTarget(chatroom.id)}
                      >
                        <Trash2 className="size-3.5 text-destructive" />
                      </Button>
                    </div>
                  )}
                </CardContent>
              </Card>
            ))}
          </div>
        )}
      </section>

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

      <AlertDialog
        open={discussionAction !== null}
        onOpenChange={(open) => !open && setDiscussionAction(null)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {discussionAction?.action === "start"
                ? t(($) => $.rongcloud.start_discussion_confirm_title)
                : t(($) => $.rongcloud.stop_discussion_confirm_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {discussionAction?.action === "start"
                ? t(($) => $.rongcloud.start_discussion_confirm_description)
                : t(($) => $.rongcloud.stop_discussion_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={discussionLoading}>
              {t(($) => $.rongcloud.cancel)}
            </AlertDialogCancel>
            <AlertDialogAction
              onClick={handleDiscussionAction}
              disabled={discussionLoading}
            >
              {discussionLoading
                ? t(($) => $.rongcloud.processing)
                : t(($) => $.rongcloud.confirm)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
