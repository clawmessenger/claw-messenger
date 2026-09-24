/** RongCloud integration configuration returned by the public config endpoint. */
export interface RongCloudConfig {
  app_key: string;
  configured: boolean;
}

/** A registered AI node within a workspace. */
export interface RongCloudNode {
  id: string;
  workspace_id: string;
  owner_user_id: string;
  rongcloud_user_id: string;
  node_id: string;
  ai_type: string;
  capabilities: string[];
  deploy_status: "registered" | "deployed" | "offline" | string;
  binding_version: number;
  created_at: string;
  updated_at: string;
}

/** A chatroom that groups AI nodes for multi-agent discussions. */
export interface RongCloudChatroom {
  id: string;
  workspace_id: string;
  rongcloud_chatroom_id: string;
  owner_user_id: string;
  host_node_id: string;
  max_rounds: number;
  conversation_kind: string;
  config: Record<string, unknown>;
  status: "active" | "deleted" | string;
  created_at: string;
  updated_at: string;
}

/** A member (AI node) participating in a chatroom discussion. */
export interface RongCloudChatroomMember {
  id: string;
  chatroom_id: string;
  node_id: string;
  member_type: "ai" | "human" | string;
  role_name: string;
  role_instructions: string;
  capabilities: Record<string, unknown>;
  model: string;
  speaking_order: number;
  enabled: boolean;
  discussion_model: string;
  created_at: string;
  updated_at: string;
}

/** A device credential enrolled for a node. */
export interface RongCloudDevice {
  id: string;
  workspace_id: string;
  owner_user_id: string;
  node_id: string;
  device_name: string;
  device_type: string;
  credential_id: string;
  status: "active" | "disabled" | "deleted" | string;
  created_at: string;
  updated_at: string;
}

/** A model catalog entry for a node. */
export interface RongCloudNodeModel {
  id: string;
  workspace_id: string;
  node_id: string;
  model_id: string;
  provider: string;
  model_name: string;
  config: Record<string, unknown>;
  created_at: string;
  updated_at: string;
}

/** A pairing session for device binding. */
export interface RongCloudPairingSession {
  id: string;
  workspace_id: string;
  ticket: string;
  status: "pending" | "claimed" | "expired" | string;
  client_claim_key: string;
  candidate_node_ids: string[];
  expires_at: string;
  created_at: string;
  updated_at: string;
}

/** System host configuration. */
export interface RongCloudSystemHost {
  config_key: string;
  node_id: string;
  config: Record<string, unknown>;
  config_version: number;
}

/** Discussion state snapshot. */
export interface RongCloudDiscussionState {
  chatroom_id: string;
  workspace_id: string;
  status: "idle" | "starting" | "in_progress" | "paused" | "ended" | string;
  current_round: number;
  current_speaker: string;
  host_node_id: string;
  speakers: RongCloudSpeakerInfo[];
  started_at: string;
  ended_at: string | null;
}

/** Speaker info within a discussion. */
export interface RongCloudSpeakerInfo {
  node_id: string;
  speaking_order: number;
  role_name: string;
  model: string;
  rongcloud_user_id: string;
  ai_type: string;
}

/** A discussion event from the event sourcing log. */
export interface RongCloudDiscussionEvent {
  id: string;
  chatroom_id: string;
  workspace_id: string;
  event_type: string;
  round_number: number;
  speaking_order: number;
  node_id: string;
  content: Record<string, unknown>;
  msg_uid: string;
  created_at: string;
}

/** Response from registering an AI node. */
export interface RongCloudRegisterNodeResponse {
  node_id: string;
  token: string;
  capabilities: string[];
  device_credential_ticket: string;
  binding_version: number;
}

/** Response from enrolling a device credential. */
export interface RongCloudEnrollDeviceResponse {
  credential_id: string;
  secret: string;
}

/** Response from creating a connection session. */
export interface RongCloudConnectionSessionResponse {
  session_id: string;
}
