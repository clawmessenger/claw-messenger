package rongcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// 本轮检查点（round_checkpoint）的生产者。
//
// 历史背景：前端早就有 round_checkpoint 的卡片、状态机分支与逐字段严格校验，
// 但 Go 引擎只把主持人纪要存成一段 markdown（round_summary_completed），从不发
// 这个事件 —— 于是「成员立场 / 共识 / 分歧 / 下一轮聚焦」这套结构化数据在线上
// 从来没出现过，产品页写的检查点实际只是主持人一段自由文本。
//
// 这里的做法是让主持人在纪要末尾追加一段带 checkpoint 语言标记的代码块，由服务端
// 解析、按角色板校正后落成事件。为什么不改设备端协议：派给主持人的任务提示词本来
// 就由服务端生成（interactiveHostTask），设备只负责把 agent 的回答原样回传，所以
// 不需要重新出 exe 就能生效；旧设备同样会照抄这段要求。
//
// 解析失败一律降级为「没有检查点」：纪要本身已经存好，结构化这层是增强，不能因为
// 模型少写一个花括号就让整轮讨论失败。

const (
	// interactiveCheckpointFence 是检查点代码块的语言标记。用它而不是 json，
	// 是为了在兜底扫描时能区分「检查点」和模型顺手写的其他 json 片段。
	interactiveCheckpointFence = "```checkpoint"
	// interactiveCheckpointMaxItems 对齐前端校验上限（每类最多 32 条）。
	interactiveCheckpointMaxItems = 32
	// interactiveCheckpointMaxRunes 对齐前端 boundedString 的 2000 码点上限。
	interactiveCheckpointMaxRunes = 2000
)

// interactiveCheckpointWire 是模型回包里那段 JSON 的形状。字段一律按前端
// RoundCheckpoint 的命名，避免两端各写一套。
type interactiveCheckpointWire struct {
	MemberPositions []struct {
		MemberID string `json:"memberId"`
		RoleName string `json:"roleName"`
		Name     string `json:"name"`
		Position string `json:"position"`
	} `json:"memberPositions"`
	Agreements     []string `json:"agreements"`
	Disagreements  []string `json:"disagreements"`
	OpenQuestions  []string `json:"openQuestions"`
	NextFocus      string   `json:"nextFocus"`
	Recommendation string   `json:"recommendation"`
}

// interactiveCheckpointContract 是追加在主持人任务后面、要求输出机器可读检查点的
// 那段说明。它同时服务于「让模型知道要写什么」和「让代码评审知道字段来源」。
func interactiveCheckpointContract() string {
	return "另外，在最末尾追加一个代码块，语言标记写 checkpoint，块内只放一行 JSON（不要注释，不要在正文重复它）：\n" +
		interactiveCheckpointFence + "\n" +
		`{"memberPositions":[{"memberId":"<名册里的 memberId>","position":"<此人的立场，一句>"}],` +
		`"agreements":[],"disagreements":[],"openQuestions":[],` +
		`"nextFocus":"<下一轮聚焦，一句>","recommendation":"continue"}` + "\n" +
		"```\n" +
		"memberPositions 覆盖本轮每位发过言的成员：memberId 逐字抄名册、别自己编，" +
		"position 只写这个人自己的立场，不要把别人的观点安在他头上；" +
		"三类列表各不超过 5 条、每条一句；" +
		"recommendation 只写 continue 或 finish，与你最后一行「建议」一致。"
}

// interactiveCheckpointRoster 给主持人一份 memberId ↔ 角色名对照表。
//
// 没有这张表时模型只能编 id（或者把角色名当 id），落进事件后前端按 memberId 找
// 角色会全部落空，检查点里就会出现一串裸 id。名册很短，直接放进提示词。
func interactiveCheckpointRoster(ordinary []interactiveRole) string {
	if len(ordinary) == 0 {
		return ""
	}
	lines := make([]string, 0, len(ordinary)+1)
	lines = append(lines, "成员名册（memberId 只能从这里逐字抄）：")
	for _, role := range ordinary {
		name := strings.TrimSpace(role.RoleName)
		if name == "" {
			name = strings.TrimSpace(role.Nickname)
		}
		if name == "" {
			name = role.MemberID
		}
		lines = append(lines, fmt.Sprintf("- memberId=%s 角色=%s", role.MemberID, name))
	}
	return strings.Join(lines, "\n")
}

// parseInteractiveCheckpoint 从主持人回包里摘出检查点代码块，并返回去掉该块之后的
// 纪要正文。第二个返回值在解析失败时原样返回入参，调用方据此决定是否落事件。
func parseInteractiveCheckpoint(content string) (interactiveCheckpointWire, string, bool) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return interactiveCheckpointWire{}, content, false
	}
	// checkpoint 标记优先；json 兜底只接受「长得像检查点」的对象，避免把主持人
	// 顺手写的数据示例当成检查点。模型偶尔会在标记后加一个空格，一并容忍。
	for _, fence := range []struct {
		token  string
		strict bool
	}{
		{interactiveCheckpointFence, false},
		{"``` checkpoint", false},
		{"```json", true},
	} {
		start := strings.LastIndex(trimmed, fence.token)
		if start < 0 {
			continue
		}
		body := trimmed[start+len(fence.token):]
		end := strings.Index(body, "```")
		if end < 0 {
			continue
		}
		raw := strings.TrimSpace(body[:end])
		if raw == "" || !looksLikeCheckpointJSON(raw, fence.strict) {
			continue
		}
		var wire interactiveCheckpointWire
		if err := json.Unmarshal([]byte(raw), &wire); err != nil {
			continue
		}
		markdown := strings.TrimSpace(trimmed[:start] + body[end+3:])
		return wire, markdown, true
	}
	return interactiveCheckpointWire{}, content, false
}

// looksLikeCheckpointJSON 判断一段 JSON 是否可能是检查点。strict=true（json 兜底）
// 时要求至少带一个检查点专属键，防止把别的对象误当检查点。
func looksLikeCheckpointJSON(raw string, strict bool) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return false
	}
	if probe == nil {
		return false
	}
	if !strict {
		return true
	}
	for _, key := range []string{"memberPositions", "agreements", "disagreements", "openQuestions", "nextFocus"} {
		if _, ok := probe[key]; ok {
			return true
		}
	}
	return false
}

// normalizeInteractiveCheckpoint 把模型给的检查点校正成前端契约的形状：
// 成员只认角色板里有的（id 或唯一角色名），认不出的直接丢 —— 宁可少一条立场，
// 也不能把某人的观点安在别人头上；条目数、长度按前端校验上限截断。
func normalizeInteractiveCheckpoint(wire interactiveCheckpointWire, round int, ordinary []interactiveRole) map[string]interface{} {
	byID := make(map[string]struct{}, len(ordinary))
	nameHits := make(map[string][]string, len(ordinary))
	for _, role := range ordinary {
		byID[role.MemberID] = struct{}{}
		for _, candidate := range []string{strings.TrimSpace(role.RoleName), strings.TrimSpace(role.Nickname)} {
			if candidate == "" {
				continue
			}
			nameHits[candidate] = append(nameHits[candidate], role.MemberID)
		}
	}

	positions := make([]map[string]interface{}, 0, len(wire.MemberPositions))
	seen := make(map[string]struct{}, len(wire.MemberPositions))
	for _, item := range wire.MemberPositions {
		if len(positions) >= interactiveCheckpointMaxItems {
			break
		}
		position := clipCheckpointText(item.Position)
		if position == "" {
			continue
		}
		memberID := strings.TrimSpace(item.MemberID)
		if _, ok := byID[memberID]; !ok {
			memberID = ""
			// 模型把角色名当 id 写进来是常见形态；只有名字唯一时才能认定是谁。
			for _, alias := range []string{item.RoleName, item.Name} {
				alias = strings.TrimSpace(alias)
				if alias == "" {
					continue
				}
				if ids := nameHits[alias]; len(ids) == 1 {
					memberID = ids[0]
					break
				}
			}
		}
		if memberID == "" {
			continue
		}
		if _, ok := seen[memberID]; ok {
			continue
		}
		seen[memberID] = struct{}{}
		positions = append(positions, map[string]interface{}{
			"memberId": memberID,
			"position": position,
		})
	}

	recommendation := "continue"
	if strings.EqualFold(strings.TrimSpace(wire.Recommendation), "finish") {
		recommendation = "finish"
	}
	return map[string]interface{}{
		"round":           round,
		"memberPositions": positions,
		"agreements":      cleanCheckpointList(wire.Agreements),
		"disagreements":   cleanCheckpointList(wire.Disagreements),
		"openQuestions":   cleanCheckpointList(wire.OpenQuestions),
		"nextFocus":       clipCheckpointText(wire.NextFocus),
		"recommendation":  recommendation,
	}
}

// cleanCheckpointList 去空、去重、截长、限条数。返回的一定是非 nil 切片：nil 会被
// 序列化成 null，而前端校验要求这三个字段是数组。
func cleanCheckpointList(items []string) []string {
	out := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if len(out) >= interactiveCheckpointMaxItems {
			break
		}
		value := clipCheckpointText(item)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// clipCheckpointText 按 rune 截断（按字节切会把中文切成乱码），并去掉首尾空白。
func clipCheckpointText(value string) string {
	trimmed := strings.TrimSpace(value)
	runes := []rune(trimmed)
	if len(runes) > interactiveCheckpointMaxRunes {
		return strings.TrimSpace(string(runes[:interactiveCheckpointMaxRunes]))
	}
	return trimmed
}

// interactiveRecordCheckpoint 把解析出来的检查点落成事件并广播。
//
// 发件人用系统账号（actorId="system"）而不是主持人节点：检查点是服务端从主持人
// 回包里派生出来的结构化数据，而 C 端 ingress 对「节点发送的 discussion_event」
// 只放行 contribution_completed / round_summary_completed（`isNodeSpokenEvent`
// 的 NODE_SPOKEN_EVENT_TYPES），用节点身份发会被当成伪造事件直接丢掉 —— 而且
// 已经上线的旧前端没有这条白名单，改客户端也救不回来。系统账号广播则所有版本
// 都收得到；卡片上的「谁说的」由 roles 里的 isHost 兜底解析。
//
// 失败只记日志：这一轮的纪要已经存好、状态已经推进到 awaiting_user，检查点是
// 增强层，不能因为它把整轮讨论打进失败态（用户会莫名其妙看到「本轮总结失败」）。
func (rt *discussionRuntime) interactiveRecordCheckpoint(
	ctx context.Context, d DiscussionRecord, task interactiveTask, wire interactiveCheckpointWire,
) {
	ordinary, _, ok := interactiveRoles(d)
	if !ok {
		return
	}
	record, created, err := rt.interactiveEmit(ctx, d, interactiveEvent{
		EventType: "round_checkpoint", ActorID: "system", TargetID: task.TargetID,
		Payload:   normalizeInteractiveCheckpoint(wire, task.Round, ordinary),
		Key:       "round-checkpoint:" + task.RequestID,
		RequestID: task.RequestID, StateVer: d.Version,
		Round: task.Round, Revision: task.RoundRevision,
	})
	if err != nil {
		rt.logger.Warn("discussion: append round checkpoint failed",
			"error", err, "requestId", task.RequestID)
		return
	}
	if created {
		rt.interactiveBroadcast(ctx, d, record)
	}
}

// interactiveFocusForRound 从事件流里读第 round 轮该聚焦的问题（上一轮检查点的
// nextFocus）。用户点「继续下一轮」时 round_started 要带上它，时间线上才会显示
// 「第 N 轮已开始：<要谈的问题>」，而不是一句空话。
func (rt *discussionRuntime) interactiveFocusForRound(ctx context.Context, discussionID string, round int) string {
	if round <= 1 {
		return ""
	}
	events, err := rt.repo.ListEventsAfter(ctx, discussionID, 0)
	if err != nil {
		return ""
	}
	focus := ""
	for _, event := range events {
		if event.EventType != "round_checkpoint" {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		eventRound, _ := payload["round"].(float64)
		if int(eventRound) != round-1 {
			continue
		}
		if value, _ := payload["nextFocus"].(string); strings.TrimSpace(value) != "" {
			focus = value
		}
	}
	return strings.TrimSpace(focus)
}
