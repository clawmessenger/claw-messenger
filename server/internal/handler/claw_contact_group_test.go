package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---- 脚本化假 DB ------------------------------------------------------------
//
// 与 claw_admin_groups_test.go 同一套思路：每个用例只声明它关心的那几条语句，
// 未声明的分支 panic（"忘了打桩"不能变成静默通过）。
//
// 这里额外强调一条：**鉴权查询也是走 DB**（clawRequireClawUser 查
// claw_im_users.rongcloud_token），所以每个用例的 queryRow 都必须先接住它，
// 否则会 panic 在鉴权上，测试失败信息会指向错误的行。

const cgTestUserID = "user-cg-1"

// 路径参数是 uuid 列，用例统一用这个合法值（用 "g-ops" 这种可读 id 会被
// parseContactGroupID 拦成 400，测不到想测的分支）。
const cgGroupUUID = "77777777-7777-7777-7777-777777777777"

type cgFakeDB struct {
	queryRow func(sql string, args []any) pgx.Row
	query    func(sql string, args []any) (pgx.Rows, error)
	exec     func(sql string, args []any) (pgconn.CommandTag, error)
	// 记录真正执行过的 SQL 前缀，用于断言「默认分组懒创建」这类副作用。
	execs    []string
	queries  []string
	rowCalls []string
}

func (f *cgFakeDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.queryRow == nil {
		panic("cgFakeDB.QueryRow not stubbed: " + sql)
	}
	f.rowCalls = append(f.rowCalls, sql)
	return f.queryRow(sql, args)
}

func (f *cgFakeDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if f.query == nil {
		panic("cgFakeDB.Query not stubbed: " + sql)
	}
	f.queries = append(f.queries, sql)
	return f.query(sql, args)
}

func (f *cgFakeDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execs = append(f.execs, sql)
	if f.exec == nil {
		// 默认分组懒创建是「每次写之前都跑一次」的幂等语句，用例大多不关心它的
		// 返回值，所以不强制打桩；但调用记录要留下。
		return pgconn.NewCommandTag("INSERT 0 2"), nil
	}
	return f.exec(sql, args)
}

// cgRow 是 pgx.Row 的最小实现：一个固定值切片或一个错误。
type cgRow struct {
	values []any
	err    error
}

func (r *cgRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return cgAssign(dest, r.values)
}

func cgRowOf(values ...any) *cgRow { return &cgRow{values: values} }
func cgRowErr(err error) *cgRow    { return &cgRow{err: err} }

type cgStubRows struct {
	pgx.Rows
	rows [][]any
	idx  int
	err  error
}

func (r *cgStubRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *cgStubRows) Scan(dest ...any) error {
	if r.idx == 0 || r.idx > len(r.rows) {
		return errors.New("Scan called outside of Next")
	}
	return cgAssign(dest, r.rows[r.idx-1])
}

func (r *cgStubRows) Close()     {}
func (r *cgStubRows) Err() error { return r.err }

func cgAssign(dest []any, values []any) error {
	if len(dest) != len(values) {
		return fmt.Errorf("scan: %d dest vs %d values", len(dest), len(values))
	}
	for i := range dest {
		// sql.NullString 在 pgx 里是常见 Scan 目标（可空列），reflect 的
		// Assignable/Convertible 都接不住 string → NullString，这里显式兜住。
		if nullStr, ok := dest[i].(*sql.NullString); ok {
			if values[i] == nil {
				*nullStr = sql.NullString{}
				continue
			}
			str, isStr := values[i].(string)
			if !isStr {
				return fmt.Errorf("scan: cannot assign %T to sql.NullString", values[i])
			}
			*nullStr = sql.NullString{String: str, Valid: true}
			continue
		}
		target := reflect.ValueOf(dest[i])
		if target.Kind() != reflect.Ptr || target.IsNil() {
			return fmt.Errorf("scan: dest %d is not a non-nil pointer", i)
		}
		elem := target.Elem()
		if values[i] == nil {
			elem.Set(reflect.Zero(elem.Type()))
			continue
		}
		value := reflect.ValueOf(values[i])
		switch {
		case value.Type().AssignableTo(elem.Type()):
			elem.Set(value)
		case value.Type().ConvertibleTo(elem.Type()):
			elem.Set(value.Convert(elem.Type()))
		default:
			return fmt.Errorf("scan: cannot assign %T to %s", values[i], elem.Type())
		}
	}
	return nil
}

// ---- 请求脚手架 -------------------------------------------------------------

// cgAuthedDB 返回一个假 DB：鉴权查询固定回 cgTestUserID，其余语句交给 stub。
func cgAuthedDB(t *testing.T) *cgFakeDB {
	t.Helper()
	return &cgFakeDB{
		queryRow: func(sql string, args []any) pgx.Row {
			if strings.Contains(sql, "claw_im_users") {
				return cgRowOf(cgTestUserID)
			}
			panic("unexpected QueryRow: " + sql)
		},
	}
}

func cgRequest(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token-cg")
	return req
}

// cgWithID 往 chi 的 RouteCtx 里塞 {id}，不用起完整 router。
func cgWithID(req *http.Request, id string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func cgDecode(t *testing.T, rec *httptest.ResponseRecorder) (code int, data map[string]any) {
	t.Helper()
	var envelope struct {
		Code int                    `json:"code"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	return envelope.Code, envelope.Data
}

// ---- 用例 ------------------------------------------------------------------

// 列表必须把两个默认分组补齐 —— 前端第一次进通讯录就靠这一条拿到「运维」
// 和「我的 Agent」的 id（后面改名要用），少了它用户连改名的入口都没有。
func TestClawListContactGroups_EnsuresDefaultsAndListsMembers(t *testing.T) {
	db := cgAuthedDB(t)
	db.query = func(sql string, args []any) (pgx.Rows, error) {
		if !strings.Contains(sql, "FROM im_contact_group g") {
			t.Fatalf("unexpected Query: %s", sql)
		}
		return &cgStubRows{rows: [][]any{
			{"g-ops", "运维", "maintenance", 0, []string{}},
			{"g-mine", "我的 Agent", "my_agent", 1, []string{"node_a", "node_b"}},
			{"g-work", "工作", "", 2, []string{"node_c"}},
		}}, nil
	}
	h := &Handler{DB: db}

	rec := httptest.NewRecorder()
	h.ClawListContactGroups(rec, cgRequest(t, http.MethodGet, "/api/claw/contact-groups", ""))

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	code, data := cgDecode(t, rec)
	if code != 200 {
		t.Fatalf("envelope code = %d", code)
	}
	raw, _ := json.Marshal(data["groups"])
	var groups []contactGroupDTO
	if err := json.Unmarshal(raw, &groups); err != nil {
		t.Fatalf("decode groups: %v", err)
	}
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3", len(groups))
	}
	if groups[0].Slot != contactGroupSlotMaintenance || groups[1].Slot != contactGroupSlotMyAgent {
		t.Fatalf("default slots = %q,%q", groups[0].Slot, groups[1].Slot)
	}
	if groups[2].Slot != "" {
		t.Fatalf("custom group slot = %q, want empty", groups[2].Slot)
	}
	if len(groups[1].Members) != 2 || groups[1].Members[0] != "node_a" {
		t.Fatalf("members = %v", groups[1].Members)
	}
	// 默认分组懒创建必须真的跑过，否则第一次进通讯录会看到空列表。
	if len(db.execs) == 0 || !strings.Contains(db.execs[0], "ON CONFLICT") {
		t.Fatalf("defaults not ensured, execs = %v", db.execs)
	}
}

// 未登录必须是 401 且**不碰业务表**：鉴权失败时还去建默认分组，等于给
// 匿名请求写库。
func TestClawListContactGroups_RequiresAuthBeforeTouchingDB(t *testing.T) {
	db := &cgFakeDB{}
	h := &Handler{DB: db}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/claw/contact-groups", nil)
	h.ClawListContactGroups(rec, req)

	if rec.Code != 401 {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(db.execs) != 0 || len(db.queries) != 0 {
		t.Fatalf("unauthenticated request touched DB: execs=%v queries=%v", db.execs, db.queries)
	}
}

// 中文名按字符数计长：24 个汉字是允许的，用 len() 会误判成 72 字节超限。
func TestNormalizeContactGroupName_CountsRunesNotBytes(t *testing.T) {
	ok := strings.Repeat("运", 24)
	if name, valid := normalizeContactGroupName(" " + ok + " "); !valid || name != ok {
		t.Fatalf("24 个汉字应通过，got %q valid=%v", name, valid)
	}
	if _, valid := normalizeContactGroupName(strings.Repeat("运", 25)); valid {
		t.Fatal("25 个汉字应被拒")
	}
	if _, valid := normalizeContactGroupName("   "); valid {
		t.Fatal("全空白应被拒")
	}
}

func TestClawCreateContactGroup_RejectsBlankName(t *testing.T) {
	db := cgAuthedDB(t)
	h := &Handler{DB: db}

	rec := httptest.NewRecorder()
	h.ClawCreateContactGroup(rec, cgRequest(t, http.MethodPost, "/api/claw/contact-groups", `{"name":"   "}`))

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	// 名字都没过校验就写默认分组，是多余的一次写库。
	if len(db.execs) != 0 {
		t.Fatalf("blank name touched DB: %v", db.execs)
	}
}

// 重名返回 409 而不是 500：前端要据此提示「分组名已存在」，而不是「服务器错误」。
func TestClawCreateContactGroup_MapsUniqueViolationToConflict(t *testing.T) {
	db := cgAuthedDB(t)
	db.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "claw_im_users") {
			return cgRowOf(cgTestUserID)
		}
		return cgRowErr(&pgconn.PgError{Code: "23505", Message: "duplicate key"})
	}
	h := &Handler{DB: db}

	rec := httptest.NewRecorder()
	h.ClawCreateContactGroup(rec, cgRequest(t, http.MethodPost, "/api/claw/contact-groups", `{"name":"工作"}`))

	if rec.Code != 409 {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestClawCreateContactGroup_ReturnsGroupWithEmptyMembers(t *testing.T) {
	db := cgAuthedDB(t)
	db.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "claw_im_users") {
			return cgRowOf(cgTestUserID)
		}
		return cgRowOf("g-new", "工作", 2)
	}
	h := &Handler{DB: db}

	rec := httptest.NewRecorder()
	h.ClawCreateContactGroup(rec, cgRequest(t, http.MethodPost, "/api/claw/contact-groups", `{"name":"工作"}`))

	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	_, data := cgDecode(t, rec)
	group, _ := data["group"].(map[string]any)
	if group["name"] != "工作" {
		t.Fatalf("group = %v", group)
	}
	// members 必须是 []，不能是 null —— 前端直接 .map 会炸。
	members, ok := group["members"].([]any)
	if !ok || len(members) != 0 {
		t.Fatalf("members = %#v, want empty array", group["members"])
	}
}

// 改名是「运维分组支持自定义名称」的实现，默认分组也必须能改。
func TestClawUpdateContactGroup_RenamesDefaultGroup(t *testing.T) {
	db := cgAuthedDB(t)
	db.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "claw_im_users") {
			return cgRowOf(cgTestUserID)
		}
		return cgRowOf(cgGroupUUID, "设备运维", "maintenance", 0)
	}
	h := &Handler{DB: db}

	req := cgWithID(cgRequest(t, http.MethodPut, "/api/claw/contact-groups/"+cgGroupUUID, `{"name":"设备运维"}`), cgGroupUUID)
	rec := httptest.NewRecorder()
	h.ClawUpdateContactGroup(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	_, data := cgDecode(t, rec)
	group, _ := data["group"].(map[string]any)
	if group["name"] != "设备运维" || group["slot"] != "maintenance" {
		t.Fatalf("group = %v", group)
	}
}

func TestClawUpdateContactGroup_RejectsMalformedID(t *testing.T) {
	db := cgAuthedDB(t)
	h := &Handler{DB: db}

	req := cgWithID(cgRequest(t, http.MethodPut, "/api/claw/contact-groups/not-a-uuid", `{"name":"x"}`), "not-a-uuid")
	rec := httptest.NewRecorder()
	h.ClawUpdateContactGroup(rec, req)

	// 非法 uuid 若直接进 SQL 会变成 500，日志里看着像服务端故障。
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestClawUpdateContactGroup_NotFound(t *testing.T) {
	db := cgAuthedDB(t)
	db.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "claw_im_users") {
			return cgRowOf(cgTestUserID)
		}
		return cgRowErr(pgx.ErrNoRows)
	}
	h := &Handler{DB: db}

	id := "11111111-1111-1111-1111-111111111111"
	req := cgWithID(cgRequest(t, http.MethodPut, "/api/claw/contact-groups/"+id, `{"name":"x"}`), id)
	rec := httptest.NewRecorder()
	h.ClawUpdateContactGroup(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// 默认分组删不掉，且必须给 400 + 明确文案 —— 这才是「操作不被允许」，
// 回 404「分组不存在」会让用户以为自己的分组莫名消失了。
func TestClawDeleteContactGroup_RefusesDefaultGroup(t *testing.T) {
	db := cgAuthedDB(t)
	db.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "claw_im_users") {
			return cgRowOf(cgTestUserID)
		}
		return cgRowOf("maintenance")
	}
	h := &Handler{DB: db}

	id := "22222222-2222-2222-2222-222222222222"
	req := cgWithID(cgRequest(t, http.MethodDelete, "/api/claw/contact-groups/"+id, ""), id)
	rec := httptest.NewRecorder()
	h.ClawDeleteContactGroup(rec, req)

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "默认分组不能删除") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestClawDeleteContactGroup_DeletesCustomGroup(t *testing.T) {
	db := cgAuthedDB(t)
	step := 0
	db.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "claw_im_users") {
			return cgRowOf(cgTestUserID)
		}
		step++
		if step == 1 {
			// SELECT slot：自定义分组 slot 为 NULL
			return cgRowOf(nil)
		}
		return cgRowOf("33333333-3333-3333-3333-333333333333")
	}
	h := &Handler{DB: db}

	id := "33333333-3333-3333-3333-333333333333"
	req := cgWithID(cgRequest(t, http.MethodDelete, "/api/claw/contact-groups/"+id, ""), id)
	rec := httptest.NewRecorder()
	h.ClawDeleteContactGroup(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestClawDeleteContactGroup_NotFound(t *testing.T) {
	db := cgAuthedDB(t)
	db.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "claw_im_users") {
			return cgRowOf(cgTestUserID)
		}
		return cgRowErr(pgx.ErrNoRows)
	}
	h := &Handler{DB: db}

	id := "44444444-4444-4444-4444-444444444444"
	req := cgWithID(cgRequest(t, http.MethodDelete, "/api/claw/contact-groups/"+id, ""), id)
	rec := httptest.NewRecorder()
	h.ClawDeleteContactGroup(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// 把设备移进别人的分组：SQL 的 SELECT 源天然插入 0 行 → 404。
// 这条是越权防线，回 403「无权」反而会泄露「这个分组存在」。
func TestClawAssignContactGroup_ForeignGroupIsNotFound(t *testing.T) {
	db := cgAuthedDB(t)
	db.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "claw_im_users") {
			return cgRowOf(cgTestUserID)
		}
		return cgRowErr(pgx.ErrNoRows)
	}
	h := &Handler{DB: db}

	body := `{"nodeId":"node_a","groupId":"55555555-5555-5555-5555-555555555555"}`
	rec := httptest.NewRecorder()
	h.ClawAssignContactGroup(rec, cgRequest(t, http.MethodPut, "/api/claw/contact-groups/assignment", body))

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// groupId 为空 = 移出分组（回到默认「我的 Agent」）。源码里这里会复用
// nodeID 变量做 Scan 目标，扫不到行（本来就没分组）也必须回成功 —— 幂等。
func TestClawAssignContactGroup_EmptyGroupUnassignsIdempotently(t *testing.T) {
	db := cgAuthedDB(t)
	db.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "claw_im_users") {
			return cgRowOf(cgTestUserID)
		}
		if !strings.Contains(sql, "DELETE FROM im_contact_group_member") {
			t.Fatalf("unexpected QueryRow for unassign: %s", sql)
		}
		return cgRowErr(pgx.ErrNoRows)
	}
	h := &Handler{DB: db}

	rec := httptest.NewRecorder()
	h.ClawAssignContactGroup(rec, cgRequest(t, http.MethodPut, "/api/claw/contact-groups/assignment", `{"nodeId":"node_a"}`))

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	_, data := cgDecode(t, rec)
	if data["nodeId"] != "node_a" || data["groupId"] != "" {
		t.Fatalf("data = %v", data)
	}
}

func TestClawAssignContactGroup_RequiresNodeID(t *testing.T) {
	db := cgAuthedDB(t)
	h := &Handler{DB: db}

	rec := httptest.NewRecorder()
	h.ClawAssignContactGroup(rec, cgRequest(t, http.MethodPut, "/api/claw/contact-groups/assignment", `{"groupId":"55555555-5555-5555-5555-555555555555"}`))

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestClawAssignContactGroup_AssignsNode(t *testing.T) {
	db := cgAuthedDB(t)
	db.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "claw_im_users") {
			return cgRowOf(cgTestUserID)
		}
		if !strings.Contains(sql, "INSERT INTO im_contact_group_member") {
			t.Fatalf("unexpected QueryRow: %s", sql)
		}
		return cgRowOf("node_a")
	}
	h := &Handler{DB: db}

	body := `{"nodeId":"node_a","groupId":"66666666-6666-6666-6666-666666666666"}`
	rec := httptest.NewRecorder()
	h.ClawAssignContactGroup(rec, cgRequest(t, http.MethodPut, "/api/claw/contact-groups/assignment", body))

	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	_, data := cgDecode(t, rec)
	if data["nodeId"] != "node_a" || data["groupId"] != "66666666-6666-6666-6666-666666666666" {
		t.Fatalf("data = %v", data)
	}
}
