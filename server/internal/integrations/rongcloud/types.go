package rongcloud

import (
	"encoding/json"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

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

// UnmarshalJSON accepts both spellings of the command envelope.
//
// Two clients put the same objectName ("command") on the wire with different
// key styles: the device CLI (packages/xiachat-cli/src/protocol.ts) sends
// snake_case — request_id / params — while the web client (sendSystemRequest in
// clawmessenger-web/src/services/im.ts) sends camelCase — requestId / payload.
// Parsing only snake_case made every web request look like it had no request_id,
// so handleCommand dropped it before dispatch: the web's group.create never
// reached a handler. Accepting either is the whole fix.
func (c *CommandContent) UnmarshalJSON(data []byte) error {
	var raw struct {
		RequestIDSnake string                 `json:"request_id"`
		RequestIDCamel string                 `json:"requestId"`
		Service        string                 `json:"service"`
		Action         string                 `json:"action"`
		Params         map[string]interface{} `json:"params"`
		Payload        map[string]interface{} `json:"payload"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.RequestID = raw.RequestIDSnake
	if c.RequestID == "" {
		c.RequestID = raw.RequestIDCamel
	}
	c.Service = raw.Service
	c.Action = raw.Action
	// params wins when both are present; payload is the web's name for it.
	c.Params = raw.Params
	if c.Params == nil {
		c.Params = raw.Payload
	}
	if c.Params == nil {
		c.Params = map[string]interface{}{}
	}
	return nil
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
