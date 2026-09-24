"use client";

import { useQuery } from "@tanstack/react-query";
import {
  Pause,
  Play,
  Square,
} from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  rongcloudDiscussionOptions,
  rongcloudDiscussionEventsOptions,
  rongcloudKeys,
} from "@multica/core/rongcloud";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import { useT } from "../i18n";

const STATUS_COLORS: Record<string, string> = {
  idle: "text-muted-foreground",
  starting: "text-blue-500",
  in_progress: "text-success",
  paused: "text-amber-500",
  ended: "text-muted-foreground",
};

const EVENT_LABELS: Record<string, string> = {
  discussion_started: "Discussion Started",
  round_started: "Round Started",
  turn_started: "Turn Started",
  turn_completed: "Turn Completed",
  turn_skipped: "Turn Skipped",
  round_completed: "Round Completed",
  discussion_ended: "Discussion Ended",
  discussion_paused: "Discussion Paused",
  discussion_resumed: "Discussion Resumed",
  host_changed: "Host Changed",
  error: "Error",
};

export function DiscussionMonitor({ chatroomId }: { chatroomId: string }) {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();

  const { data: discussion, isLoading } = useQuery({
    ...rongcloudDiscussionOptions(wsId, chatroomId),
    enabled: !!wsId && !!chatroomId,
  });

  const { data: events = [] } = useQuery({
    ...rongcloudDiscussionEventsOptions(wsId, chatroomId),
    enabled: !!wsId && !!chatroomId,
  });

  async function handleAction(action: "start" | "stop" | "pause" | "resume") {
    try {
      if (action === "start") {
        await api.startRongCloudDiscussion(wsId, chatroomId);
      } else if (action === "stop") {
        await api.stopRongCloudDiscussion(wsId, chatroomId);
      } else if (action === "pause") {
        await api.pauseRongCloudDiscussion(wsId, chatroomId);
      } else {
        await api.resumeRongCloudDiscussion(wsId, chatroomId);
      }
      await qc.invalidateQueries({
        queryKey: rongcloudKeys.discussion(wsId, chatroomId),
      });
      toast.success(t(($) => $.rongcloud.toast_discussion_action_success));
    } catch (e) {
      toast.error(
        e instanceof Error ? e.message : t(($) => $.rongcloud.toast_discussion_failed),
      );
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

  const status = discussion?.status ?? "idle";
  const isActive = status === "in_progress" || status === "starting";

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2">
          <span
            className={`size-2 rounded-full ${isActive ? "bg-success" : "bg-muted-foreground"}`}
          />
          <span className={`text-body font-medium ${STATUS_COLORS[status] ?? ""}`}>
            {status}
          </span>
        </div>
        <div className="flex items-center gap-2">
          {isActive && (
            <>
              <Button variant="outline" size="sm" onClick={() => handleAction("pause")}>
                <Pause className="size-3.5" />
                {t(($) => $.rongcloud.pause)}
              </Button>
              <Button variant="outline" size="sm" onClick={() => handleAction("stop")}>
                <Square className="size-3.5" />
                {t(($) => $.rongcloud.stop_discussion)}
              </Button>
            </>
          )}
          {status === "paused" && (
            <Button variant="outline" size="sm" onClick={() => handleAction("resume")}>
              <Play className="size-3.5" />
              {t(($) => $.rongcloud.resume)}
            </Button>
          )}
          {status === "idle" && (
            <Button variant="outline" size="sm" onClick={() => handleAction("start")}>
              <Play className="size-3.5" />
              {t(($) => $.rongcloud.start_discussion)}
            </Button>
          )}
        </div>
      </div>

      {discussion && discussion.speakers && discussion.speakers.length > 0 && (
        <div className="space-y-1">
          <p className="text-caption font-medium text-muted-foreground">
            {t(($) => $.rongcloud.speakers)}
          </p>
          <div className="flex flex-wrap gap-2">
            {discussion.speakers.map((speaker, i) => (
              <span
                key={i}
                className={`rounded-md border border-surface-border px-2 py-1 text-micro ${
                  discussion.current_speaker === speaker.node_id
                    ? "bg-primary/10 text-primary border-primary/30"
                    : "text-muted-foreground"
                }`}
              >
                {speaker.role_name || `Node ${i + 1}`}
                {discussion.current_speaker === speaker.node_id && " ●"}
              </span>
            ))}
          </div>
        </div>
      )}

      <div className="space-y-1">
        <p className="text-caption font-medium text-muted-foreground">
          {t(($) => $.rongcloud.events_timeline)}
        </p>
        {events.length === 0 ? (
          <p className="text-caption text-muted-foreground">
            {t(($) => $.rongcloud.no_events)}
          </p>
        ) : (
          <div className="space-y-1.5">
            {events.slice(-20).map((event, i) => (
              <div
                key={i}
                className="flex items-start gap-2 text-caption text-muted-foreground"
              >
                <span className="shrink-0 text-micro text-muted-foreground/60">
                  {new Date(event.created_at).toLocaleTimeString()}
                </span>
                <span className="font-medium">
                  {EVENT_LABELS[event.event_type] ?? event.event_type}
                </span>
                {event.round_number > 0 && (
                  <span className="text-micro">R{event.round_number}</span>
                )}
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
