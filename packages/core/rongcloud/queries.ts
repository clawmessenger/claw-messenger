import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

/** Query key namespace for RongCloud resources. */
export const rongcloudKeys = {
  all: (wsId: string) => ["rongcloud", wsId] as const,
  config: () => ["rongcloud", "config"] as const,
  chatrooms: (wsId: string) => [...rongcloudKeys.all(wsId), "chatrooms"] as const,
  chatroom: (wsId: string, chatroomId: string) =>
    [...rongcloudKeys.chatrooms(wsId), chatroomId] as const,
  chatroomMembers: (wsId: string, chatroomId: string) =>
    [...rongcloudKeys.chatroom(wsId, chatroomId), "members"] as const,
  nodes: (wsId: string) => [...rongcloudKeys.all(wsId), "nodes"] as const,
  nodeModels: (wsId: string, nodeId: string) =>
    [...rongcloudKeys.all(wsId), "node-models", nodeId] as const,
  devices: (wsId: string) => [...rongcloudKeys.all(wsId), "devices"] as const,
  systemHost: (wsId: string) => [...rongcloudKeys.all(wsId), "system-host"] as const,
  pairing: (wsId: string, ticket: string) =>
    [...rongcloudKeys.all(wsId), "pairing", ticket] as const,
  discussion: (wsId: string, chatroomId: string) =>
    [...rongcloudKeys.chatroom(wsId, chatroomId), "discussion"] as const,
  discussionEvents: (wsId: string, chatroomId: string) =>
    [...rongcloudKeys.discussion(wsId, chatroomId), "events"] as const,
};

export const rongcloudConfigOptions = () =>
  queryOptions({
    queryKey: rongcloudKeys.config(),
    queryFn: () => api.getRongCloudConfig(),
  });

export const rongcloudChatroomsOptions = (wsId: string) =>
  queryOptions({
    queryKey: rongcloudKeys.chatrooms(wsId),
    queryFn: () => api.listRongCloudChatrooms(wsId),
    enabled: !!wsId,
  });

export const rongcloudChatroomOptions = (wsId: string, chatroomId: string) =>
  queryOptions({
    queryKey: rongcloudKeys.chatroom(wsId, chatroomId),
    queryFn: () => api.getRongCloudChatroom(wsId, chatroomId),
    enabled: !!wsId && !!chatroomId,
  });

export const rongcloudNodesOptions = (wsId: string) =>
  queryOptions({
    queryKey: rongcloudKeys.nodes(wsId),
    queryFn: () => api.listRongCloudNodes(wsId),
    enabled: !!wsId,
  });

export const rongcloudNodeModelsOptions = (wsId: string, nodeId: string) =>
  queryOptions({
    queryKey: rongcloudKeys.nodeModels(wsId, nodeId),
    queryFn: () => api.listRongCloudNodeModels(wsId, nodeId),
    enabled: !!wsId && !!nodeId,
  });

export const rongcloudDevicesOptions = (wsId: string) =>
  queryOptions({
    queryKey: rongcloudKeys.devices(wsId),
    queryFn: () => api.listRongCloudDevices(wsId),
    enabled: !!wsId,
  });

export const rongcloudSystemHostOptions = (wsId: string) =>
  queryOptions({
    queryKey: rongcloudKeys.systemHost(wsId),
    queryFn: () => api.getRongCloudSystemHost(wsId),
    enabled: !!wsId,
  });

export const rongcloudPairingOptions = (wsId: string, ticket: string) =>
  queryOptions({
    queryKey: rongcloudKeys.pairing(wsId, ticket),
    queryFn: () => api.getRongCloudPairing(wsId, ticket),
    enabled: !!wsId && !!ticket,
  });

export const rongcloudDiscussionOptions = (wsId: string, chatroomId: string) =>
  queryOptions({
    queryKey: rongcloudKeys.discussion(wsId, chatroomId),
    queryFn: () => api.getRongCloudDiscussion(wsId, chatroomId),
    enabled: !!wsId && !!chatroomId,
    // Discussion state changes arrive via RongCloud webhooks, not HTTP
    // mutations. Poll lightly while the monitor is open.
    refetchInterval: (query) =>
      query.state.status === "success" &&
      query.state.data?.status !== "ended" &&
      query.state.data?.status !== "idle"
        ? 3_000
        : false,
  });

export const rongcloudDiscussionEventsOptions = (wsId: string, chatroomId: string) =>
  queryOptions({
    queryKey: rongcloudKeys.discussionEvents(wsId, chatroomId),
    queryFn: () => api.listRongCloudDiscussionEvents(wsId, chatroomId),
    enabled: !!wsId && !!chatroomId,
    refetchInterval: (query) =>
      query.state.status === "success" && (query.state.data?.length ?? 0) > 0
        ? 3_000
        : false,
  });
