package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// ---- 脚本化假 DB ------------------------------------------------------------

// convFakeDB 让每个用例只声明它关心的语句；未声明的分支直接 panic，避免
// 「忘了打桩」变成静默通过（与 claw_admin_groups_test.go 同一套路）。
type convFakeDB struct {
	queryRow func(sql string, args []any) pgx.Row
	query    func(sql string, args []any) (pgx.Rows, error)
	exec     func(sql string, args []any) (pgconn.CommandTag, error)
	stmts    []string
	args     [][]any
}

func (f *convFakeDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	f.record(sql, args)
	if f.queryRow == nil {
		panic("convFakeDB.QueryRow not stubbed: " + sql)
	}
	return f.queryRow(sql, args)
}

func (f *convFakeDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.record(sql, args)
	if f.query == nil {
		panic("convFakeDB.Query not stubbed: " + sql)
	}
	return f.query(sql, args)
}

func (f *convFakeDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.record(sql, args)
	if f.exec == nil {
		if strings.Contains(sql, "claw_admin_audit_logs") {
			return pgconn.NewCommandTag("INSERT 0 1"), nil
		}
		panic("convFakeDB.Exec not stubbed: " + sql)
	}
	return f.exec(sql, args)
}

func (f *convFakeDB) record(sql string, args []any) {
	f.stmts = append(f.stmts, sql)
	f.args = append(f.args, args)
}

// sawStatement reports whether any issued statement contains marker.
func (f *convFakeDB) sawStatement(marker string) bool {
	for _, sql := range f.stmts {
		if strings.Contains(sql, marker) {
			return true
		}
	}
	return false
}

// argsFor returns the arguments bound to the first statement containing marker.
func (f *convFakeDB) argsFor(t *testing.T, marker string) []any {
	t.Helper()
	for i, sql := range f.stmts {
		if strings.Contains(sql, marker) {
			return f.args[i]
		}
	}
	t.Fatalf("no statement contained %q; issued: %v", marker, f.stmts)
	return nil
}

// ---- 行 / 结果集打桩 --------------------------------------------------------

type convStubRow struct {
	pgx.Row
	values []any
	err    error
}

func (r *convStubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return assignConvScan(dest, r.values)
}

type convStubRows struct {
	pgx.Rows
	rows [][]any
	idx  int
	err  error
}

func (r *convStubRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *convStubRows) Scan(dest ...any) error {
	if r.idx == 0 || r.idx > len(r.rows) {
		return errors.New("Scan called outside of Next")
	}
	return assignConvScan(dest, r.rows[r.idx-1])
}

func (r *convStubRows) Close()     {}
func (r *convStubRows) Err() error { return r.err }

// assignConvScan 按目标类型逐列赋值。比群组用例里的手写 switch 更宽：这一片
// 有可空时间戳（last_message_at / max_rounds）和 pgtype.UUID，手写穷举会把
// 用例淹没在类型分支里。指针目标（*time.Time、*int）遇到非 nil 值会自动分配。
func assignConvScan(dest []any, values []any) error {
	if len(dest) != len(values) {
		return fmt.Errorf("scan arity mismatch: %d destinations vs %d values", len(dest), len(values))
	}
	for i := range dest {
		if err := assignConvOne(dest[i], values[i]); err != nil {
			return fmt.Errorf("column %d: %w", i, err)
		}
	}
	return nil
}

func assignConvOne(target any, value any) error {
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("target %T is not a non-nil pointer", target)
	}
	elem := rv.Elem()
	if value == nil {
		elem.Set(reflect.Zero(elem.Type()))
		return nil
	}
	source := reflect.ValueOf(value)
	// 先试直接赋值/转换，再考虑解引用：可空列（*time.Time / *int）既可能拿到值，
	// 也可能拿到同类型的指针，先走指针分支会把后者判成类型不匹配。
	if source.Type().AssignableTo(elem.Type()) {
		elem.Set(source)
		return nil
	}
	if source.Type().ConvertibleTo(elem.Type()) {
		elem.Set(source.Convert(elem.Type()))
		return nil
	}
	if elem.Kind() == reflect.Ptr {
		allocated := reflect.New(elem.Type().Elem())
		if err := assignConvOne(allocated.Interface(), value); err != nil {
			return err
		}
		elem.Set(allocated)
		return nil
	}
	return fmt.Errorf("cannot assign %T to %s", value, elem.Type())
}

// ---- 认证脚手架 -------------------------------------------------------------

const convAdminID = "admin-1"

func newConvHandler(fake *convFakeDB, permissions []string) *Handler {
	encoded, _ := json.Marshal(permissions)
	baseRow := &convStubRow{values: []any{
		convAdminID, "boss", "老板", "role-1", "active", "超级管理员", encoded,
	}}
	fake.queryRow = func(string, []any) pgx.Row { return baseRow }
	return &Handler{DB: fake}
}

// convRequest 造一个带 Bearer token 的请求，并把 {id} 塞进 chi 的路由上下文，
// 这样不用起完整 router 也能测 handler。
func convRequest(t *testing.T, method, target string, body string, permissions []string) *http.Request {
	t.Helper()
	token, err := issueClawAdminJWT(convAdminID, "access", time.Hour)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if idx := strings.Index(target, "?"); idx >= 0 {
		target = target[:idx]
	}
	segments := strings.Split(strings.Trim(target, "/"), "/")
	// /api/admin/<resource>/<id>[/<sub>] → id 总是第 4 段。
	rctx := chi.NewRouteContext()
	if len(segments) >= 4 {
		rctx.URLParams.Add("id", segments[3])
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func decodeConv(t *testing.T, rr *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var payload struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rr.Body.String())
	}
	out := map[string]interface{}{
		"code":    float64(payload.Code),
		"message": payload.Message,
	}
	if len(payload.Data) > 0 {
		var data interface{}
		if err := json.Unmarshal(payload.Data, &data); err != nil {
			t.Fatalf("data is not JSON: %v (%s)", err, payload.Data)
		}
		out["data"] = data
	}
	return out
}

func dataMap(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	data, ok := body["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("data is not an object: %#v", body["data"])
	}
	return data
}

func dataList(t *testing.T, body map[string]interface{}) []interface{} {
	t.Helper()
	data, ok := body["data"].([]interface{})
	if !ok {
		t.Fatalf("data is not an array: %#v", body["data"])
	}
	return data
}

func firstItem(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	items, ok := dataMap(t, body)["items"].([]interface{})
	if !ok || len(items) == 0 {
		t.Fatalf("no items in %#v", body["data"])
	}
	item, ok := items[0].(map[string]interface{})
	if !ok {
		t.Fatalf("item is not an object: %#v", items[0])
	}
	return item
}

var convReadOnly = []string{"group:read", "user:read", "message:read", "stats:read", "group:write"}

// ---- 聊天室列表 -------------------------------------------------------------

func chatRoomRow() []any {
	created := time.Date(2026, 10, 15, 6, 15, 22, 0, time.UTC)
	lastMessage := time.Date(2026, 10, 15, 6, 39, 45, 0, time.UTC)
	return []any{
		"rt_2de41fc1-50d3-5064-95e7-4e3f50febe23", "ai这两年可以发展到什么程度？讨论组",
		"discussion", "proposal", "850509", "opencode_1ziviig73i",
		5, 214, lastMessage, 1, created, created,
	}
}

func TestClawAdminListChatRoomsProjectsRoomRow(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM claw_admin_users"):
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["group:read"]`)}}
		case strings.Contains(sql, "SELECT COUNT(*)"):
			return &convStubRow{values: []any{1}}
		}
		panic("unexpected QueryRow: " + sql)
	}
	fake.query = func(sql string, _ []any) (pgx.Rows, error) {
		if !strings.Contains(sql, "FROM rongcloud_chatroom c") {
			panic("unexpected Query: " + sql)
		}
		return &convStubRows{rows: [][]any{chatRoomRow()}}, nil
	}

	rr := httptest.NewRecorder()
	h.ClawAdminListChatRooms(rr, convRequest(t, http.MethodGet, "/api/admin/chatrooms?page=1&page_size=10", "", convReadOnly))

	body := decodeConv(t, rr)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if total := dataMap(t, body)["total"].(float64); total != 1 {
		t.Fatalf("total = %v, want 1", total)
	}
	item := firstItem(t, body)
	checks := map[string]interface{}{
		"room_id":           "rt_2de41fc1-50d3-5064-95e7-4e3f50febe23",
		"title":             "ai这两年可以发展到什么程度？讨论组",
		"conversation_kind": "discussion",
		"stage":             "proposal",
		"user_id":           "850509",
		"assigned_executor": "opencode_1ziviig73i",
		"participant_count": float64(5),
		"message_count":     float64(214),
		"last_message_at":   "2026-10-15T06:39:45Z",
	}
	for key, want := range checks {
		if got := item[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
}

func TestClawAdminListChatRoomsDefaultsToActiveRooms(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		if strings.Contains(sql, "FROM claw_admin_users") {
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["group:read"]`)}}
		}
		return &convStubRow{values: []any{0}}
	}
	fake.query = func(string, []any) (pgx.Rows, error) { return &convStubRows{}, nil }

	rr := httptest.NewRecorder()
	h.ClawAdminListChatRooms(rr, convRequest(t, http.MethodGet, "/api/admin/chatrooms", "", convReadOnly))

	if !sawStatementContaining(fake, "room_status = 'active'") {
		t.Fatalf("list did not exclude soft-deleted rooms; statements: %v", fake.stmts)
	}
}

// sawStatementContaining 同时覆盖 Query 与 QueryRow（列表的 COUNT 与 SELECT 都会带
// where 子句，任一条命中即算）。
func sawStatementContaining(fake *convFakeDB, marker string) bool {
	return fake.sawStatement(marker)
}

func TestClawAdminListChatRoomsRejectsUnknownStage(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	rr := httptest.NewRecorder()
	h.ClawAdminListChatRooms(rr, convRequest(t, http.MethodGet, "/api/admin/chatrooms?stage=bogus", "", convReadOnly))

	body := decodeConv(t, rr)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(body["message"].(string), "stage") {
		t.Fatalf("message = %v, want it to mention stage", body["message"])
	}
}

func TestClawAdminListChatRoomsConversationKindFilter(t *testing.T) {
	newHandler := func() (*Handler, *convFakeDB) {
		fake := &convFakeDB{}
		h := newConvHandler(fake, convReadOnly)
		// newConvHandler 会覆盖 queryRow，必须在它之后再 stub。
		fake.queryRow = func(sql string, _ []any) pgx.Row {
			if strings.Contains(sql, "FROM claw_admin_users") {
				return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["group:read"]`)}}
			}
			return &convStubRow{values: []any{0}}
		}
		fake.query = func(string, []any) (pgx.Rows, error) { return &convStubRows{}, nil }
		return h, fake
	}

	// discussion=讨论组房间：走参数化的 conversation_kind = $n。
	h, fake := newHandler()
	rr := httptest.NewRecorder()
	h.ClawAdminListChatRooms(rr, convRequest(t, http.MethodGet, "/api/admin/chatrooms?conversation_kind=discussion", "", convReadOnly))
	if rr.Code != http.StatusOK {
		t.Fatalf("discussion filter status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !sawStatementContaining(fake, "conversation_kind = $") {
		t.Fatalf("discussion filter missing parameterized kind clause; statements: %v", fake.stmts)
	}

	// normal=普通聊天室：投影里 kind 是空串，只能写成 = ''，不能参数化空串
	// （空串在 handler 里表示「不过滤」）。
	h, fake = newHandler()
	rr = httptest.NewRecorder()
	h.ClawAdminListChatRooms(rr, convRequest(t, http.MethodGet, "/api/admin/chatrooms?conversation_kind=normal", "", convReadOnly))
	if rr.Code != http.StatusOK {
		t.Fatalf("normal filter status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !sawStatementContaining(fake, "conversation_kind = ''") {
		t.Fatalf("normal filter should match empty kind literally; statements: %v", fake.stmts)
	}

	// 其他取值一律 400，与 stage 的口径一致。
	h, _ = newHandler()
	rr = httptest.NewRecorder()
	h.ClawAdminListChatRooms(rr, convRequest(t, http.MethodGet, "/api/admin/chatrooms?conversation_kind=bogus", "", convReadOnly))
	body := decodeConv(t, rr)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bogus kind status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(body["message"].(string), "conversation_kind") {
		t.Fatalf("message = %v, want it to mention conversation_kind", body["message"])
	}
}

func TestClawAdminListChatRoomsKeywordEscapesForLike(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		if strings.Contains(sql, "FROM claw_admin_users") {
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["group:read"]`)}}
		}
		return &convStubRow{values: []any{0}}
	}
	fake.query = func(string, []any) (pgx.Rows, error) { return &convStubRows{}, nil }

	rr := httptest.NewRecorder()
	h.ClawAdminListChatRooms(rr, convRequest(t, http.MethodGet, "/api/admin/chatrooms?keyword=50%25_off", "", convReadOnly))

	args := fake.argsFor(t, "SELECT COUNT(*)")
	if len(args) != 1 {
		t.Fatalf("count args = %v, want one pattern", args)
	}
	if got := args[0].(string); got != `%50\%\_off%` {
		t.Fatalf("pattern = %q, want LIKE wildcards escaped", got)
	}
}

func TestClawAdminListChatRoomsRequiresReadPermission(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, []string{"stats:read"})
	rr := httptest.NewRecorder()
	req := convRequest(t, http.MethodGet, "/api/admin/chatrooms", "", []string{"stats:read"})
	req.Header.Set("Authorization", "Bearer "+mustToken(t, "access"))
	h.ClawAdminListChatRooms(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rr.Code, rr.Body.String())
	}
}

func mustToken(t *testing.T, kind string) string {
	t.Helper()
	token, err := issueClawAdminJWT(convAdminID, kind, time.Hour)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	return token
}

// ---- 聊天室详情 / 消息 ------------------------------------------------------

func TestClawAdminGetChatRoomMissingReturns404(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		if strings.Contains(sql, "FROM claw_admin_users") {
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["group:read"]`)}}
		}
		return &convStubRow{err: pgx.ErrNoRows}
	}
	rr := httptest.NewRecorder()
	h.ClawAdminGetChatRoom(rr, convRequest(t, http.MethodGet, "/api/admin/chatrooms/rt_missing", "", convReadOnly))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rr.Code, rr.Body.String())
	}
}

func TestClawAdminChatRoomMessagesMapsEventRows(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM claw_admin_users"):
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["group:read"]`)}}
		case strings.Contains(sql, "FROM (") && strings.Contains(sql, "rongcloud_chatroom c"):
			return &convStubRow{values: chatRoomRow()}
		}
		panic("unexpected QueryRow: " + sql)
	}
	created := time.Date(2026, 10, 15, 6, 39, 9, 0, time.UTC)
	fake.query = func(sql string, _ []any) (pgx.Rows, error) {
		if !strings.Contains(sql, "FROM rongcloud_discussion_event e") {
			panic("unexpected Query: " + sql)
		}
		return &convStubRows{rows: [][]any{
			{"f0a6c2a7-2276-59e6-9f18-c839706281d9", "contribution_completed", "rc_node_3300776275_hermes",
				"质疑者", "质疑者", "我总觉得大家聊AI都在聊模型多聪明", 3, created},
		}}, nil
	}

	rr := httptest.NewRecorder()
	h.ClawAdminChatRoomMessages(rr, convRequest(t, http.MethodGet,
		"/api/admin/chatrooms/rt_2de41fc1-50d3-5064-95e7-4e3f50febe23/messages", "", convReadOnly))

	body := decodeConv(t, rr)
	items := dataList(t, body)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	item := items[0].(map[string]interface{})
	if item["author_label"] != "质疑者" || item["author_role"] != "质疑者" {
		t.Fatalf("author fields = %#v", item)
	}
	if item["room_id"] != "rt_2de41fc1-50d3-5064-95e7-4e3f50febe23" {
		t.Fatalf("room_id = %v", item["room_id"])
	}
	if item["content"] == "" {
		t.Fatalf("content should be populated: %#v", item)
	}
	// 轮次必须从 payload 兜底取：新链路不写 round_number 列，只读列会让界面上
	// 的轮次永远是 0（线上 214 行事件全部如此）。
	if !fake.sawStatement("e.payload->>'round'") {
		t.Fatalf("round must fall back to payload; statements: %v", fake.stmts)
	}
}

// ---- 聊天室删除（软删） -----------------------------------------------------

func TestClawAdminDeleteChatRoomSoftDeletesAndAudits(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["group:write"]`)}}
	}
	var updateSQL string
	fake.exec = func(sql string, _ []any) (pgconn.CommandTag, error) {
		if strings.Contains(sql, "UPDATE rongcloud_chatroom") {
			updateSQL = sql
			return pgconn.NewCommandTag("UPDATE 1"), nil
		}
		if strings.Contains(sql, "claw_admin_audit_logs") {
			return pgconn.NewCommandTag("INSERT 0 1"), nil
		}
		panic("unexpected Exec: " + sql)
	}

	rr := httptest.NewRecorder()
	h.ClawAdminDeleteChatRoom(rr, convRequest(t, http.MethodDelete, "/api/admin/chatrooms/rt_x", "", convReadOnly))

	body := decodeConv(t, rr)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if deleted, _ := dataMap(t, body)["deleted"].(bool); !deleted {
		t.Fatalf("deleted flag = %#v", dataMap(t, body)["deleted"])
	}
	if !strings.Contains(updateSQL, "status = 'deleted'") {
		t.Fatalf("delete must be a soft delete, got %s", updateSQL)
	}
	if !fake.sawStatement("claw_admin_audit_logs") {
		t.Fatalf("delete did not write an audit row: %v", fake.stmts)
	}
	args := fake.argsFor(t, "UPDATE rongcloud_chatroom")
	if len(args) != 1 || args[0] != "rt_x" {
		t.Fatalf("delete args = %v, want the room id", args)
	}
}

func TestClawAdminDeleteChatRoomMissingIs404(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(string, []any) pgx.Row {
		return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["group:write"]`)}}
	}
	fake.exec = func(sql string, _ []any) (pgconn.CommandTag, error) {
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	rr := httptest.NewRecorder()
	h.ClawAdminDeleteChatRoom(rr, convRequest(t, http.MethodDelete, "/api/admin/chatrooms/rt_missing", "", convReadOnly))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// ---- 消息查询 ---------------------------------------------------------------

func TestClawAdminMessagesReturnsArchiveRows(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	created := time.Date(2026, 10, 15, 6, 39, 45, 0, time.UTC)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		if strings.Contains(sql, "FROM claw_admin_users") {
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["message:read"]`)}}
		}
		if strings.Contains(sql, "SELECT COUNT(*)") {
			return &convStubRow{values: []any{42}}
		}
		panic("unexpected QueryRow: " + sql)
	}
	fake.query = func(sql string, _ []any) (pgx.Rows, error) {
		if !strings.Contains(sql, "rongcloud_discussion_event e") {
			panic("unexpected Query: " + sql)
		}
		return &convStubRows{rows: [][]any{
			{"evt:ceec2b0f", "rt_2de41fc1", "ceec2b0f", "opencode_1ziviig73i", "", "第三轮我想聊聊政府", "jsonb",
				"contribution_completed", "chatroom", []byte(`{"round":3}`), created},
		}}, nil
	}

	rr := httptest.NewRecorder()
	h.ClawAdminMessages(rr, convRequest(t, http.MethodGet, "/api/admin/messages?page=1&page_size=10", "", convReadOnly))

	body := decodeConv(t, rr)
	if total := dataMap(t, body)["total"].(float64); total != 42 {
		t.Fatalf("total = %v, want 42", total)
	}
	item := firstItem(t, body)
	if item["message_type"] != "contribution_completed" || item["conversation_type"] != "chatroom" {
		t.Fatalf("item = %#v", item)
	}
	metadata, ok := item["metadata"].(map[string]interface{})
	if !ok || metadata["round"] != float64(3) {
		t.Fatalf("metadata should pass through as an object, got %#v", item["metadata"])
	}
	if !strings.HasPrefix(item["message_id"].(string), "evt:") {
		t.Fatalf("message_id = %v", item["message_id"])
	}
}

func TestClawAdminMessagesKeywordBindsAPattern(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		if strings.Contains(sql, "FROM claw_admin_users") {
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["message:read"]`)}}
		}
		return &convStubRow{values: []any{0}}
	}
	fake.query = func(string, []any) (pgx.Rows, error) { return &convStubRows{}, nil }

	rr := httptest.NewRecorder()
	h.ClawAdminMessages(rr, convRequest(t, http.MethodGet,
		"/api/admin/messages?keyword=AI&sender_id=850509&message_type=user_interjection", "", convReadOnly))

	args := fake.argsFor(t, "SELECT COUNT(*)")
	if len(args) != 3 {
		t.Fatalf("count args = %v, want 3 filters", args)
	}
	if args[0] != "850509" || args[1] != "user_interjection" || args[2] != "%AI%" {
		t.Fatalf("args = %v", args)
	}
}

func TestClawAdminMessagesRejectsUnsupportedConversationTypeWithoutQuerying(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(string, []any) pgx.Row {
		return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["message:read"]`)}}
	}
	rr := httptest.NewRecorder()
	h.ClawAdminMessages(rr, convRequest(t, http.MethodGet, "/api/admin/messages?conversation_type=single", "", convReadOnly))

	body := decodeConv(t, rr)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if total := dataMap(t, body)["total"].(float64); total != 0 {
		t.Fatalf("total = %v, want 0", total)
	}
	if fake.sawStatement("SELECT COUNT(*)") {
		t.Fatalf("unsupported conversation_type should not hit the archive table")
	}
}

// ---- 讨论组 -----------------------------------------------------------------

func discussionRow() []any {
	created := time.Date(2026, 10, 15, 6, 15, 22, 0, time.UTC)
	lastEvent := time.Date(2026, 10, 15, 6, 39, 45, 0, time.UTC)
	maxRounds := 5
	return []any{
		"ceec2b0f-8580-5e49-a4e6-b669a1072dc8", "rt_2de41fc1-50d3-5064-95e7-4e3f50febe23",
		"ai这两年可以发展到什么程度？讨论组", "ai这两年可以发展到什么程度？", "形成共识清单",
		"awaiting_user", "roundtable", 3, &maxRounds, "legacy_capped", "850509", 7,
		214, 3, lastEvent, created, lastEvent,
	}
}

func TestClawAdminListDiscussionsProjectsRow(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM claw_admin_users"):
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["group:read"]`)}}
		case strings.Contains(sql, "SELECT COUNT(*)"):
			return &convStubRow{values: []any{1}}
		}
		panic("unexpected QueryRow: " + sql)
	}
	fake.query = func(sql string, _ []any) (pgx.Rows, error) {
		if !strings.Contains(sql, "FROM rongcloud_discussion d") {
			panic("unexpected Query: " + sql)
		}
		return &convStubRows{rows: [][]any{discussionRow()}}, nil
	}

	rr := httptest.NewRecorder()
	h.ClawAdminListDiscussions(rr, convRequest(t, http.MethodGet, "/api/admin/discussions", "", convReadOnly))

	item := firstItem(t, decodeConv(t, rr))
	checks := map[string]interface{}{
		"discussion_id":  "ceec2b0f-8580-5e49-a4e6-b669a1072dc8",
		"chatroom_name":  "ai这两年可以发展到什么程度？讨论组",
		"status":         "awaiting_user",
		"mode":           "roundtable",
		"current_round":  float64(3),
		"max_rounds":     float64(5),
		"round_policy":   "legacy_capped",
		"created_by":     "850509",
		"event_count":    float64(214),
		"document_count": float64(3),
	}
	for key, want := range checks {
		if got := item[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
}

func TestClawAdminListDiscussionsRejectsUnknownStatus(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	rr := httptest.NewRecorder()
	h.ClawAdminListDiscussions(rr, convRequest(t, http.MethodGet, "/api/admin/discussions?status=nope", "", convReadOnly))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestClawAdminDiscussionEventsRequireExistingDiscussion(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		if strings.Contains(sql, "FROM claw_admin_users") {
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["group:read"]`)}}
		}
		return &convStubRow{err: pgx.ErrNoRows}
	}
	rr := httptest.NewRecorder()
	h.ClawAdminDiscussionEvents(rr, convRequest(t, http.MethodGet,
		"/api/admin/discussions/missing/events", "", convReadOnly))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestClawAdminDiscussionEventsMapsTimeline(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM claw_admin_users"):
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["group:read"]`)}}
		case strings.Contains(sql, "FROM (") && strings.Contains(sql, "rongcloud_discussion d"):
			return &convStubRow{values: discussionRow()}
		}
		panic("unexpected QueryRow: " + sql)
	}
	created := time.Date(2026, 10, 15, 6, 36, 50, 0, time.UTC)
	seq := int64(30)
	fake.query = func(sql string, _ []any) (pgx.Rows, error) {
		if !strings.Contains(sql, "FROM rongcloud_discussion_event") {
			panic("unexpected Query: " + sql)
		}
		return &convStubRows{rows: [][]any{
			{"1f0e...", &seq, "round_started", "850509", "", 3, "", []byte(`{"round":3}`), created},
		}}, nil
	}

	rr := httptest.NewRecorder()
	h.ClawAdminDiscussionEvents(rr, convRequest(t, http.MethodGet,
		"/api/admin/discussions/ceec2b0f-8580-5e49-a4e6-b669a1072dc8/events", "", convReadOnly))

	items := dataList(t, decodeConv(t, rr))
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	event := items[0].(map[string]interface{})
	if event["event_type"] != "round_started" || event["actor_id"] != "850509" {
		t.Fatalf("event = %#v", event)
	}
	if payload, ok := event["payload"].(map[string]interface{}); !ok || payload["round"] != float64(3) {
		t.Fatalf("payload = %#v", event["payload"])
	}
	if !fake.sawStatement("e.payload->>'round'") {
		t.Fatalf("timeline round must fall back to payload; statements: %v", fake.stmts)
	}
}

func TestClawAdminDiscussionRoundDocumentsMapsRows(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM claw_admin_users"):
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["group:read"]`)}}
		case strings.Contains(sql, "FROM (") && strings.Contains(sql, "rongcloud_discussion d"):
			return &convStubRow{values: discussionRow()}
		}
		panic("unexpected QueryRow: " + sql)
	}
	created := time.Date(2026, 10, 15, 6, 17, 27, 0, time.UTC)
	fake.query = func(sql string, _ []any) (pgx.Rows, error) {
		if !strings.Contains(sql, "FROM rongcloud_discussion_round_document") {
			panic("unexpected Query: " + sql)
		}
		return &convStubRows{rows: [][]any{
			{"bc8c390d-c2d1-451e-8934-aeb098ae5731", 1, 0, "Round 1 summary", "本轮纪要正文", created},
		}}, nil
	}

	rr := httptest.NewRecorder()
	h.ClawAdminDiscussionRoundDocuments(rr, convRequest(t, http.MethodGet,
		"/api/admin/discussions/ceec2b0f-8580-5e49-a4e6-b669a1072dc8/round-documents", "", convReadOnly))

	items := dataList(t, decodeConv(t, rr))
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	doc := items[0].(map[string]interface{})
	if doc["title"] != "Round 1 summary" || doc["content"] != "本轮纪要正文" || doc["round"] != float64(1) {
		t.Fatalf("document = %#v", doc)
	}
}

// ---- 数据看板 ---------------------------------------------------------------

// 看板以前把节点/群组/消息全写死成 0，即使库里有 11 台设备、1 个群、214 条事件。
// 这个用例锁住「数字来自表而不是常量」。
func TestClawAdminStatsReadsRealCounts(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, []string{"stats:read"})
	created := time.Date(2026, 10, 10, 8, 34, 58, 0, time.UTC)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM claw_admin_users"):
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["stats:read"]`)}}
		case strings.Contains(sql, "FROM claw_im_users WHERE user_id"), strings.Contains(sql, "FROM rongcloud_node WHERE node_id"):
			return &convStubRow{err: pgx.ErrNoRows}
		case strings.Contains(sql, "COUNT(*) FROM claw_im_users") && strings.Contains(sql, "status = 'active'"):
			return &convStubRow{values: []any{6}}
		case strings.Contains(sql, "COUNT(*) FROM claw_im_users") && strings.Contains(sql, "om_"):
			return &convStubRow{values: []any{3}}
		case strings.Contains(sql, "COUNT(*) FROM claw_im_users"):
			return &convStubRow{values: []any{7}}
		case strings.Contains(sql, "COUNT(*) FROM rongcloud_group"):
			return &convStubRow{values: []any{1}}
		case strings.Contains(sql, "COUNT(*) FROM rongcloud_chatroom"):
			return &convStubRow{values: []any{9}}
		case strings.Contains(sql, "COUNT(*) FROM rongcloud_discussion_event"):
			return &convStubRow{values: []any{214}}
		case strings.Contains(sql, "COUNT(*) FROM rongcloud_discussion_round_document"):
			return &convStubRow{values: []any{13}}
		case strings.Contains(sql, "COUNT(*) FROM rongcloud_discussion"):
			return &convStubRow{values: []any{10}}
		case strings.Contains(sql, "COUNT(*) FROM claw_admin_audit_logs"):
			return &convStubRow{values: []any{17}}
		case strings.Contains(sql, "FROM rongcloud_system_config"):
			return &convStubRow{err: pgx.ErrNoRows}
		}
		panic("unexpected QueryRow: " + sql)
	}
	fake.query = func(sql string, _ []any) (pgx.Rows, error) {
		switch {
		case strings.Contains(sql, "FROM rongcloud_node"):
			return &convStubRows{rows: [][]any{
				// 可见 agent 节点
				{pgtype.UUID{}, "node_a", "rc_node_3300776275_opencode", "opencode", "", []byte(`[]`), "offline", created, created},
				// 内置 ops 运维节点：不进用户可见列表
				{pgtype.UUID{}, "node_b", "rc_node_3300776275_ops", "ops", "", []byte(`[]`), "offline", created, created},
				{pgtype.UUID{}, "node_c", "rc_node_3300776275_hermes", "hermes", "", []byte(`[]`), "offline", created, created},
			}}, nil
		case strings.Contains(sql, "FROM rongcloud_user"):
			return &convStubRows{}, nil
		}
		panic("unexpected Query: " + sql)
	}

	rr := httptest.NewRecorder()
	h.ClawAdminStats(rr, convRequest(t, http.MethodGet, "/api/admin/stats", "", []string{"stats:read"}))

	stats := dataMap(t, decodeConv(t, rr))
	checks := map[string]float64{
		"users":           7,
		"normal_users":    6,
		"operation_users": 3,
		"nodes":           2, // ops 节点被 IsDisplayNode 过滤掉
		"groups":          1,
		"chatrooms":       9,
		"discussions":     10,
		"messages":        214,
	}
	for key, want := range checks {
		got, ok := stats[key].(float64)
		if !ok {
			t.Errorf("%s missing from stats payload", key)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	providers, ok := stats["provider_nodes"].(map[string]interface{})
	if !ok {
		t.Fatalf("provider_nodes = %#v", stats["provider_nodes"])
	}
	if providers["opencode"] != float64(1) || providers["hermes"] != float64(1) {
		t.Fatalf("provider_nodes = %#v", providers)
	}
	if _, leaked := providers["ops"]; leaked {
		t.Fatalf("ops node must not appear in provider_nodes: %#v", providers)
	}
}

// ---- 用户详情的节点 / 群组 ---------------------------------------------------

func TestClawAdminUserGroupsListsMirroredGroups(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	created := time.Date(2026, 10, 13, 7, 7, 25, 0, time.UTC)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM claw_admin_users"):
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["user:read"]`)}}
		case strings.Contains(sql, "FROM claw_im_users WHERE user_id"):
			return &convStubRow{values: []any{"850509"}}
		}
		panic("unexpected QueryRow: " + sql)
	}
	fake.query = func(sql string, _ []any) (pgx.Rows, error) {
		if !strings.Contains(sql, "FROM rongcloud_group g") {
			panic("unexpected Query: " + sql)
		}
		return &convStubRows{rows: [][]any{
			{"782523", "测试群1", "850509", 5, "", created, created},
		}}, nil
	}

	rr := httptest.NewRecorder()
	h.ClawAdminUserGroups(rr, convRequest(t, http.MethodGet, "/api/admin/users/850509/groups", "", convReadOnly))

	items := dataList(t, decodeConv(t, rr))
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	group := items[0].(map[string]interface{})
	if group["group_id"] != "782523" || group["group_name"] != "测试群1" || group["member_count"] != float64(5) {
		t.Fatalf("group = %#v", group)
	}
}

func TestClawAdminUserGroupsUnknownUserIs404(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		if strings.Contains(sql, "FROM claw_admin_users") {
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["user:read"]`)}}
		}
		return &convStubRow{err: pgx.ErrNoRows}
	}
	rr := httptest.NewRecorder()
	h.ClawAdminUserGroups(rr, convRequest(t, http.MethodGet, "/api/admin/users/nope/groups", "", convReadOnly))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestClawAdminUserNodesWithoutRongCloudDegradesToEmpty(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM claw_admin_users"):
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["user:read"]`)}}
		case strings.Contains(sql, "FROM claw_im_users WHERE user_id"):
			return &convStubRow{values: []any{"850509"}}
		}
		panic("unexpected QueryRow: " + sql)
	}

	rr := httptest.NewRecorder()
	h.ClawAdminUserNodes(rr, convRequest(t, http.MethodGet, "/api/admin/users/850509/nodes", "", convReadOnly))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (integration absent must not 500)", rr.Code)
	}
	if items := dataList(t, decodeConv(t, rr)); len(items) != 0 {
		t.Fatalf("items = %#v, want empty", items)
	}
}

// ---- 用户管理的账号类型 / AI 类型筛选 ---------------------------------------

func TestClawAdminListUsersFiltersOnDerivedColumns(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	fake.queryRow = func(sql string, _ []any) pgx.Row {
		if strings.Contains(sql, "FROM claw_admin_users") {
			return &convStubRow{values: []any{convAdminID, "boss", "老板", "role-1", "active", "超级管理员", []byte(`["user:read"]`)}}
		}
		return &convStubRow{values: []any{0}}
	}
	fake.query = func(string, []any) (pgx.Rows, error) { return &convStubRows{}, nil }

	rr := httptest.NewRecorder()
	h.ClawAdminListUsers(rr, convRequest(t, http.MethodGet,
		"/api/admin/users?account_type=operation&ai_type=openclaw", "", convReadOnly))

	countSQL := ""
	for _, sql := range fake.stmts {
		if strings.Contains(sql, "SELECT COUNT(*)") {
			countSQL = sql
		}
	}
	if !strings.Contains(countSQL, "starts_with(u.user_id, 'om_')") {
		t.Fatalf("account_type filter not applied: %s", countSQL)
	}
	if !strings.Contains(countSQL, "n.ai_type = $1") {
		t.Fatalf("ai_type filter not applied: %s", countSQL)
	}
}

func TestClawAdminListUsersRejectsUnknownAccountType(t *testing.T) {
	fake := &convFakeDB{}
	h := newConvHandler(fake, convReadOnly)
	rr := httptest.NewRecorder()
	h.ClawAdminListUsers(rr, convRequest(t, http.MethodGet, "/api/admin/users?account_type=weird", "", convReadOnly))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}
