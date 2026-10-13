package rongcloud

// command_dispatch.go 让融云的命令回调「先 200，再处理」。
//
// 融云的回调语义是 at-least-once：它要求服务端尽快返回 200，超时未确认就会
// 重投同一条消息。而 service=discussion 的部分动作注定慢 —— recommendRoles /
// createRoundtable 要把请求经融云 P2P 下发给指定设备，等设备端跑完一次 agent
// 调用再回包，耗时以几十秒计（上限由 system_host.timeout_seconds 决定）。
//
// 以前这些动作是在 webhook handler 里同步执行的，于是有三个连锁后果：
//
//  1. 融云的回调客户端先超时断开 → r.Context() 被取消 → 正在等待设备回包的
//     broker 立刻以「超时」收尾，随后那条回包又因为 ctx 已死发不出去。前端
//     拿不到任何应答，只能干等到自己的超时（~3 分钟），最后只看到一句
//     「操作失败，请稍后重试」。
//  2. 同一请求被融云重投，在设备端重复执行 agent —— 表现为「推荐非常慢」，
//     且设备上并发跑着好几遍同样的活。
//  3. 服务端日志里只有 handle command 的报错，看不出是「设备没回」还是
//     「回调断了」。
//
// 现在命令一律丢进后台 goroutine 执行（带独立超时、不随请求取消），webhook
// 立即 200。重投由 claimCommand 按消息 id 去重，不会重复下发到设备。

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

const (
	// commandDispatchTimeout 是后台命令处理的兜底上限。必须大于任何单个动作的
	// 内部超时（最大 host.timeoutSeconds=600s + 余量），否则会把还在等设备回包
	// 的请求提前掐掉。它只是防泄漏的外围护栏，不是业务超时。
	commandDispatchTimeout = 15 * time.Minute

	// commandDedupWindow 去重窗口。至少覆盖一次完整处理耗时，否则重投会在上一个
	// 还在跑的时候被放进来。
	commandDedupWindow = commandDispatchTimeout

	// commandDedupMaxKeys 是去重表里最多保留的键数，防止异常流量下无界增长。
	commandDedupMaxKeys = 8192
)

// commandDedup 记录近期已受理的命令，用于丢弃融云的重投。
type commandDedup struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// claimCommand 原子地登记本次投递的两个标识（消息 id 与请求 id），返回 false
// 表示这条命令已经在处理或处理过了，调用方必须直接丢弃。
//
// 两个键都记：正常的重投 msgUID 相同，而某些重投会重新包装消息、只有信封里的
// request_id 不变。任一命中即视为重复。
func (d *commandDedup) claimCommand(keys ...string) bool {
	now := time.Now()
	effective := make([]string, 0, len(keys))
	for _, key := range keys {
		if key != "" {
			effective = append(effective, key)
		}
	}
	if len(effective) == 0 {
		// 没有可用标识时无法去重，只能放行。
		return true
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == nil {
		d.seen = make(map[string]time.Time, 64)
	}
	for _, key := range effective {
		if at, ok := d.seen[key]; ok && now.Sub(at) < commandDedupWindow {
			// 命中即续期，避免长跑请求在窗口边缘被第二次重投放进来。
			for _, k := range effective {
				d.seen[k] = now
			}
			return false
		}
	}
	for _, key := range effective {
		d.seen[key] = now
	}
	if len(d.seen) > commandDedupMaxKeys {
		for key, at := range d.seen {
			if now.Sub(at) >= commandDedupWindow {
				delete(d.seen, key)
			}
		}
	}
	return true
}

// commandDedupKeys 提取一条命令消息的去重键：IM 消息 id 优先，信封里的
// request_id / requestId 兜底（设备端发 snake_case，C 端 web 发 camelCase）。
func commandDedupKeys(msg NormalizedMessage) []string {
	keys := make([]string, 0, 2)
	if msg.MsgUID != "" {
		keys = append(keys, "uid:"+msg.MsgUID)
	}
	var envelope struct {
		RequestIDSnake string `json:"request_id"`
		RequestIDCamel string `json:"requestId"`
	}
	if err := json.Unmarshal([]byte(msg.Content), &envelope); err == nil {
		if envelope.RequestIDSnake != "" {
			keys = append(keys, "req:"+envelope.RequestIDSnake)
		}
		if envelope.RequestIDCamel != "" {
			keys = append(keys, "req:"+envelope.RequestIDCamel)
		}
	}
	return keys
}

// dispatchCommandAsync 把命令交给后台 goroutine 执行，立即返回。
//
// 用 context.WithoutCancel(reqCtx) 而不是 reqCtx：融云的连接一旦断开，请求
// context 就会被取消，而这条命令很可能正卡在等设备回包上 —— 那正是需要它继续
// 跑完并回包的时刻。请求 context 里的值（链路信息）仍然保留。
func (c *rongcloudChannel) dispatchCommandAsync(reqCtx context.Context, msg NormalizedMessage) {
	if !c.commands.claimCommand(commandDedupKeys(msg)...) {
		c.logger.Info("rongcloud: duplicate command delivery ignored",
			"msgUID", msg.MsgUID, "fromUserID", msg.FromUserID)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), commandDispatchTimeout)
	go func() {
		defer cancel()
		if err := c.systemHandler.handleCommand(ctx, msg); err != nil {
			c.logger.Error("rongcloud: handle command failed",
				"error", err, "msgUID", msg.MsgUID, "fromUserID", msg.FromUserID)
		}
	}()
}
