package rongcloud

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// 模型目录请求族（`catalog_`）的释放路径与入站路由（task 2026-10-25/task-001）。
//
// 这三处失败模式**全都不报错**，只表现为「管理端永远拿不到目录 / 进程内存缓慢
// 增长」，所以必须逐条钉住：
//
//  1. 设备回包没在 channel 的入站分发里被识别 → 掉进命令分发器被当未知命令丢弃
//     → 等待方只可能超时（`channel.go` 的 `isModelCatalogResponse` 分支）；
//  2. pending 记录在发送失败 / 超时 / ctx 取消之后没被释放 → 无界泄漏
//     （页面每次切节点都会拉一次目录，这条已经是热路径）；
//  3. 重复回包 / 来自别的节点的回包 → 覆盖掉已经结算的终态。

const (
	catalogTestHostA = "ai-host-a"
	catalogTestHostB = "ai-host-b"
)

// catalogOKResponse 是一份合法的目录回包（含 openai/gpt-5.6-sol）。
func catalogOKResponse(requestID string) string {
	return fmt.Sprintf(
		`{"msg_type":%q,"protocolVersion":%d,"requestId":%q,"defaultModel":null,`+
			`"providers":[{"id":"openai","name":"OpenAI","models":[{"id":"gpt-5.6-sol","name":"GPT 5.6 Sol"}]}],`+
			`"timestamp":1}`,
		catalogResponseMsgType, catalogProtocolVersion, requestID)
}

// catalogBroker 起一个 broker，sender 由调用方决定「设备怎么答」。不答就是
// 超时 / 取消两条路径的现场。
func catalogBroker(t *testing.T, send NodeRequestSender) *NodeRequestBroker {
	t.Helper()
	b := NewNodeRequestBroker(testLogger())
	b.BindSender(send)
	return b
}

// silentSender 模拟「消息发出去了，但设备一直不回」。
func silentSender(context.Context, string, string) error { return nil }

// startCatalog 发一条目录请求并返回它的 request id（失败直接 Fatal）。
func startCatalog(t *testing.T, b *NodeRequestBroker, aiUserID string, timeoutSeconds int) string {
	t.Helper()
	requestID, code := b.StartModelCatalog(context.Background(), NodeRequestHost{
		AIUserID:       aiUserID,
		TimeoutSeconds: timeoutSeconds,
	})
	if code != "" {
		t.Fatalf("StartModelCatalog = %q, want sent", code)
	}
	return requestID
}

// expireNowForTest 把一条 pending 的到期时间拨到过去，等价于「等满了
// host.TimeoutSeconds」，但不真等（生产上这个值是 8s / 12s）。
func (b *NodeRequestBroker) expireNowForTest(requestID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if record, ok := b.pending[requestID]; ok {
		record.expiresAt = time.Now().Add(-time.Second)
	}
}

// TestModelCatalogBrokerReleasesPendingOnSendFailure 是 M1 的回归：发送失败时
// 调用方直接拿到错误码、不会走 AwaitModelCatalog，所以清理必须发生在 send 失败
// 分支里，否则每条失败请求都会永久驻留 pending。
func TestModelCatalogBrokerReleasesPendingOnSendFailure(t *testing.T) {
	b := catalogBroker(t, func(context.Context, string, string) error {
		return fmt.Errorf("rongcloud: send: boom")
	})

	_, code := b.FetchModelCatalog(context.Background(), NodeRequestHost{
		AIUserID: catalogTestHostA, TimeoutSeconds: 5,
	})
	if code != NodeRequestCodeCatalogInvalid {
		t.Fatalf("code = %q, want %q", code, NodeRequestCodeCatalogInvalid)
	}
	if got := b.pendingCount(); got != 0 {
		t.Fatalf("发送失败后 pending 残留 %d 条（无界泄漏）", got)
	}
}

// TestRecommendationBrokerReleasesPendingOnSendFailure 是同一处 bug 在推荐族的
// 孪生用例：两族共用一张 pending 表，行为必须一致。
func TestRecommendationBrokerReleasesPendingOnSendFailure(t *testing.T) {
	b := catalogBroker(t, func(context.Context, string, string) error {
		return fmt.Errorf("rongcloud: send: boom")
	})

	_, code := b.RecommendRoles(context.Background(), NodeRequestHost{
		AIUserID: catalogTestHostA, TimeoutSeconds: 5,
	}, "话题", "目标", 3, "")
	if code != NodeRequestCodeRecommendInvalid {
		t.Fatalf("code = %q, want %q", code, NodeRequestCodeRecommendInvalid)
	}
	if got := b.pendingCount(); got != 0 {
		t.Fatalf("发送失败后 pending 残留 %d 条（无界泄漏）", got)
	}
}

// TestModelCatalogBrokerReleasesPendingOnTimeout：设备不回包 → 超时返回
// MODEL_CATALOG_TIMEOUT，且记录被释放。管理端据此降级为手填。
func TestModelCatalogBrokerReleasesPendingOnTimeout(t *testing.T) {
	b := catalogBroker(t, silentSender)
	requestID := startCatalog(t, b, catalogTestHostA, 5)
	b.expireNowForTest(requestID)

	catalog, code := b.AwaitModelCatalog(context.Background(), requestID)
	if code != NodeRequestCodeCatalogTimeout {
		t.Fatalf("code = %q, want %q", code, NodeRequestCodeCatalogTimeout)
	}
	if len(catalog.Providers) != 0 {
		t.Fatalf("超时不该带回半份目录: %+v", catalog)
	}
	if got := b.pendingCount(); got != 0 {
		t.Fatalf("超时后 pending 残留 %d 条", got)
	}
}

// TestModelCatalogBrokerReleasesPendingOnContextCancel：管理端页面切走 / 请求
// 超时（handler 里的 12s / 8s）会取消 ctx，这条路同样必须释放记录。
func TestModelCatalogBrokerReleasesPendingOnContextCancel(t *testing.T) {
	b := catalogBroker(t, silentSender)
	requestID := startCatalog(t, b, catalogTestHostA, 60)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, code := b.AwaitModelCatalog(ctx, requestID); code != NodeRequestCodeCatalogTimeout {
		t.Fatalf("code = %q, want %q", code, NodeRequestCodeCatalogTimeout)
	}
	if got := b.pendingCount(); got != 0 {
		t.Fatalf("ctx 取消后 pending 残留 %d 条", got)
	}
}

// TestModelCatalogBrokerFirstSettleWins：融云会重投，重复回包不能覆盖已结算的
// 终态（否则操作员看到的目录会「跳变」）。
func TestModelCatalogBrokerFirstSettleWins(t *testing.T) {
	b := catalogBroker(t, silentSender)
	requestID := startCatalog(t, b, catalogTestHostA, 60)

	if !b.AcceptCatalogResponse(catalogTestHostA, catalogOKResponse(requestID)) {
		t.Fatal("合法回包没有被 catalog 族认领")
	}
	// 重投一份畸形回包：仍然要被本族认领（绝不能再进命令分发），但不能覆盖。
	dup := fmt.Sprintf(`{"msg_type":%q,"protocolVersion":%d,"requestId":%q,"providers":"garbage"}`,
		catalogResponseMsgType, catalogProtocolVersion, requestID)
	if !b.AcceptCatalogResponse(catalogTestHostA, dup) {
		t.Fatal("重复回包没有被 catalog 族认领")
	}

	catalog, code := b.AwaitModelCatalog(context.Background(), requestID)
	if code != "" {
		t.Fatalf("code = %q, want completed", code)
	}
	if len(catalog.Providers) != 1 || catalog.Providers[0].ID != "openai" {
		t.Fatalf("重复回包覆盖了首次结算的目录: %+v", catalog)
	}
	if got := b.pendingCount(); got != 0 {
		t.Fatalf("结算后 pending 残留 %d 条", got)
	}
}

// TestModelCatalogBrokerIgnoresAnswerFromOtherNode：别的节点回了同名的 requestId
// 时只能被认领（别进命令分发），绝不能结算 —— 否则一台设备可以伪造另一台的目录。
func TestModelCatalogBrokerIgnoresAnswerFromOtherNode(t *testing.T) {
	b := catalogBroker(t, silentSender)
	requestID := startCatalog(t, b, catalogTestHostA, 60)

	if !b.AcceptCatalogResponse(catalogTestHostB, catalogOKResponse(requestID)) {
		t.Fatal("错节点的回包没有被认领（会被当命令分发下去）")
	}
	b.expireNowForTest(requestID)
	if _, code := b.AwaitModelCatalog(context.Background(), requestID); code != NodeRequestCodeCatalogTimeout {
		t.Fatalf("code = %q, want %q（错节点的回包不该结算）", code, NodeRequestCodeCatalogTimeout)
	}
	if got := b.pendingCount(); got != 0 {
		t.Fatalf("pending 残留 %d 条", got)
	}
}

// TestModelCatalogBrokerRejectsUnrelatedPayloads：认领必须严格 —— 协议版本不符、
// 没有 requestId、msg_type 不是目录回包的，一律返回 false（交回命令分发器），
// 否则会把别人的命令吞掉。
func TestModelCatalogBrokerRejectsUnrelatedPayloads(t *testing.T) {
	b := catalogBroker(t, silentSender)
	requestID := startCatalog(t, b, catalogTestHostA, 60)

	cases := map[string]string{
		"wrong protocol version": fmt.Sprintf(
			`{"msg_type":%q,"protocolVersion":1,"requestId":%q,"providers":[]}`,
			catalogResponseMsgType, requestID),
		"missing request id": fmt.Sprintf(
			`{"msg_type":%q,"protocolVersion":%d,"providers":[]}`,
			catalogResponseMsgType, catalogProtocolVersion),
		"other msg_type": `{"msg_type":"command_result","requestId":"` + requestID + `"}`,
		"unknown request id": fmt.Sprintf(
			`{"msg_type":%q,"protocolVersion":%d,"requestId":"catalog_does-not-exist","providers":[]}`,
			catalogResponseMsgType, catalogProtocolVersion),
	}
	for name, payload := range cases {
		if b.AcceptCatalogResponse(catalogTestHostA, payload) {
			t.Errorf("%s: 不该被 catalog 族认领", name)
		}
	}
	// 上面这些都不该影响这条仍在等待的请求。
	if got := b.pendingCount(); got != 1 {
		t.Fatalf("pending = %d, want 1（无关回包不能结算）", got)
	}
}

// TestHandleWebhookRoutesCatalogResponseToBroker 钉住 `channel.go` 里那个入站
// 分支：目录回包不是 command_result，必须在 `dispatchCommandAsync` **之前**被
// 同步转交 broker。少了那三行，回包会掉进命令分发器当未知命令丢掉，管理端只
// 可能等到超时 —— 功能事实性死亡，且日志里没有任何线索。
func TestHandleWebhookRoutesCatalogResponseToBroker(t *testing.T) {
	ch, srv := newTestWebhookChannel(t)
	defer srv.Close()

	broker := catalogBroker(t, silentSender) // 回包由下面的 webhook 送进来
	ch.systemHandler.nodeRequests = broker

	requestID := startCatalog(t, broker, catalogTestHostA, 5)

	rr := httptest.NewRecorder()
	ch.handleWebhook(rr, buildCommandWebhookRequest(t, "test-secret", catalogTestHostA, catalogOKResponse(requestID)))
	if rr.Code != http.StatusOK {
		t.Fatalf("handleWebhook = %d, want 200", rr.Code)
	}

	catalog, code := broker.AwaitModelCatalog(context.Background(), requestID)
	if code != "" {
		t.Fatalf("目录回包没有到达 broker（code = %q）", code)
	}
	if len(catalog.Providers) != 1 || catalog.Providers[0].ID != "openai" {
		t.Fatalf("catalog = %+v", catalog)
	}

	// 决定性断言：这条消息没有进 dispatchCommandAsync（进了就会登记去重键）。
	ch.commands.mu.Lock()
	claimed := len(ch.commands.seen)
	ch.commands.mu.Unlock()
	if claimed != 0 {
		t.Fatalf("目录回包进了命令分发器（登记了 %d 条去重键）", claimed)
	}
}

// buildCommandWebhookRequest 造一条 `objectName=command` 的融云回调请求，
// 签名与 buildTextWebhookRequest 同一套算法。
func buildCommandWebhookRequest(t *testing.T, appSecret, fromUserID, content string) *http.Request {
	t.Helper()
	form := url.Values{
		"objectName":       {objectNameCommand},
		"fromUserId":       {fromUserID},
		"toUserId":         {"sys-node"},
		"msgUID":           {"msg-catalog-" + fromUserID},
		"msgTimeStamp":     {"1234567890"},
		"conversationType": {"1"},
		"content":          {content},
	}
	req := httptest.NewRequest(http.MethodPost,
		"http://localhost/webhooks/rongcloud?inst=test-inst", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	nonce := "nonce-for-catalog"
	timestamp := "1234567890"
	req.Header.Set("rc-nonce", nonce)
	req.Header.Set("rc-timestamp", timestamp)
	req.Header.Set("rc-signature", computeSignatureFromString(appSecret, nonce, timestamp))
	return req
}
