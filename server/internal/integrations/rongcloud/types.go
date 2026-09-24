package rongcloud

import "github.com/multica-ai/multica/server/internal/integrations/channel"

// TypeRongCloud is the channel type slug for RongCloud installations.
const TypeRongCloud channel.Type = "rongcloud"

// RongCloud message objectName constants.
const (
	objectNameText    = "RC:TxtMsg"
	objectNameImage   = "RC:ImgMsg"
	objectNameCommand = "command"
	objectNameStream  = "RC:StreamMsg"
	objectNameTyping  = "RC:TypSts"
)

// NormalizedMessage represents a parsed RongCloud webhook payload.
type NormalizedMessage struct {
	ObjectName       string `json:"objectName"`
	FromUserID       string `json:"fromUserId"`
	ToUserID         string `json:"toUserId"`
	TargetID         string `json:"targetId"`
	Content          string `json:"content"`
	MsgUID           string `json:"msgUID"`
	MsgTimeStamp     string `json:"msgTimeStamp"`
	ConversationType string `json:"conversationType"`
}

// CommandContent is the parsed JSON content of a command message.
type CommandContent struct {
	RequestID string                 `json:"request_id"`
	Service   string                 `json:"service"`
	Action    string                 `json:"action"`
	Params    map[string]interface{} `json:"params,omitempty"`
}

// CommandResultContent is the JSON content sent back as a command_result.
type CommandResultContent struct {
	RequestID string                 `json:"request_id"`
	MsgType   string                 `json:"msg_type"`
	Payload   map[string]interface{} `json:"payload"`
}

// getString extracts a string value from a map, returning "" if missing or not a string.
func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
