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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/go-chi/chi/v5"
)

// ---- 脚本化假 DB ------------------------------------------------------------

// groupFakeDB 让每个用例只声明它关心的那一条语句。未声明的分支直接 panic，
// 免得"忘了打桩"变成静默通过。
type groupFakeDB struct {
	queryRow func(sql string, args []any) pgx.Row
	query    func(sql string, args []any) (pgx.Rows, error)
	exec     func(sql string, args []any) (pgconn.CommandTag, error)
	execs    []string
	argsSeen map[string][]any
}

func (f *groupFakeDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.queryRow == nil {
		panic("groupFakeDB.QueryRow not stubbed: " + sql)
	}
	f.record(sql, args)
	return f.queryRow(sql, args)
}

func (f *groupFakeDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if f.query == nil {
		panic("groupFakeDB.Query not stubbed: " + sql)
	}
	f.record(sql, args)
	return f.query(sql, args)
}

func (f *groupFakeDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execs = append(f.execs, sql)
	f.record(sql, args)
	if f.exec == nil {
		// 审计写入默认成功，用例可以专心断言业务语句。
		if strings.Contains(sql, "claw_admin_audit_logs") {
			return pgconn.NewCommandTag("INSERT 0 1"), nil
		}
		panic("groupFakeDB.Exec not stubbed: " + sql)
	}
	return f.exec(sql, args)
}

func (f *groupFakeDB) record(sql string, args []any) {
	if f.argsSeen == nil {
		f.argsSeen = make(map[string][]any)
	}
	for _, marker := range []string{
		"FROM claw_admin_users", "SELECT COUNT(*) FROM rongcloud_group",
		"SELECT owner_id FROM rongcloud_group", "FROM rongcloud_group WHERE group_id",
		"DELETE FROM rongcloud_group_member", "DELETE FROM rongcloud_group", "UPDATE rongcloud_group",
	} {
		if strings.Contains(sql, marker) {
			f.argsSeen[marker] = args
			return
		}
	}
	f.argsSeen[sql] = args
}

// ---- 行 / 结果集打桩 --------------------------------------------------------

type groupStubRow struct {
	pgx.Row
	values []any
	err    error
}

func (r *groupStubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return assignGroupScan(dest, r.values)
}

func assignGroupScan(dest []any, values []any) error {
	if len(dest) != len(values) {
		return fmt.Errorf("scan arity mismatch: %d destinations vs %d values", len(dest), len(values))
	}
	for i := range dest {
		switch target := dest[i].(type) {
		case *string:
			value, ok := values[i].(string)
			if !ok {
				return fmt.Errorf("column %d: want string, got %T", i, values[i])
			}
			*target = value
		case *int:
			value, ok := values[i].(int)
			if !ok {
				return fmt.Errorf("column %d: want int, got %T", i, values[i])
			}
			*target = value
		case *[]byte:
			value, ok := values[i].([]byte)
			if !ok {
				return fmt.Errorf("column %d: want []byte, got %T", i, values[i])
			}
			*target = value
		case *time.Time:
			value, ok := values[i].(time.Time)
			if !ok {
				return fmt.Errorf("column %d: want time.Time, got %T", i, values[i])
			}
			*target = value
		case *bool:
			value, ok := values[i].(bool)
			if !ok {
				return fmt.Errorf("column %d: want bool, got %T", i, values[i])
			}
			*target = value
		default:
			return fmt.Errorf("column %d: unsupported destination %T", i, dest[i])
		}
	}
	return nil
}

// groupStubRows 是一个最小的 pgx.Rows：只按行喂 []any。
type groupStubRows struct {
	pgx.Rows
	rows [][]any
	idx  int
	err  error
}

func (r *groupStubRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *groupStubRows) Scan(dest ...any) error {
	if r.idx == 0 || r.idx > len(r.rows) {
		return errors.New("Scan called outside of Next")
	}
	return assignGroupScan(dest, r.rows[r.idx-1])
}

func (r *groupStubRows) Close()     {}
func (r *groupStubRows) Err() error { return r.err }

// ---- 认证脚手架 -------------------------------------------------------------

const groupAdminID = "admin-1"

func groupAdminRow(permissions []string) []any {
	encoded, _ := json.Marshal(permissions)
	return []any{groupAdminID, "boss", "老板", "role-1", "active", "超级管理员", encoded}
}

func groupAuthedRequest(t *testing.T, method, target string, body string, permissions []string) *http.Request {
	t.Helper()
	token, err := issueClawAdminJWT(groupAdminID, "access", time.Hour)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	return req.WithContext(contextWithGroupRouteParams(req.Context(), target))
}

// contextWithGroupRouteParams 把路径里的 {id} / {user_id} 塞进 chi 的 RouteCtx，
// 这样不用起完整 router 也能测 handler。
func contextWithGroupRouteParams(ctx context.Context, target string) context.Context {
	path := strings.TrimPrefix(target, "/api/admin/groups")
	segments := make([]string, 0, 3)
	for _, segment := range strings.Split(path, "/") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	rctx := chi.NewRouteContext()
	if len(segments) > 0 {
		rctx.URLParams.Add("id", segments[0])
	}
	if len(segments) > 2 {
		rctx.URLParams.Add("user_id", segments[2])
	}
	return context.WithValue(ctx, chi.RouteCtxKey, rctx)
}

// newGroupHandler 返回一个只接了群组接口所需依赖的 Handler。
func newGroupHandler(fake *groupFakeDB, permissions []string) *Handler {
	baseRow := &groupStubRow{values: groupAdminRow(permissions)}
	fake.queryRow = func(string, []any) pgx.Row { return baseRow }
	return &Handler{DB: fake}
}

func decodeGroupBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rr.Body.String())
	}
	return body
}

// isNilArg reports whether a query argument is absent. A typed nil pointer
// (*int)(nil) is not == nil once boxed in an any, so a plain comparison would
// wrongly flag every untouched column as "changed".
func isNilArg(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Slice, reflect.Map:
		return reflected.IsNil()
	default:
		return false
	}
}

// ---- clawAdminScanGroup -----------------------------------------------------

func TestClawAdminScanGroupReadsSevenColumnsInOrder(t *testing.T) {
	created := time.Date(2026, 5, 1, 10, 30, 0, 0, time.UTC)
	updated := time.Date(2026, 5, 2, 11, 0, 0, 0, time.UTC)
	row := &groupStubRow{values: []any{"100001", "产品讨论", "850509", 3, "说明", created, updated}}

	group, err := clawAdminScanGroup(row)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	checks := map[string]interface{}{
		"group_id":     "100001",
		"group_name":   "产品讨论",
		"owner_id":     "850509",
		"member_count": 3,
		"description":  "说明",
		"created_at":   "2026-05-01T10:30:00Z",
		"updated_at":   "2026-05-02T11:00:00Z",
	}
	for key, want := range checks {
		if got := group[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
	if len(group) != 7 {
		t.Errorf("group has %d keys, want 7 (old contract shape)", len(group))
	}
}

func TestClawAdminScanGroupPropagatesNoRows(t *testing.T) {
	row := &groupStubRow{err: pgx.ErrNoRows}
	if _, err := clawAdminScanGroup(row); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("err = %v, want ErrNoRows", err)
	}
}

// ---- clawGroupOwner ---------------------------------------------------------

func TestClawGroupOwnerReportsMissingAsNotFound(t *testing.T) {
	fake := &groupFakeDB{queryRow: func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "SELECT owner_id FROM rongcloud_group") {
			return &groupStubRow{err: pgx.ErrNoRows}
		}
		return &groupStubRow{}
	}}
	h := &Handler{DB: fake}

	owner, found, err := h.clawGroupOwner(context.Background(), "404")
	if err != nil {
		t.Fatalf("err = %v, want nil (missing group is not an error)", err)
	}
	if found || owner != "" {
		t.Fatalf("owner = %q found = %v, want not found", owner, found)
	}
}

func TestClawGroupOwnerReturnsTheOwner(t *testing.T) {
	fake := &groupFakeDB{queryRow: func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "SELECT owner_id FROM rongcloud_group") {
			if args[0] != "100001" {
				t.Errorf("group id arg = %v", args[0])
			}
			return &groupStubRow{values: []any{"850509"}}
		}
		return &groupStubRow{}
	}}
	h := &Handler{DB: fake}

	owner, found, err := h.clawGroupOwner(context.Background(), "100001")
	if err != nil || !found || owner != "850509" {
		t.Fatalf("owner = %q found = %v err = %v", owner, found, err)
	}
}

// ---- 权限门 ----------------------------------------------------------------

func TestClawAdminGroupEndpointsRequireTheirPermission(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		target      string
		permissions []string
		handler     func(h *Handler, w http.ResponseWriter, r *http.Request)
	}{
		{"list needs group:read", http.MethodGet, "/api/admin/groups", []string{"node:read"}, (*Handler).ClawAdminGroups},
		{"detail needs group:read", http.MethodGet, "/api/admin/groups/100001", []string{"node:read"}, (*Handler).ClawAdminGroupDetail},
		{"members needs group:read", http.MethodGet, "/api/admin/groups/100001/members", nil, (*Handler).ClawAdminGroupMembers},
		{"update needs group:write", http.MethodPut, "/api/admin/groups/100001", []string{"group:read"}, (*Handler).ClawAdminUpdateGroup},
		{"delete needs group:write", http.MethodDelete, "/api/admin/groups/100001", []string{"group:read"}, (*Handler).ClawAdminDeleteGroup},
		{"remove member needs group:write", http.MethodDelete, "/api/admin/groups/100001/members/850509", []string{"group:read"}, (*Handler).ClawAdminRemoveGroupMember},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 没有任何业务语句打桩：一旦越权门走到 DB 就会 panic。
			h := newGroupHandler(&groupFakeDB{}, tc.permissions)
			req := groupAuthedRequest(t, tc.method, tc.target, "", tc.permissions)
			rr := httptest.NewRecorder()
			tc.handler(h, rr, req)

			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", rr.Code, rr.Body.String())
			}
			body := decodeGroupBody(t, rr)
			if body["code"] != float64(http.StatusForbidden) {
				t.Errorf("code = %#v", body["code"])
			}
		})
	}
}

func TestClawAdminGroupEndpointsRejectAnonymous(t *testing.T) {
	h := newGroupHandler(&groupFakeDB{}, clawAdminAllPermissions)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/groups", nil)
	rr := httptest.NewRecorder()
	h.ClawAdminGroups(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

// ---- 404 路径 ---------------------------------------------------------------

func TestClawAdminGroupDetailReturns404WhenMissing(t *testing.T) {
	h := newGroupHandler(&groupFakeDB{}, clawAdminAllPermissions)
	h.DB.(*groupFakeDB).queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "FROM claw_admin_users") {
			return &groupStubRow{values: groupAdminRow(clawAdminAllPermissions)}
		}
		return &groupStubRow{err: pgx.ErrNoRows}
	}

	req := groupAuthedRequest(t, http.MethodGet, "/api/admin/groups/999", "", clawAdminAllPermissions)
	rr := httptest.NewRecorder()
	h.ClawAdminGroupDetail(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rr.Code, rr.Body.String())
	}
}

func TestClawAdminDeleteGroupReturns404WhenNothingDeleted(t *testing.T) {
	fake := &groupFakeDB{}
	fake.queryRow = func(sql string, args []any) pgx.Row {
		return &groupStubRow{values: groupAdminRow(clawAdminAllPermissions)}
	}
	fake.exec = func(sql string, args []any) (pgconn.CommandTag, error) {
		return pgconn.NewCommandTag("DELETE 0"), nil
	}
	h := &Handler{DB: fake}

	req := groupAuthedRequest(t, http.MethodDelete, "/api/admin/groups/999", "", clawAdminAllPermissions)
	rr := httptest.NewRecorder()
	h.ClawAdminDeleteGroup(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rr.Code, rr.Body.String())
	}
	// 404 时不能留下审计记录。
	for _, sql := range fake.execs {
		if strings.Contains(sql, "claw_admin_audit_logs") {
			t.Fatal("a failed delete must not write an audit row")
		}
	}
}

func TestClawAdminDeleteGroupWritesAuditOnSuccess(t *testing.T) {
	fake := &groupFakeDB{}
	fake.queryRow = func(sql string, args []any) pgx.Row {
		return &groupStubRow{values: groupAdminRow(clawAdminAllPermissions)}
	}
	fake.exec = func(sql string, args []any) (pgconn.CommandTag, error) {
		if strings.Contains(sql, "claw_admin_audit_logs") {
			return pgconn.NewCommandTag("INSERT 0 1"), nil
		}
		if args[0] != "100001" {
			t.Errorf("delete arg = %v", args[0])
		}
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	h := &Handler{DB: fake}

	req := groupAuthedRequest(t, http.MethodDelete, "/api/admin/groups/100001", "", clawAdminAllPermissions)
	rr := httptest.NewRecorder()
	h.ClawAdminDeleteGroup(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	body := decodeGroupBody(t, rr)
	if body["code"] != float64(0) || body["message"] != "ok" {
		t.Errorf("envelope = %#v", body)
	}
	data, ok := body["data"].(map[string]interface{})
	if !ok || data["deleted"] != true {
		t.Errorf("data = %#v, want {deleted:true}", body["data"])
	}
}

func TestClawAdminRemoveGroupMemberReturns404WhenMissing(t *testing.T) {
	fake := &groupFakeDB{}
	fake.queryRow = func(sql string, args []any) pgx.Row {
		return &groupStubRow{values: groupAdminRow(clawAdminAllPermissions)}
	}
	var memberDeletes int
	fake.exec = func(sql string, args []any) (pgconn.CommandTag, error) {
		if strings.Contains(sql, "claw_admin_audit_logs") {
			return pgconn.NewCommandTag("INSERT 0 1"), nil
		}
		if strings.Contains(sql, "DELETE FROM rongcloud_group_member") {
			memberDeletes++
			if args[0] != "100001" || args[1] != "850509" {
				t.Errorf("delete args = %v", args)
			}
			return pgconn.NewCommandTag("DELETE 0"), nil
		}
		if strings.Contains(sql, "UPDATE rongcloud_group") {
			t.Fatal("member_count must not be recomputed when nothing was removed")
		}
		return pgconn.NewCommandTag(""), nil
	}
	h := &Handler{DB: fake}

	req := groupAuthedRequest(t, http.MethodDelete, "/api/admin/groups/100001/members/850509", "", clawAdminAllPermissions)
	rr := httptest.NewRecorder()
	h.ClawAdminRemoveGroupMember(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rr.Code, rr.Body.String())
	}
	if memberDeletes != 1 {
		t.Fatalf("member deletes = %d", memberDeletes)
	}
}

func TestClawAdminRemoveGroupMemberRecomputesCount(t *testing.T) {
	fake := &groupFakeDB{}
	fake.queryRow = func(sql string, args []any) pgx.Row {
		return &groupStubRow{values: groupAdminRow(clawAdminAllPermissions)}
	}
	var recomputed bool
	fake.exec = func(sql string, args []any) (pgconn.CommandTag, error) {
		switch {
		case strings.Contains(sql, "claw_admin_audit_logs"):
			return pgconn.NewCommandTag("INSERT 0 1"), nil
		case strings.Contains(sql, "DELETE FROM rongcloud_group_member"):
			return pgconn.NewCommandTag("DELETE 1"), nil
		case strings.Contains(sql, "UPDATE rongcloud_group"):
			recomputed = true
			if !strings.Contains(sql, "SELECT COUNT(*) FROM rongcloud_group_member") {
				t.Error("member_count must be recomputed from the member table")
			}
			return pgconn.NewCommandTag("UPDATE 1"), nil
		}
		return pgconn.NewCommandTag(""), nil
	}
	h := &Handler{DB: fake}

	req := groupAuthedRequest(t, http.MethodDelete, "/api/admin/groups/100001/members/850509", "", clawAdminAllPermissions)
	rr := httptest.NewRecorder()
	h.ClawAdminRemoveGroupMember(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	if !recomputed {
		t.Fatal("member_count was not recomputed")
	}
	data, _ := decodeGroupBody(t, rr)["data"].(map[string]interface{})
	if data["removed"] != true {
		t.Errorf("data = %#v", data)
	}
}

func TestClawAdminGroupMembersReturns404WhenGroupMissing(t *testing.T) {
	fake := &groupFakeDB{}
	fake.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "FROM claw_admin_users") {
			return &groupStubRow{values: groupAdminRow(clawAdminAllPermissions)}
		}
		return &groupStubRow{err: pgx.ErrNoRows}
	}
	h := &Handler{DB: fake}

	req := groupAuthedRequest(t, http.MethodGet, "/api/admin/groups/999/members", "", clawAdminAllPermissions)
	rr := httptest.NewRecorder()
	h.ClawAdminGroupMembers(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rr.Code, rr.Body.String())
	}
}

func TestClawAdminGroupMembersMapsRowsToTheFrontendShape(t *testing.T) {
	joined := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	fake := &groupFakeDB{}
	fake.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "FROM claw_admin_users") {
			return &groupStubRow{values: groupAdminRow(clawAdminAllPermissions)}
		}
		return &groupStubRow{values: []any{"850509"}}
	}
	fake.query = func(sql string, args []any) (pgx.Rows, error) {
		return &groupStubRows{rows: [][]any{
			{"850509", "100001", "11257339", "小酒", "owner", joined},
			{"rc_node_abc_opencode", "100001", "", "", "member", joined},
		}}, nil
	}
	h := &Handler{DB: fake}

	req := groupAuthedRequest(t, http.MethodGet, "/api/admin/groups/100001/members", "", clawAdminAllPermissions)
	rr := httptest.NewRecorder()
	h.ClawAdminGroupMembers(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	var envelope struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(envelope.Data) != 2 {
		t.Fatalf("members = %d", len(envelope.Data))
	}
	human := envelope.Data[0]
	if human["username"] != "11257339" || human["nickname"] != "小酒" || human["role"] != "owner" {
		t.Errorf("human member = %#v", human)
	}
	// 前端成员表两处都读 joined_at，两个键必须都在。
	if human["created_at"] != "2026-06-01T08:00:00Z" || human["joined_at"] != "2026-06-01T08:00:00Z" {
		t.Errorf("timestamps = %#v / %#v", human["created_at"], human["joined_at"])
	}
	node := envelope.Data[1]
	if node["username"] != "" || node["nickname"] != "" {
		t.Errorf("node members have no IM account, got %#v", node)
	}
}

// ---- 列表 -------------------------------------------------------------------

func TestClawAdminGroupsListShapeAndKeyword(t *testing.T) {
	created := time.Date(2026, 5, 1, 10, 30, 0, 0, time.UTC)
	fake := &groupFakeDB{}
	fake.queryRow = func(sql string, args []any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM claw_admin_users"):
			return &groupStubRow{values: groupAdminRow(clawAdminAllPermissions)}
		case strings.Contains(sql, "SELECT COUNT(*) FROM rongcloud_group"):
			if len(args) != 1 || args[0] != "%产品%" {
				t.Errorf("count args = %v, want the LIKE pattern", args)
			}
			return &groupStubRow{values: []any{1}}
		}
		return &groupStubRow{}
	}
	fake.query = func(sql string, args []any) (pgx.Rows, error) {
		if !strings.Contains(sql, "ORDER BY created_at DESC") {
			t.Errorf("list query must keep the old DESC ordering: %s", sql)
		}
		// 关键词占位符在前，分页两参数在后。
		if len(args) != 3 || args[0] != "%产品%" || args[1] != 10 || args[2] != 0 {
			t.Errorf("list args = %v, want [pattern, pageSize, offset]", args)
		}
		return &groupStubRows{rows: [][]any{
			{"100001", "产品讨论", "850509", 3, "说明", created, created},
		}}, nil
	}
	h := &Handler{DB: fake}

	req := groupAuthedRequest(t, http.MethodGet, "/api/admin/groups?keyword=产品&page=1&page_size=10", "", clawAdminAllPermissions)
	rr := httptest.NewRecorder()
	h.ClawAdminGroups(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	var envelope struct {
		Data struct {
			Items    []map[string]interface{} `json:"items"`
			Total    int                      `json:"total"`
			Page     int                      `json:"page"`
			PageSize int                      `json:"page_size"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Data.Total != 1 || envelope.Data.Page != 1 || envelope.Data.PageSize != 10 {
		t.Errorf("paging = %#v", envelope.Data)
	}
	if len(envelope.Data.Items) != 1 || envelope.Data.Items[0]["group_id"] != "100001" {
		t.Errorf("items = %#v", envelope.Data.Items)
	}
}

func TestClawAdminGroupsRejectsUnescapedKeyword(t *testing.T) {
	// % 与 _ 必须被转义，否则用户输入的通配符会变成"查全部"。
	fake := &groupFakeDB{}
	fake.queryRow = func(sql string, args []any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM claw_admin_users"):
			return &groupStubRow{values: groupAdminRow(clawAdminAllPermissions)}
		case strings.Contains(sql, "SELECT COUNT(*) FROM rongcloud_group"):
			if args[0] != `%100\%\_001%` {
				t.Errorf("pattern = %q, want the escaped literal", args[0])
			}
			return &groupStubRow{values: []any{0}}
		}
		return &groupStubRow{}
	}
	fake.query = func(sql string, args []any) (pgx.Rows, error) {
		return &groupStubRows{}, nil
	}
	h := &Handler{DB: fake}

	req := groupAuthedRequest(t, http.MethodGet, "/api/admin/groups?keyword=100%25_001", "", clawAdminAllPermissions)
	rr := httptest.NewRecorder()
	h.ClawAdminGroups(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
}

func TestClawAdminGroupsReports503WhenTheCountFails(t *testing.T) {
	fake := &groupFakeDB{}
	fake.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "FROM claw_admin_users") {
			return &groupStubRow{values: groupAdminRow(clawAdminAllPermissions)}
		}
		return &groupStubRow{err: errors.New("db down")}
	}
	h := &Handler{DB: fake}

	req := groupAuthedRequest(t, http.MethodGet, "/api/admin/groups", "", clawAdminAllPermissions)
	rr := httptest.NewRecorder()
	h.ClawAdminGroups(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", rr.Code, rr.Body.String())
	}
}

// ---- PUT /api/admin/groups/{id} ---------------------------------------------

func TestClawAdminUpdateGroupValidatesTheBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty object", `{}`},
		{"blank name", `{"group_name":"   "}`},
		{"name too long", fmt.Sprintf(`{"group_name":%q}`, strings.Repeat("名", clawAdminGroupMaxName+1))},
		{"description too long", fmt.Sprintf(`{"description":%q}`, strings.Repeat("说", clawAdminGroupMaxDescription+1))},
		{"not an object", `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newGroupHandler(&groupFakeDB{}, clawAdminAllPermissions)
			req := groupAuthedRequest(t, http.MethodPut, "/api/admin/groups/100001", tc.body, clawAdminAllPermissions)
			rr := httptest.NewRecorder()
			h.ClawAdminUpdateGroup(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestClawAdminUpdateGroupAppliesAndReturnsTheGroup(t *testing.T) {
	created := time.Date(2026, 5, 1, 10, 30, 0, 0, time.UTC)
	name := "产品讨论"
	description := "原说明"
	fake := &groupFakeDB{}
	fake.queryRow = func(sql string, args []any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM claw_admin_users"):
			return &groupStubRow{values: groupAdminRow(clawAdminAllPermissions)}
		case strings.Contains(sql, "FROM rongcloud_group WHERE group_id"):
			return &groupStubRow{values: []any{"100001", name, "850509", 3, description, created, created}}
		}
		return &groupStubRow{}
	}
	fake.exec = func(sql string, args []any) (pgconn.CommandTag, error) {
		if strings.Contains(sql, "claw_admin_audit_logs") {
			return pgconn.NewCommandTag("INSERT 0 1"), nil
		}
		if !strings.Contains(sql, "UPDATE rongcloud_group") {
			t.Fatalf("unexpected exec: %s", sql)
		}
		// args: groupID, groupName, portraitURI, description, maxRounds, stopped
		if args[0] != "100001" {
			t.Errorf("group id = %v", args[0])
		}
		if args[1] != "新名字" {
			t.Errorf("group name = %v", args[1])
		}
		if args[2] != "" {
			t.Errorf("portrait must stay untouched, got %v", args[2])
		}
		if args[3] != "" {
			t.Errorf("description must stay untouched, got %v", args[3])
		}
		if args[4] != nil && !isNilArg(args[4]) {
			t.Errorf("rounds must stay untouched, got %v", args[4])
		}
		if args[5] != nil && !isNilArg(args[5]) {
			t.Errorf("stopped must stay untouched, got %v", args[5])
		}
		name = "新名字"
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	h := &Handler{DB: fake}

	req := groupAuthedRequest(t, http.MethodPut, "/api/admin/groups/100001", `{"group_name":"新名字"}`, clawAdminAllPermissions)
	rr := httptest.NewRecorder()
	h.ClawAdminUpdateGroup(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
	}
	body := decodeGroupBody(t, rr)
	if body["message"] != "ok" {
		t.Errorf("message = %#v", body["message"])
	}
	data, _ := body["data"].(map[string]interface{})
	if data["group_name"] != "新名字" {
		t.Errorf("returned group = %#v", data)
	}
	// 审计必须记下改了哪些字段。
	var audited bool
	for _, sql := range fake.execs {
		if strings.Contains(sql, "claw_admin_audit_logs") {
			audited = true
		}
	}
	if !audited {
		t.Fatal("update must be audited")
	}
}

func TestClawAdminUpdateGroupReturns404WhenMissing(t *testing.T) {
	fake := &groupFakeDB{}
	fake.queryRow = func(sql string, args []any) pgx.Row {
		if strings.Contains(sql, "FROM claw_admin_users") {
			return &groupStubRow{values: groupAdminRow(clawAdminAllPermissions)}
		}
		return &groupStubRow{err: pgx.ErrNoRows}
	}
	h := &Handler{DB: fake}

	req := groupAuthedRequest(t, http.MethodPut, "/api/admin/groups/999", `{"group_name":"x"}`, clawAdminAllPermissions)
	rr := httptest.NewRecorder()
	h.ClawAdminUpdateGroup(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rr.Code, rr.Body.String())
	}
	for _, sql := range fake.execs {
		if strings.Contains(sql, "UPDATE rongcloud_group") {
			t.Fatal("a missing group must not be updated")
		}
	}
}

// ---- GroupMirror 装配 --------------------------------------------------------

func TestClawGroupMirrorWiresEveryCallback(t *testing.T) {
	h := &Handler{}
	mirror := h.ClawGroupMirror()
	if mirror == nil {
		t.Fatal("mirror is nil")
	}
	// 少挂任何一个回调都会让某条命令路径在运行期 503，所以这里逐个钉住。
	if mirror.AllocateID == nil {
		t.Error("AllocateID not wired")
	}
	if mirror.SaveGroup == nil {
		t.Error("SaveGroup not wired")
	}
	if mirror.OwnerOf == nil {
		t.Error("OwnerOf not wired")
	}
	if mirror.UpdateMeta == nil {
		t.Error("UpdateMeta not wired")
	}
	if mirror.FallbackAgent == nil {
		t.Error("FallbackAgent not wired")
	}
	// 成员/资料类动作（addMember、removeMember、getMembers、getMyGroups、
	// getProfiles、getInfo、transfer、quit、dismiss）的数据侧。
	if mirror.IsMember == nil {
		t.Error("IsMember not wired")
	}
	if mirror.AddMembers == nil {
		t.Error("AddMembers not wired")
	}
	if mirror.RemoveMember == nil {
		t.Error("RemoveMember not wired")
	}
	if mirror.ListMembers == nil {
		t.Error("ListMembers not wired")
	}
	if mirror.ListGroupsByUser == nil {
		t.Error("ListGroupsByUser not wired")
	}
	if mirror.GroupProfiles == nil {
		t.Error("GroupProfiles not wired")
	}
	if mirror.SetOwner == nil {
		t.Error("SetOwner not wired")
	}
	if mirror.DeleteGroup == nil {
		t.Error("DeleteGroup not wired")
	}
}

func TestClawGroupMirrorFallbackAgentNeedsTheRongCloudIntegration(t *testing.T) {
	// RongCloudNode 为 nil（未配置融云）时必须安静地返回"没有兜底节点"，
	// 而不是 panic 或报错。
	h := &Handler{}
	agentID, ok, err := h.clawGroupFallbackAgent(context.Background())
	if err != nil || ok || agentID != "" {
		t.Fatalf("got %q %v %v, want empty/false/nil", agentID, ok, err)
	}
}
