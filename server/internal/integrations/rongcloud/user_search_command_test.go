package rongcloud

// user_search_command_test.go 钉住 service=user/action=search —— web「添加好友」
// 的账号搜索（实现见 user_search_command.go，路由见 user_command.go 的
// `case "search"`）。
//
// 为什么这批用例必须存在：searchClawIMUsers 里那条
// `SELECT ... FROM claw_im_users` 至今**从未在真实 Postgres 上执行过**
// （本机没有 psql、没有 TEST_DATABASE_URL），开发期只有人工语法审查。因此
// B 组用例把 SQL 文本形状与绑定参数当契约钉死：表名拼错、过滤条件丢失、
// `$1/$2` 错位、关键字从参数退化成拼接，都会立刻变红。这条防线是本次唯一的
// 自动化防线，不是可选项。
//
// 分组对应任务书：
//   A. 命令通道行为（脚手架：im_service_commands_test.go 的 newIMServiceServer /
//      newIMServiceHandler / sendWebCommand / wantWebError）
//   B. SQL 形状（假 pgx.Rows 范式照 friend_device_owner_claim_test.go 的 claimRows）
//   C. 纯函数 clawIMLikePattern
//   D. 边界（按实现事实写预期：nickname 可为空串、不排除发送者自己）
//
// 刻意不用既有 userFixture.store()：它的 SearchUsers 是 nil，本题要精确控制
// store 的返回/错误，所以直接手搓 &UserStore{SearchUsers: ...}，
// 也刻意不复用 stubDBTX（它的 Query 是故意报错的）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// --- 脚手架 -----------------------------------------------------------------

// userSearchStub 顶替 UserStore.SearchUsers：记录 handler 传下来的 keyword /
// limit，并回预置的结果或错误。
type userSearchStub struct {
	mu      sync.Mutex
	keyword string
	limit   int
	calls   int
	result  []UserSummary
	err     error
}

func (s *userSearchStub) search(_ context.Context, keyword string, limit int) ([]UserSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keyword = keyword
	s.limit = limit
	s.calls++
	return s.result, s.err
}

func (s *userSearchStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// newUserSearchHandler 构造一个只装配了 SearchUsers 的 handler。
// 其它字段（DB queries / 其它 store）保持 nil，于是任何一次多余的依赖都会
// 直接 nil panic 而不是被静默兜底。
func newUserSearchHandler(t *testing.T, stub *userSearchStub) (*imServiceServer, *systemHandler) {
	t.Helper()
	rec, srv := newIMServiceServer(t, imServiceServerOptions{})
	t.Cleanup(srv.Close)
	handler := newIMServiceHandler(t, srv)
	if stub != nil {
		handler.userStore = &UserStore{SearchUsers: stub.search}
	}
	return rec, handler
}

// userSearchList 取 reply.data.list，并要求它是 JSON 数组。
// `null` 反序列化成 nil interface，这里的类型断言会失败 —— 这正是
// 「零命中必须是 []，不能是 null」的判据。
func userSearchList(t *testing.T, reply map[string]interface{}) []interface{} {
	t.Helper()
	data, ok := reply["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("reply.data = %#v, want an object carrying `list` (full reply %#v)", reply["data"], reply)
	}
	list, ok := data["list"].([]interface{})
	if !ok {
		t.Fatalf("reply.data.list = %#v, want a JSON array — `null` here makes the web iterate nothing "+
			"(full reply %#v)", data["list"], reply)
	}
	return list
}

// --- A. 命令通道行为 ---------------------------------------------------------

// 命中：回包 status=success/code=200，data.list 长度正确，且每条结果的字段名
// 是 camelCase。字段名不是小事 —— web 的 UserInfo 读 `portraitUri`，
// 少一个字母就一直显示默认头像；`user_id` 这种 snake_case 漏出则读不到。
//
// 同时钉住**回包只有 4 个字段**（M2 的隐私收窄）：`email`/`phone` 仍参与 WHERE
// 命中（按手机号/邮箱能搜到人，保留旧实现的产品能力），但**不回给搜索者** ——
// 把别人的手机号回给任意搜索者本身就是个泄漏面。所以这里对 email/phone 是
// 「必须不存在」的断言，而不是「值对不对」。
func TestUserSearchReturnsCamelCaseSummaries(t *testing.T) {
	stub := &userSearchStub{result: []UserSummary{
		{
			UserID:      "100001",
			Username:    "alice",
			Nickname:    "爱丽丝",
			PortraitURI: "https://example.test/a.png",
		},
		{UserID: "100002", Username: "bob", Nickname: "鲍勃"},
	}}
	rec, handler := newUserSearchHandler(t, stub)

	sendWebCommand(t, handler, "user-7", "user", "search",
		map[string]interface{}{"keyword": "alice"})

	reply := rec.last(t)
	if reply["status"] != "success" || reply["code"] != float64(200) {
		t.Fatalf("status/code = %#v/%#v, want success/200 (reply %#v)",
			reply["status"], reply["code"], reply)
	}
	list := userSearchList(t, reply)
	if len(list) != 2 {
		t.Fatalf("data.list length = %d, want 2 (got %#v)", len(list), list)
	}

	first, ok := list[0].(map[string]interface{})
	if !ok {
		t.Fatalf("list[0] = %#v, want an object", list[0])
	}
	for _, field := range []struct{ name, want string }{
		{"userId", "100001"},
		{"username", "alice"},
		{"nickname", "爱丽丝"},
		{"portraitUri", "https://example.test/a.png"},
	} {
		if got := first[field.name]; got != field.want {
			t.Errorf("list[0].%s = %#v, want %q (entries: %#v)", field.name, got, field.want, first)
		}
	}
	// 反面断言：snake_case 漏出 = 前端字段读空。
	for _, snake := range []string{"user_id", "portrait_uri", "nick_name", "email_address"} {
		if _, exists := first[snake]; exists {
			t.Errorf("list[0] carries the snake_case key %q — the JSON tags must stay camelCase (%#v)",
				snake, first)
		}
	}
	// 隐私收窄（M2）：命中列比回包宽，email/phone 只参与 WHERE、绝不回传。
	// 这条不是「锦上添花」——把手机号回给任意搜索者是一个泄漏面，且两端 UI
	// 都不消费这两个字段，没有任何理由留在回包里。
	for _, forbidden := range []string{"email", "phone"} {
		if _, exists := first[forbidden]; exists {
			t.Errorf("list[0] exposes %q to the caller: %#v — email/phone are hit-only columns "+
				"and must not be returned (M2)", forbidden, first)
		}
	}
	if len(first) != 4 {
		t.Errorf("list[0] has %d keys, want exactly the 4 documented ones (%#v)", len(first), first)
	}

	// userId 是 web「账号」列显示的值（AddFriendDialog），必须在每一条里都有。
	second, _ := list[1].(map[string]interface{})
	if second["userId"] != "100002" {
		t.Errorf("list[1].userId = %#v, want 100002 (%#v)", second["userId"], second)
	}
}

// 零命中：data.list 必须是 []。`null` 会让 web 的 `result?.list ?? []` 这类兜底
// 写法各自表现不同（有的直接 forEach 崩），所以这条钉在 JSON 层面。
func TestUserSearchZeroHitsRepliesAnEmptyListNotNull(t *testing.T) {
	cases := []struct {
		name   string
		result []UserSummary
	}{
		{"the store returns a nil slice", nil},
		{"the store returns an empty slice", []UserSummary{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, handler := newUserSearchHandler(t, &userSearchStub{result: tc.result})

			sendWebCommand(t, handler, "user-7", "user", "search",
				map[string]interface{}{"keyword": "nobody"})

			reply := rec.last(t)
			if reply["status"] != "success" {
				t.Fatalf("status = %#v, want success — zero hits are not an error (reply %#v)",
					reply["status"], reply)
			}
			list := userSearchList(t, reply) // `null` → 这里直接 fatal
			if len(list) != 0 {
				t.Fatalf("data.list = %#v, want an empty array", list)
			}
			encoded, err := json.Marshal(reply["data"])
			if err != nil {
				t.Fatalf("re-marshal data: %v", err)
			}
			if !strings.Contains(string(encoded), `"list":[]`) {
				t.Fatalf("serialized data = %s, want `\"list\":[]` — a null list is not interchangeable here", encoded)
			}
		})
	}
}

// 空白/空关键字：400 且 store 根本不该被调用（不能拿空白去捞全表）。
func TestUserSearchRejectsAnEmptyKeyword(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]interface{}
	}{
		{"whitespace only", map[string]interface{}{"keyword": "   "}},
		{"empty string", map[string]interface{}{"keyword": ""}},
		{"keyword omitted", map[string]interface{}{}},
		{"q as documented alias is also blank", map[string]interface{}{"q": " \t\n "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &userSearchStub{}
			rec, handler := newUserSearchHandler(t, stub)

			sendWebCommand(t, handler, "user-7", "user", "search", tc.payload)

			wantWebError(t, rec.last(t), 400, "keyword")
			if calls := stub.callCount(); calls != 0 {
				t.Errorf("SearchUsers called %d times for a blank keyword, want 0 — "+
					"clawIMLikePattern(\"\") is \"%%\", i.e. the whole table", calls)
			}
		})
	}

	// 校验顺序：keyword 先于 store 判空。没有 DB 的部署里，客户端仍应收到能自纠的
	// 「keyword 为空」400，而不是被 503 顶掉。
	t.Run("the keyword check runs before the store check", func(t *testing.T) {
		rec, handler := newUserSearchHandler(t, nil) // 故意不装配 store

		sendWebCommand(t, handler, "user-7", "user", "search",
			map[string]interface{}{"keyword": "   "})

		wantWebError(t, rec.last(t), 400, "keyword")
	})
}

// store 未装配 / SearchUsers 为 nil：必须是 503「能力未启用」，不能 panic。
// 这两种形态在部署里都真实存在（无 DB 时 NewUserStore 返回 nil）。
func TestUserSearchFailsClosedWhenTheStoreIsNotWired(t *testing.T) {
	t.Run("no user store at all", func(t *testing.T) {
		rec, handler := newUserSearchHandler(t, nil) // handler.userStore 保持 nil
		if handler.userStore != nil {
			t.Fatal("precondition failed: userStore should start nil")
		}

		sendWebCommand(t, handler, "user-7", "user", "search",
			map[string]interface{}{"keyword": "alice"})

		wantWebError(t, rec.last(t), 503, "user service is unavailable")
	})

	t.Run("a store without SearchUsers", func(t *testing.T) {
		rec, handler := newUserSearchHandler(t, nil)
		// 生产形态：DBTX 缺失时 NewUserStore 返回 nil，但旧 store 装配也可能只
		// 填了几个字段。SearchUsers 为 nil 时调用会 panic，所以必须先判空。
		handler.userStore = &UserStore{}

		sendWebCommand(t, handler, "user-7", "user", "search",
			map[string]interface{}{"keyword": "alice"})

		wantWebError(t, rec.last(t), 503, "user service is unavailable")
	})
}

// store 报错：500 + 通用文案，且响应体全文不得包含内部表名或底层错误文本。
// 旧 Python 实现把 str(e) 直接回给客户端（command_handler.py:1992），这条是
// 那条行为不再回来的护栏。
func TestUserSearchHidesStoreErrorsFromTheClient(t *testing.T) {
	const leaky = "boom: relation claw_im_users does not exist"
	rec, handler := newUserSearchHandler(t, &userSearchStub{err: errors.New(leaky)})

	sendWebCommand(t, handler, "user-7", "user", "search",
		map[string]interface{}{"keyword": "alice"})

	reply := rec.last(t)
	wantWebError(t, reply, 500, "failed to search users")

	encoded, err := json.Marshal(reply)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	body := string(encoded)
	for _, forbidden := range []string{"claw_im_users", "boom", "relation", "does not exist"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("reply body leaks the internal detail %q: %s", forbidden, body)
		}
	}
}

// 关键字透传：handler 必须先 TrimSpace 再交给 store，limit 必须是 userSearchLimit。
// 传空白进 SQL 等于没有任何过滤条件（`%   %`）；limit 漂了就是一次把全表下发。
func TestUserSearchTrimsTheKeywordAndAppliesTheLimit(t *testing.T) {
	t.Run("keyword is trimmed and the limit is the service cap", func(t *testing.T) {
		if userSearchLimit != 20 {
			t.Fatalf("userSearchLimit = %d, want 20 — the web UI and the旧实现's无上限 are both pinned to 20",
				userSearchLimit)
		}
		stub := &userSearchStub{}
		rec, handler := newUserSearchHandler(t, stub)

		sendWebCommand(t, handler, "user-7", "user", "search",
			map[string]interface{}{"keyword": "  Alice  "})

		wantWebSuccess(t, rec.last(t))
		if got := stub.keyword; got != "Alice" {
			t.Errorf("keyword passed to the store = %q, want %q (whitespace must be trimmed)", got, "Alice")
		}
		if got := stub.limit; got != userSearchLimit {
			t.Errorf("limit passed to the store = %d, want %d", got, userSearchLimit)
		}
		if calls := stub.callCount(); calls != 1 {
			t.Errorf("SearchUsers calls = %d, want exactly 1", calls)
		}
	})

	// `q` 是旧客户端的别名（getStringAny("keyword","q")），不能因为改名就搜不到。
	t.Run("the q alias is accepted", func(t *testing.T) {
		stub := &userSearchStub{}
		rec, handler := newUserSearchHandler(t, stub)

		sendWebCommand(t, handler, "user-7", "user", "search",
			map[string]interface{}{"q": "  Bob "})

		wantWebSuccess(t, rec.last(t))
		if got := stub.keyword; got != "Bob" {
			t.Errorf("keyword passed to the store = %q, want %q (payload.q must be read)", got, "Bob")
		}
	})
}

// CLI 方言（snake_case 的 request_id + params）下回包走 {request_id, payload}，
// handler 必须把 list 放进 map 才能让它落进 payload —— 传裸切片会变成
// payload.data（system_handler.go:634-642），CLI 侧就读不到 list。
func TestUserSearchRepliesTheListInsidePayloadForTheDeviceEnvelope(t *testing.T) {
	stub := &userSearchStub{result: []UserSummary{{UserID: "100001", Username: "alice"}}}
	rec, handler := newUserSearchHandler(t, stub)

	content, err := json.Marshal(map[string]interface{}{
		"request_id": "req-device-1",
		"service":    "user",
		"action":     "search",
		"params":     map[string]interface{}{"keyword": "alice"},
	})
	if err != nil {
		t.Fatalf("marshal command: %v", err)
	}
	msg := NormalizedMessage{
		ObjectName: objectNameCommand,
		FromUserID: "user-7",
		Content:    string(content),
	}
	if err := handler.handleCommand(context.Background(), msg); err != nil {
		t.Fatalf("handleCommand user/search: %v", err)
	}

	reply := rec.last(t)
	if reply["msg_type"] != "command_result" {
		t.Fatalf("msg_type = %#v, want command_result (reply %#v)", reply["msg_type"], reply)
	}
	payload, ok := reply["payload"].(map[string]interface{})
	if !ok {
		t.Fatalf("reply.payload = %#v, want an object", reply["payload"])
	}
	list, ok := payload["list"].([]interface{})
	if !ok || len(list) != 1 {
		t.Fatalf("payload.list = %#v, want the list flattened into payload "+
			"(a bare slice would land in payload.data instead) — payload=%#v", payload["list"], payload)
	}
}

// --- B. SQL 形状（本次最大风险） ---------------------------------------------

// userSearchDB 是 db.DBTX 的假实现：Query 记录 SQL/参数并吐出预置的 pgx.Rows；
// Exec/QueryRow 一律报错，保证搜索路径只可能走那一条 SELECT。
type userSearchDB struct {
	queryErr error
	rows     pgx.Rows
	sql      string
	args     []interface{}
	queries  int
}

func (d *userSearchDB) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag(""), errors.New("userSearchDB: the search path must not Exec")
}

func (d *userSearchDB) Query(_ context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	d.queries++
	d.sql = sql
	d.args = args
	if d.queryErr != nil {
		return nil, d.queryErr
	}
	if d.rows == nil {
		d.rows = &userSearchRows{}
	}
	return d.rows, nil
}

func (d *userSearchDB) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	return userSearchErrRow{err: errors.New("userSearchDB: the search path must not QueryRow")}
}

type userSearchErrRow struct{ err error }

func (r userSearchErrRow) Scan(...interface{}) error { return r.err }

// userSearchRows 是 pgx.Rows 的最小实现（范式同 friend_device_owner_claim_test.go
// 的 claimRows）：按列序吐出预置的字符串行。
//
// **它能证明什么、不能证明什么**（审查员 Ray 的一次纠正）：Scan 里的个数校验比的是
// `len(dest)` 与这条**夹具行**的长度，不是与捕获到的 SQL 文本 —— 它只能挡住「夹具
// 和 Scan 目标数不一致」，挡不住「SELECT 的列序/列数和 Scan 不一致」。后者由
// userSearchSelectColumns 那条整段列清单检查从 SQL 文本上守，两者分工不要混。
type userSearchRows struct {
	values  [][]string
	index   int
	scanErr error
	iterErr error
	closed  bool
}

func (r *userSearchRows) Next() bool {
	if r.index >= len(r.values) {
		return false
	}
	r.index++
	return true
}

func (r *userSearchRows) Scan(dest ...interface{}) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if r.index == 0 || r.index > len(r.values) {
		return errors.New("userSearchRows: Scan called outside Next()")
	}
	row := r.values[r.index-1]
	if len(dest) != len(row) {
		return fmt.Errorf("userSearchRows: Scan got %d destinations, the fixture row has %d values "+
			"(fix the fixture, not the assertion — the SELECT list's shape is pinned by "+
			"userSearchSelectColumns)", len(dest), len(row))
	}
	for i := range dest {
		target, ok := dest[i].(*string)
		if !ok {
			return fmt.Errorf("userSearchRows: column %d destination is %T, want *string", i, dest[i])
		}
		*target = row[i]
	}
	return nil
}

func (r *userSearchRows) Close()                                       { r.closed = true }
func (r *userSearchRows) Err() error                                   { return r.iterErr }
func (r *userSearchRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 0") }
func (r *userSearchRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *userSearchRows) Values() ([]interface{}, error) {
	return nil, errors.New("userSearchRows: Values not implemented")
}
func (r *userSearchRows) RawValues() [][]byte  { return nil }
func (r *userSearchRows) Conn() *pgx.Conn      { return nil }
func (r *userSearchRows) TypeMap() *pgtype.Map { return nil }

// userSearchILikeColumn 匹配「某一列参与 $1 的 ILIKE 命中」，容忍
// COALESCE(col, 空串) 这种写法（不写 COALESCE 也合法，NULL ILIKE 结果为 false）。
func userSearchILikeColumn(column string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:COALESCE\(\s*)?` + regexp.QuoteMeta(column) +
		`\s*(?:,\s*'')?\)?\s*ILIKE\s+\$1\b`)
}

// userSearchSelectColumns 是 SELECT 列表的**列顺序契约**：这个顺序必须与
// searchClawIMUsers 里 `rows.Scan(&user.UserID, &user.Username, &user.Nickname,
// &user.PortraitURI)` 的目标顺序逐位一致。
//
// 为什么必须单独守：位置参数按列序绑定，Scan 也按列序赋值 —— 一旦 SELECT 里把
// nickname 与 portrait_uri 对调、或把 user_id 与 username 对调，**没有任何 Go
// 代码会报错**，线上表现是「昵称显示成头像 URL」这类静默串行。假 Rows 只能证明
// 「Scan 按顺序吃夹具的 4 个值」，证明不了「SQL 的列序与 Scan 的列序一致」，
// 所以这一条只能从 SQL 文本上守。
//
// 注意 WHERE 与 SELECT 的列集合**故意不同**（M2 隐私收窄）：WHERE 里 ILIKE 的是
// 5 列（含 email / phone），SELECT 只取 4 列。所以「SELECT 里又出现 email/phone」
// 本身就是这条检查要防的东西 —— 下面的整段列清单正则锚在 FROM 上，多一列即红。
//
// 抽成一处单独的切片（而不是散在断言里）：将来回包字段再变时（例如再去掉
// nickname），只改这里一处 —— SELECT 列表与 Scan 目标必须同时改，这条契约就
// 跟着一起走。
var userSearchSelectColumns = []string{
	"user_id",
	"username",
	"COALESCE(nickname, '')",
	"COALESCE(portrait_uri, '')",
}

// userSearchSelectListPattern 把 userSearchSelectColumns 编译成「整段列清单」正则：
// 列与列之间允许任意空白，每列内部的空格也允许被删除（`COALESCE(nickname,空串)`），
// 因此只对**列的顺序与个数**敏感，对版式不敏感。
func userSearchSelectListPattern() *regexp.Regexp {
	parts := make([]string, 0, len(userSearchSelectColumns))
	for _, column := range userSearchSelectColumns {
		parts = append(parts, strings.ReplaceAll(regexp.QuoteMeta(column), " ", `\s*`))
	}
	return regexp.MustCompile(`(?i)\bSELECT\s+` + strings.Join(parts, `\s*,\s*`) + `\s+FROM\b`)
}

// userSearchSQLCheck 是一条「SQL 必须长成什么样」的断言。what 会在失败信息里
// 直接点名缺哪一段；它同时是反证用例（TestUserSearchQueryChecksRejectBrokenSQL）
// 的配对键，改文案时两处要一起改。
type userSearchSQLCheck struct {
	what string
	re   *regexp.Regexp
}

// userSearchSQLChecks 的断言风格：**逐片段正则**，不是整体字符串比对。
//
// 调用方会先把 SQL 的所有空白折叠成单个空格（userSearchNormalizeSQL），所以
// 「有人重排缩进 / 把 WHERE 子句拆成别的行宽 / 加删换行」**不会**误红。
//
// 在此基础上每条正则再留了三类语义等价的空间，避免把「格式重构」误判成
// 「SQL 写错」：
//   - 大小写不敏感（`(?i)`）：PG 会把未加引号的标识符折叠成小写，`ILIKE`/`ilike`
//     在语义上没有区别；
//   - 逗号/括号周围的空白可选：`LEFT(username,3)` 与 `LEFT(username, 3)` 等价；
//   - `<>` 与 `!=` 等价（左侧两条 om_ 排除用角色类 `(?:<>|!=)` 表达）。
//
// 反过来，只要**语义**变了（少一个过滤条件、列名拼错、改用 `= $1`、`ORDER BY`
// 翻方向、LIMIT 写成字面量、表名改了）就一定会红 —— 这条由
// TestUserSearchQueryChecksRejectBrokenSQL 逐个变体反证。
func userSearchSQLChecks() []userSearchSQLCheck {
	return []userSearchSQLCheck{
		// 第一条就盯 SELECT 列表的顺序与列集合：这是「值被静默串到别的字段」的
		// 唯一防线（位置参数 + Scan 都按列序走，列序错了 Go 侧不会报错），同时
		// 也是「email/phone 不得回传」（M2 隐私收窄）的防线 —— 正则锚在 FROM 上，
		// 往 SELECT 里加回 email/phone 会立刻红。
		{"the SELECT list in column order (userId, username, nickname, portraitUri)",
			userSearchSelectListPattern()},
		{"the table `FROM claw_im_users`", regexp.MustCompile(`(?i)\bFROM\s+claw_im_users\b`)},
		{"the `status = 'active'` filter (disabled accounts must not be searchable)",
			regexp.MustCompile(`(?i)\bstatus\s*=\s*'active'`)},
		{"the om_ username exclusion `LEFT(username, 3) <> 'om_'",
			regexp.MustCompile(`(?i)LEFT\(\s*username\s*,\s*3\s*\)\s*(?:<>|!=)\s*'om_'`)},
		{"the om_ user_id exclusion `LEFT(user_id, 3) <> 'om_'",
			regexp.MustCompile(`(?i)LEFT\(\s*user_id\s*,\s*3\s*\)\s*(?:<>|!=)\s*'om_'`)},
		{"a `username ILIKE $1` hit column", userSearchILikeColumn("username")},
		{"a `nickname ILIKE $1` hit column", userSearchILikeColumn("nickname")},
		{"an `email ILIKE $1` hit column", userSearchILikeColumn("email")},
		{"a `phone ILIKE $1` hit column", userSearchILikeColumn("phone")},
		{"a `user_id ILIKE $1` hit column (the web shows userId as the account)",
			userSearchILikeColumn("user_id")},
		{"the ordering `ORDER BY created_at DESC`",
			regexp.MustCompile(`(?i)ORDER\s+BY\s+created_at\s+DESC`)},
		{"the row cap `LIMIT $2`", regexp.MustCompile(`(?i)LIMIT\s+\$2`)},
	}
}

// userSearchCapturedSQL 真跑一次 searchClawIMUsers，把捕获到的 SQL 归一化空白
// 后返回（附带的副作用是：顺手证明了这条查询只发一次）。
func userSearchCapturedSQL(t *testing.T) (normalized, raw string) {
	t.Helper()
	d := &userSearchDB{rows: &userSearchRows{}}
	if _, err := searchClawIMUsers(context.Background(), d, "alice", userSearchLimit); err != nil {
		t.Fatalf("searchClawIMUsers: %v", err)
	}
	if d.queries != 1 {
		t.Fatalf("Query calls = %d, want exactly 1 — the search must stay a single round trip", d.queries)
	}
	return userSearchNormalizeSQL(d.sql), d.sql
}

func userSearchNormalizeSQL(sql string) string {
	return strings.Join(strings.Fields(sql), " ")
}

// SQL 文本形状：这条 SELECT 从未在真库上跑过，因此每个关键片段都当契约钉住。
// 归一化空白后逐段断言，失败信息直接点名缺哪一段。
func TestUserSearchQueryShape(t *testing.T) {
	sql, raw := userSearchCapturedSQL(t)

	if !strings.HasPrefix(strings.ToUpper(sql), "SELECT") {
		t.Fatalf("the statement is not a SELECT: %s", sql)
	}
	for _, check := range userSearchSQLChecks() {
		if !check.re.MatchString(sql) {
			t.Errorf("the search SQL is missing %s\n--- normalized SQL ---\n%s", check.what, sql)
		}
	}

	// 关键字绝不能拼进 SQL 文本：那既是注入面，也会让 `%` 退化成「捞全表」。
	if strings.Contains(raw, "alice") {
		t.Errorf("the keyword must be bound as a parameter, never interpolated into the SQL: %s", raw)
	}
}

// 反证：把真实 SQL 逐段改坏，对应的检查必须变红。
//
// 为什么需要这条：B 组是本次唯一的 SQL 防线，而「匹配成功」的断言很容易写成
// 空断言（正则写松了、或某个片段其实没被任何检查覆盖）。这里对每个检查各造
// 一个「只破坏这一段」的变体，要求那条检查确实拒绝它 —— 于是「绿」才有意义。
func TestUserSearchQueryChecksRejectBrokenSQL(t *testing.T) {
	real, _ := userSearchCapturedSQL(t)

	breakages := []struct {
		name   string
		what   string // 必须与 userSearchSQLChecks() 里的 what 逐字一致
		mutate func(string) string
	}{
		{
			// 最要命的一种：字段对调（位置参数 + Scan 都按列序，列序错了不会报错，
			// 线上就是「手机号显示成邮箱」）。
			name: "user_id and username are swapped in the SELECT list",
			what: "the SELECT list in column order (userId, username, nickname, portraitUri)",
			mutate: func(s string) string {
				return strings.Replace(s, "SELECT user_id, username", "SELECT username, user_id", 1)
			},
		},
		{
			name: "nickname and portrait_uri are swapped in the SELECT list",
			what: "the SELECT list in column order (userId, username, nickname, portraitUri)",
			mutate: func(s string) string {
				return strings.Replace(s,
					"COALESCE(nickname, ''), COALESCE(portrait_uri, '')",
					"COALESCE(portrait_uri, ''), COALESCE(nickname, '')", 1)
			},
		},
		{
			name: "email is added back to the SELECT list",
			what: "the SELECT list in column order (userId, username, nickname, portraitUri)",
			mutate: func(s string) string {
				// M2 的隐私收窄：email/phone 只许参与 WHERE，回传就要拦。
				return strings.Replace(s,
					"COALESCE(portrait_uri, '')", "COALESCE(portrait_uri, ''), COALESCE(email, '')", 1)
			},
		},
		{
			name: "a fifth column is appended to the SELECT list",
			what: "the SELECT list in column order (userId, username, nickname, portraitUri)",
			mutate: func(s string) string {
				return strings.Replace(s,
					"COALESCE(portrait_uri, '')", "COALESCE(portrait_uri, ''), signature", 1)
			},
		},
		{
			name: "the table is renamed",
			what: "the table `FROM claw_im_users`",
			mutate: func(s string) string {
				return strings.Replace(s, "FROM claw_im_users", "FROM claw_im_user", 1)
			},
		},
		{
			name: "the status filter is dropped",
			what: "the `status = 'active'` filter (disabled accounts must not be searchable)",
			mutate: func(s string) string {
				return strings.Replace(s, "status = 'active'", "status IS NOT NULL", 1)
			},
		},
		{
			name: "the om_ username exclusion is dropped",
			what: "the om_ username exclusion `LEFT(username, 3) <> 'om_'",
			mutate: func(s string) string {
				return strings.Replace(s, "LEFT(username, 3) <> 'om_'", "username <> ''", 1)
			},
		},
		{
			name: "the om_ user_id exclusion is dropped",
			what: "the om_ user_id exclusion `LEFT(user_id, 3) <> 'om_'",
			mutate: func(s string) string {
				return strings.Replace(s, "LEFT(user_id, 3) <> 'om_'", "user_id <> ''", 1)
			},
		},
		{
			name: "username stops being a hit column",
			what: "a `username ILIKE $1` hit column",
			mutate: func(s string) string {
				return strings.Replace(s, "username ILIKE $1", "username = $1", 1)
			},
		},
		{
			name: "nickname stops being a hit column",
			what: "a `nickname ILIKE $1` hit column",
			mutate: func(s string) string {
				return strings.Replace(s, "COALESCE(nickname, '') ILIKE $1", "COALESCE(nickname, '') = $1", 1)
			},
		},
		{
			name: "email stops being a hit column",
			what: "an `email ILIKE $1` hit column",
			mutate: func(s string) string {
				return strings.Replace(s, "COALESCE(email, '') ILIKE $1", "COALESCE(email, '') = $1", 1)
			},
		},
		{
			name: "phone stops being a hit column",
			what: "a `phone ILIKE $1` hit column",
			mutate: func(s string) string {
				return strings.Replace(s, "COALESCE(phone, '') ILIKE $1", "COALESCE(phone, '') = $1", 1)
			},
		},
		{
			name: "user_id stops being a hit column",
			what: "a `user_id ILIKE $1` hit column (the web shows userId as the account)",
			mutate: func(s string) string {
				return strings.Replace(s, "user_id ILIKE $1", "user_id = $1", 1)
			},
		},
		{
			name: "the ordering flips to ascending",
			what: "the ordering `ORDER BY created_at DESC`",
			mutate: func(s string) string {
				return strings.Replace(s, "ORDER BY created_at DESC", "ORDER BY created_at ASC", 1)
			},
		},
		{
			name: "the LIMIT becomes a literal",
			what: "the row cap `LIMIT $2`",
			mutate: func(s string) string {
				return strings.Replace(s, "LIMIT $2", "LIMIT 20", 1)
			},
		},
	}

	for _, breakage := range breakages {
		t.Run(breakage.name, func(t *testing.T) {
			broken := userSearchNormalizeSQL(breakage.mutate(real))
			if broken == real {
				t.Fatalf("the mutation did not change the SQL, so this reverse test proves nothing — "+
					"fix the mutate function for %q", breakage.name)
			}
			checks := userSearchSQLChecks()
			var target *userSearchSQLCheck
			for i := range checks {
				if checks[i].what == breakage.what {
					target = &checks[i]
					break
				}
			}
			if target == nil {
				t.Fatalf("no check is registered with the description %q — the reverse test and "+
					"userSearchSQLChecks() drifted apart", breakage.what)
			}
			if target.re.MatchString(broken) {
				t.Errorf("the check %q still matches a broken SQL, so it is a vacuous assertion\n"+
					"--- broken SQL ---\n%s", target.what, broken)
			}
		})
	}
}

// 反向的另一半：**纯格式重构必须不红**。
//
// 上一条证明「语义改坏会被抓住」，这一条证明「只改排版不会被冤枉」—— 两半合起来
// 才说明 B 组的断言方式（空白折叠 + 逐片段正则）既够硬也不脆。具体容忍：
//   - 缩进/换行/制表符重排（有人把 WHERE 子句拆成别的行宽）；
//   - 关键字大小写（PG 会把未加引号的标识符折叠成小写，`ILIKE`/`ilike` 等价）；
//   - 逗号/括号周围的空白（`LEFT(username,3)` vs `LEFT(username, 3)`）；
//   - `<>` 写成 `!=`。
//
// 如果哪天有人把断言改回「整体字符串比对」，这条会立刻红。
func TestUserSearchQueryChecksSurviveFormattingReflow(t *testing.T) {
	real, _ := userSearchCapturedSQL(t)

	variants := []struct {
		name   string
		mutate func(string) string
	}{
		{
			name: "keywords are lowercased",
			mutate: func(s string) string {
				// 长的先换，避免 "OR" 把 "ORDER BY" 拆坏。
				for _, kw := range []string{"ORDER BY", "COALESCE", "SELECT", "FROM", "WHERE",
					"AND", "LIMIT", "LEFT", "ILIKE", "DESC", "OR"} {
					s = strings.ReplaceAll(s, kw, strings.ToLower(kw))
				}
				return s
			},
		},
		{
			name:   "<> is rewritten as !=",
			mutate: func(s string) string { return strings.ReplaceAll(s, "<>", "!=") },
		},
		{
			name:   "spaces after commas are removed",
			mutate: func(s string) string { return strings.ReplaceAll(s, ", ", ",") },
		},
		{
			name: "every space becomes a newline plus tabs",
			mutate: func(s string) string {
				return strings.ReplaceAll(s, " ", "\n\t\t")
			},
		},
		{
			name: "operators get their own lines",
			mutate: func(s string) string {
				s = strings.ReplaceAll(s, " AND ", "\n\tAND\t\t")
				s = strings.ReplaceAll(s, " OR ", "\n\tOR\t\t")
				return s
			},
		},
	}

	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			mutated := variant.mutate(real)
			if mutated == real {
				t.Fatalf("the formatting change %q did not touch the SQL, so this test proves nothing", variant.name)
			}
			sql := userSearchNormalizeSQL(mutated)
			for _, check := range userSearchSQLChecks() {
				if !check.re.MatchString(sql) {
					t.Errorf("a formatting-only change (%s) broke the check %q — the assertion is too brittle\n"+
						"--- reflowed SQL ---\n%s", variant.name, check.what, sql)
				}
			}
		})
	}
}

// 绑定参数：$1 是转义后的 %kw%，$2 是 20。
func TestUserSearchBindsTheEscapedPatternAndTheLimit(t *testing.T) {
	d := &userSearchDB{rows: &userSearchRows{}}

	if _, err := searchClawIMUsers(context.Background(), d, `a%b_c\d`, userSearchLimit); err != nil {
		t.Fatalf("searchClawIMUsers: %v", err)
	}
	if len(d.args) != 2 {
		t.Fatalf("bound args = %#v, want exactly 2 ($1 pattern, $2 limit) — $1/$2 must not drift", d.args)
	}
	if got, want := fmt.Sprint(d.args[0]), `%a\%b\_c\\d%`; got != want {
		t.Errorf("$1 = %q, want %q (LIKE metacharacters must be escaped, backslash first)", got, want)
	}
	if got := d.args[1]; got != userSearchLimit {
		t.Errorf("$2 = %#v, want %d", got, userSearchLimit)
	}
	// 类型也要对：pgx 把 interface{} 原样编码，int 才是 LIMIT 的合法类型。
	if _, ok := d.args[1].(int); !ok {
		t.Errorf("$2 has type %T, want int", d.args[1])
	}
}

// limit <= 0 时回落到服务上限（生产调用点不会传 0，但这条分支不该被留成
// 「LIMIT 0 什么都不返回」）。
func TestUserSearchDefaultsANonPositiveLimit(t *testing.T) {
	for _, limit := range []int{0, -1} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			d := &userSearchDB{rows: &userSearchRows{}}
			if _, err := searchClawIMUsers(context.Background(), d, "alice", limit); err != nil {
				t.Fatalf("searchClawIMUsers: %v", err)
			}
			if got := d.args[1]; got != userSearchLimit {
				t.Errorf("$2 = %#v for limit=%d, want the fallback %d", got, limit, userSearchLimit)
			}
		})
	}
}

// 空关键字守卫（R1 补丁，`searchClawIMUsers:106-117`）：`clawIMLikePattern("")` 是
// `%%`，在 SQL 语义上等于「匹配全表」。handler 的 400 是对外契约（A3 已覆盖），
// 但那是长在调用方身上的防线；这一层守卫的职责是「就算有人绕过 handler 直接调
// SearchUsers，也不能把前 N 条账号当搜索结果吐出去」。
//
// 因此这里断言的不是「返回空」，而是**一次 Query 都没发**：
// 用 userSearchDB（会数 query 次数）证明这一点，再用 stubDBTX（Query 故意报错）
// 反证「守卫失效时错误会立刻冒泡」，两条一起才算钉住。
func TestUserSearchBlankKeywordNeverTouchesTheDatabase(t *testing.T) {
	cases := []struct{ name, keyword string }{
		{"an empty string", ""},
		{"whitespace only", "   "},
		{"mixed whitespace", " \t\n\r "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &userSearchDB{rows: &userSearchRows{values: [][]string{
				{"100001", "alice", "爱丽丝", ""},
			}}}

			users, err := searchClawIMUsers(context.Background(), d, tc.keyword, userSearchLimit)
			if err != nil {
				t.Fatalf("searchClawIMUsers(%q) = %v, want nil — a blank keyword is not an error", tc.keyword, err)
			}
			if d.queries != 0 {
				t.Fatalf("Query calls = %d for keyword %q, want 0 — a blank keyword must not reach the database "+
					"(clawIMLikePattern(\"\") is \"%%\", i.e. the whole table)", d.queries, tc.keyword)
			}
			if users == nil {
				t.Fatal("users = nil, want an empty non-nil slice — null and [] serialize differently downstream")
			}
			if len(users) != 0 {
				t.Fatalf("users = %#v, want no results", users)
			}
		})
	}

	// 反证：换成「Query 一定报错」的 DBTX。守卫一旦被删掉/挪到 Query 之后，
	// 这里会拿到 "stub dbtx: Query must not be called" 而不是 (空, nil)。
	t.Run("a failing DBTX proves the guard short-circuits before Query", func(t *testing.T) {
		users, err := searchClawIMUsers(context.Background(), &stubDBTX{}, "   ", userSearchLimit)
		if err != nil {
			t.Fatalf("searchClawIMUsers with a failing DBTX = %v, want nil — the guard must return before Query", err)
		}
		if len(users) != 0 {
			t.Fatalf("users = %#v, want no results", users)
		}
	})
}

// R1 守卫的**直接**单测（与上一条的分工见下）。
//
// 为什么这条独立存在：上一条用会数数的假 DBTX 证明「一次 Query 都没发」，是防
// 变异的主力；但那条挂在组用例里，失败时读起来是「某组里的第 4 个子用例红了」。
// 这里用最短的路径把守卫的契约单独写死一遍 —— `stubDBTX`（同包
// im_service_commands_test.go:923-941）的 `Query` 是故意返回错误的，所以「守卫
// 真的没查库」这件事不需要计数器就能被自动证明：守卫一失效，错误会立刻冒上来。
//
// 用纯空白串（三个空格）而不是空串：这样同时证明守卫内部做的是 TrimSpace，
// 而不只是判 `""`。
func TestUserSearchBlankKeywordGuardDoesNotHitTheDatabase(t *testing.T) {
	users, err := searchClawIMUsers(context.Background(), &stubDBTX{}, "   ", userSearchLimit)
	if err != nil {
		t.Fatalf("searchClawIMUsers(\"   \") = %v, want nil — the blank-keyword guard must return "+
			"before Query; an error here means the guard is gone or moved after the query", err)
	}
	if len(users) != 0 {
		t.Fatalf("users = %#v, want no results for a blank keyword", users)
	}
	// 非 nil 是契约的一部分：`null` 与 `[]` 在 JSON 里不是一回事（handler 侧有
	// `if users == nil` 兜底，但绕过 handler 的调用方会拿到 null）。
	if users == nil {
		t.Fatal("users = nil, want an empty non-nil slice — the guard must " +
			"return make([]UserSummary, 0, limit), not nil")
	}
}

// 扫描映射：假 Rows 四列 → UserSummary 四个字段逐一对得上。
//
// 这条证明的是「Scan 按列序把夹具的 4 个值塞进 4 个字段」，**不**证明「SQL 的
// SELECT 列序与 Scan 一致」—— 后者由 userSearchSelectColumns 那条整段列清单检查
// 承担（审查员 Ray 指出过这个分工，别把两件事混成一件）。
func TestUserSearchMapsEverySelectedColumnInOrder(t *testing.T) {
	rows := &userSearchRows{values: [][]string{{
		"100001",                     // user_id → UserID
		"alice",                      // username → Username
		"爱丽丝",                        // nickname → Nickname
		"https://example.test/a.png", // portrait_uri → PortraitURI
	}}}
	d := &userSearchDB{rows: rows}

	// 夹具四值必须互不相同：下面逐列比对之所以能发现「Scan 参数对调」，全靠这四个值彼此
	// 可区分。若被人简化成同值，用例会继续绿，而对调检测已经归零 —— 那样用例名与上面的
	// 注释就都在说谎。所以在取值断言之前先硬拦一道（两个方向都由 overlay 探针实证过）。
	seen := map[string]bool{}
	for _, v := range rows.values[0] {
		if seen[v] {
			t.Fatalf("the fixture has a duplicated value %q — the per-column assertions below can no "+
				"longer detect a Scan/column swap, which is the only thing this test is here for", v)
		}
		seen[v] = true
	}

	users, err := searchClawIMUsers(context.Background(), d, "alice", userSearchLimit)
	if err != nil {
		t.Fatalf("searchClawIMUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("users = %#v, want exactly one row", users)
	}

	got := users[0]
	for _, field := range []struct{ name, got, want string }{
		{"UserID (column 1)", got.UserID, "100001"},
		{"Username (column 2)", got.Username, "alice"},
		{"Nickname (column 3)", got.Nickname, "爱丽丝"},
		{"PortraitURI (column 4)", got.PortraitURI, "https://example.test/a.png"},
	} {
		if field.got != field.want {
			t.Errorf("%s = %q, want %q (whole row: %+v)", field.name, field.got, field.want, got)
		}
	}
	if !rows.closed {
		t.Error("rows.Close() was never called — pgx leaks the connection otherwise")
	}
}

// 三条错误分支：Query 失败、Scan 失败、rows.Err() 非 nil。
// 都要求带上上下文（便于排障），且都用 %w 包住底层错误（便于上层 errors.Is）。
func TestUserSearchPropagatesQueryScanAndIterateErrors(t *testing.T) {
	t.Run("the query itself fails", func(t *testing.T) {
		sentinel := errors.New(`relation "claw_im_users" does not exist`)
		d := &userSearchDB{queryErr: sentinel}

		users, err := searchClawIMUsers(context.Background(), d, "alice", userSearchLimit)
		if err == nil {
			t.Fatal("want an error when the query fails")
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v, want it to wrap the driver error %v", err, sentinel)
		}
		if !strings.Contains(err.Error(), "search claw users") {
			t.Errorf("err = %q, want the context \"search claw users\" for the log", err.Error())
		}
		if users != nil {
			t.Errorf("users = %#v, want nil on failure", users)
		}
	})

	t.Run("scan fails", func(t *testing.T) {
		sentinel := errors.New("cannot scan NULL into *string")
		d := &userSearchDB{rows: &userSearchRows{
			values:  [][]string{{"100001", "alice", "", ""}},
			scanErr: sentinel,
		}}

		users, err := searchClawIMUsers(context.Background(), d, "alice", userSearchLimit)
		if err == nil {
			t.Fatal("want an error when Scan fails")
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v, want it to wrap the scan error %v", err, sentinel)
		}
		if !strings.Contains(err.Error(), "scan claw user") {
			t.Errorf("err = %q, want the context \"scan claw user\"", err.Error())
		}
		if users != nil {
			t.Errorf("users = %#v, want nil on failure", users)
		}
	})

	t.Run("rows.Err is set after iteration", func(t *testing.T) {
		sentinel := errors.New("connection reset by peer")
		d := &userSearchDB{rows: &userSearchRows{
			values:  [][]string{{"100001", "alice", "", ""}},
			iterErr: sentinel,
		}}

		users, err := searchClawIMUsers(context.Background(), d, "alice", userSearchLimit)
		if err == nil {
			t.Fatal("want an error when the iteration ends with rows.Err() != nil")
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v, want it to wrap the iteration error %v", err, sentinel)
		}
		if !strings.Contains(err.Error(), "iterate claw users") {
			t.Errorf("err = %q, want the context \"iterate claw users\"", err.Error())
		}
		if users != nil {
			t.Errorf("users = %#v, want nil on failure (a truncated result must not look complete)", users)
		}
	})
}

// --- C. 纯函数 ---------------------------------------------------------------

// clawIMLikePattern：`%kw%` 包裹 + LIKE 元字符转义。
// 转义顺序不能换（先反斜杠，再 % 和 _），"mixed wildcards" 那条专门钉顺序。
func TestUserSearchLikePatternEscapesWildcards(t *testing.T) {
	cases := []struct {
		name    string
		keyword string
		want    string
	}{
		{"a plain keyword is only wrapped", "alice", "%alice%"},
		{"percent is escaped", "a%b", `%a\%b%`},
		{"underscore is escaped", "a_b", `%a\_b%`},
		{"backslash is escaped first", `a\b`, `%a\\b%`},
		{"a lone percent cannot match the whole table", "%", `%\%%`},
		{"mixed wildcards keep the escape order", `%_\`, `%\%\_\\%`},
		{"an empty keyword yields the match-everything pattern (the handler must block it)", "", "%%"},
		{"chinese passes through", "张三", "%张三%"},
		{"a single quote is bound, not escaped", "o'brien", "%o'brien%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clawIMLikePattern(tc.keyword); got != tc.want {
				t.Errorf("clawIMLikePattern(%q) = %q, want %q", tc.keyword, got, tc.want)
			}
		})
	}
}

// --- D. 边界（按实现事实写预期） ---------------------------------------------

// 两处**有意**的行为，避免以后有人按直觉改坏：
//  1. 服务端不排除发送者自己（前端禁用「加自己」按钮）；
//  2. nickname 为空串时不用 username 兜底（前端 `nickname || username`）。
func TestUserSearchKeepsAnEmptyNicknameAndDoesNotFilterTheSender(t *testing.T) {
	rec, handler := newUserSearchHandler(t, &userSearchStub{result: []UserSummary{
		{UserID: "user-7", Username: "user-7", Nickname: "", PortraitURI: ""},
	}})

	sendWebCommand(t, handler, "user-7", "user", "search",
		map[string]interface{}{"keyword": "user-7"})

	list := userSearchList(t, rec.last(t))
	if len(list) != 1 {
		t.Fatalf("data.list = %#v, want one entry", list)
	}
	entry, _ := list[0].(map[string]interface{})

	if entry["userId"] != "user-7" {
		t.Errorf("list[0].userId = %#v, want the sender's own id — the server intentionally "+
			"does not filter self out (the web disables the button)", entry["userId"])
	}
	nickname, present := entry["nickname"]
	if !present {
		t.Errorf("list[0] has no nickname key at all: %#v", entry)
	} else if nickname != "" {
		t.Errorf("list[0].nickname = %#v, want the empty string — the server must not backfill "+
			"it with the username (the web does `nickname || username`)", nickname)
	}
}
