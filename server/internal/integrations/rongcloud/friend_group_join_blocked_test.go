package rongcloud

// friend_group_join_blocked_test.go 钉住「新建群聊时哪些好友根本不该出现在
// 候选名单里」这条下发给前端的判定。
//
// 用户的原话是「设备拥有者已经设置权限（关闭群聊权限）的智能体，不该再出现在
// 成员列表里」，并且明确要求**直接不显示、什么都不提示**。判定只能落在服务端：
// GET /api/claw/nodes/{id}/permissions 对非主人一律 403，前端查不到别人的开关。
//
// 这里钉的是三件容易做错的事：
//   1. 不能写成「allow_group=false 就隐藏」—— 两道闸门都有主人豁免，我自己名下
//      的 agent 即使关着我也能拉，藏起来等于把我还能用的东西藏掉；
//   2. 真人（没有节点行）与未认领节点一律不隐藏 —— 它们没有主人，也就没有可点
//      的开关，按「关」处理会把它们永久踢出所有候选名单且无自救入口；
//   3. 拿不到权限时 fail-open（一个都不标记），而不是把好友列表整个打死。

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestGroupJoinBlockedIDsMatrix 是判定矩阵本身，不碰数据库也不碰融云。
//
// 每个用例都是一条产品口径，不是实现细节：改判据时这里先红。
func TestGroupJoinBlockedIDsMatrix(t *testing.T) {
	owners := map[string]friendOwner{
		// 别人的 agent，主人关了群聊 -> 隐藏。
		"rc_b_off": {id: "user-b", name: "东风", allowGroup: false},
		// 别人的 agent，主人开着群聊 -> 保留。
		"rc_b_on": {id: "user-b", name: "东风", allowGroup: true},
		// 我自己的 agent，我自己把开关关了 -> 保留（主人豁免）。
		"rc_me_off": {id: "user-me", name: "我", allowGroup: false},
		// 我自己的 agent，开关开着 -> 保留。
		"rc_me_on": {id: "user-me", name: "我", allowGroup: true},
	}
	friendIDs := []string{
		"rc_b_off",  // 隐藏
		"rc_b_on",   // 保留
		"rc_me_off", // 保留（主人豁免）
		"rc_me_on",  // 保留
		"rc_human",  // 真人：投影里没有节点行 -> 保留
		"rc_unclaimed", // 未认领：投影 WHERE 已过滤 -> 保留
	}

	got := groupJoinBlockedIDs(friendIDs, owners, "user-me")
	want := map[string]bool{"rc_b_off": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blocked = %v, want %v", got, want)
	}

	// 反证：同一份投影换一个视角，被隐藏的人必须换。若判定里写死了某个 id 或
	// 漏了 viewer 参数，这一条会红。
	gotFromB := groupJoinBlockedIDs(friendIDs, owners, "user-b")
	wantFromB := map[string]bool{"rc_me_off": true} // 反过来，user-me 关掉的那个对 user-b 就是「别人的 agent 关了」
	if !reflect.DeepEqual(gotFromB, wantFromB) {
		t.Fatalf("blocked from user-b = %v, want %v", gotFromB, wantFromB)
	}

	gotFromStranger := groupJoinBlockedIDs(friendIDs, owners, "user-c")
	wantFromStranger := map[string]bool{"rc_b_off": true, "rc_me_off": true}
	if !reflect.DeepEqual(gotFromStranger, wantFromStranger) {
		t.Fatalf("blocked from user-c = %v, want %v", gotFromStranger, wantFromStranger)
	}
}

// TestGroupJoinBlockedIDsFailOpen 覆盖三种「拿不到判定输入」的降级：
// 空投影（无 DBTX / 查询失败）、空好友列表、空 viewer。三者都必须返回空集，
// 也就是「一个都不标记」。
//
// 空 viewer 单独列出来：decideAgentJoin 对空 inviterID 是 fail-closed（加入侧
// 宁可拒一次），但显示侧跟着 fail-closed 会让我看到自己的 agent 消失 —— 藏东西
// 的代价比多说一句话高得多，所以这里显式返回空集。
func TestGroupJoinBlockedIDsFailOpen(t *testing.T) {
	owners := map[string]friendOwner{"rc_b_off": {id: "user-b", allowGroup: false}}
	friends := []string{"rc_b_off"}

	cases := []struct {
		name   string
		friend []string
		owners map[string]friendOwner
		viewer string
	}{
		{"no projection rows", friends, nil, "user-me"},
		{"empty projection map", friends, map[string]friendOwner{}, "user-me"},
		{"no friends", nil, owners, "user-me"},
		{"blank viewer", friends, owners, "  "},
		{"unclaimed only", friends, map[string]friendOwner{"rc_b_off": {}}, "user-me"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := groupJoinBlockedIDs(tc.friend, tc.owners, tc.viewer); len(got) != 0 {
				t.Fatalf("blocked = %v, want empty (fail-open) — hiding on missing data breaks the main screen", got)
			}
		})
	}
}

// friendProjectionDB 是 rongcloud_node 投影查询的内存替身。
//
// 只认 friendOwnerProjection 那一条 SQL 的形状（四列：node / owner /
// allow_group / owner_name），其余一切报错 —— 这样「有人改了 SQL 但没改 Scan
// 顺序」会在测试里直接炸，而不是在生产上把 allow_group 的 false 扫进 ownerName。
type friendProjectionDB struct {
	rows []projectionRow
	// queryErr 非 nil 时 Query 直接失败（模拟库抖动）。
	queryErr error
	// queries 记录收到的 SQL 次数，用来断言「好友列表只读一次投影」。
	queries int
	// lastIDs 记录 ANY($1) 的参数，用来断言它是批量读而不是 N 次单读。
	lastIDs []string
}

type projectionRow struct {
	nodeRC     string
	ownerRC    string
	allowGroup bool
	ownerName  string
}

func (d *friendProjectionDB) Exec(_ context.Context, sql string, _ ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag(""), fmt.Errorf("friendProjectionDB: unexpected exec: %s", sql)
}

func (d *friendProjectionDB) Query(_ context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	if !strings.Contains(sql, "FROM rongcloud_node") || !strings.Contains(sql, "allow_group") {
		return nil, fmt.Errorf("friendProjectionDB: unexpected query: %s", sql)
	}
	d.queries++
	if ids, ok := args[0].([]string); ok {
		d.lastIDs = append([]string(nil), ids...)
	}
	if d.queryErr != nil {
		return nil, d.queryErr
	}
	return &projectionRows{rows: d.rows}, nil
}

func (d *friendProjectionDB) QueryRow(_ context.Context, sql string, _ ...interface{}) pgx.Row {
	_ = sql
	return projectionRowScanner{err: errors.New("friendProjectionDB: unexpected query row")}
}

type projectionRows struct {
	rows  []projectionRow
	index int
}

func (r *projectionRows) Close()                                       {}
func (r *projectionRows) Err() error                                   { return nil }
func (r *projectionRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT") }
func (r *projectionRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *projectionRows) RawValues() [][]byte                          { return nil }
func (r *projectionRows) Conn() *pgx.Conn                              { return nil }
func (r *projectionRows) TypeMap() *pgtype.Map                         { return nil }
func (r *projectionRows) Next() bool {
	if r.index >= len(r.rows) {
		return false
	}
	r.index++
	return true
}
func (r *projectionRows) Values() ([]interface{}, error) { return nil, errors.New("not implemented") }

// Scan 严格按 SELECT 的列序（nodeRC, ownerRC, allowGroup, ownerName）赋值。
func (r *projectionRows) Scan(dest ...interface{}) error {
	if len(dest) != 4 {
		return fmt.Errorf("projectionRows: got %d dests, want 4 (the SELECT changes shape without the Scan)", len(dest))
	}
	row := r.rows[r.index-1]
	node, ok := dest[0].(*string)
	if !ok {
		return fmt.Errorf("projectionRows: dest[0] is %T, want *string", dest[0])
	}
	owner, ok := dest[1].(*string)
	if !ok {
		return fmt.Errorf("projectionRows: dest[1] is %T, want *string", dest[1])
	}
	allowGroup, ok := dest[2].(*bool)
	if !ok {
		return fmt.Errorf("projectionRows: dest[2] is %T, want *bool", dest[2])
	}
	name, ok := dest[3].(*string)
	if !ok {
		return fmt.Errorf("projectionRows: dest[3] is %T, want *string", dest[3])
	}
	*node = row.nodeRC
	*owner = row.ownerRC
	*allowGroup = row.allowGroup
	*name = row.ownerName
	return nil
}

type projectionRowScanner struct {
	err error
}

func (s projectionRowScanner) Scan(_ ...interface{}) error { return s.err }

// TestListUserFriendsMarksOnlyOthersAgentsWithGroupDisabled 走一遍真实
// ListUserFriends：假融云给好友列表，假 DBTX 给投影，断言回包上真的出现了
// groupJoinBlocked 这个键（且只出现在该出现的那一条）。
//
// 这条不能只用纯函数替代：纯函数测不到「record 里到底打没打这个键」和
// 「SQL 四列与 Scan 四参是否对齐」。后者一旦错位，allow_group 的 false 会被
// 扫成 ownerName，判定结果全反，而纯函数测试照样全绿。
func TestListUserFriendsMarksOnlyOthersAgentsWithGroupDisabled(t *testing.T) {
	ctx := context.Background()
	stub := &claimFriendServer{friends: map[string][]string{
		"user-me": {"rc_b_off", "rc_b_on", "rc_me_off", "rc_human"},
	}}
	srv := newClaimFriendServer(t, stub)
	defer srv.Close()

	d := &friendProjectionDB{rows: []projectionRow{
		{nodeRC: "rc_b_off", ownerRC: "user-b", allowGroup: false, ownerName: "东风"},
		{nodeRC: "rc_b_on", ownerRC: "user-b", allowGroup: true, ownerName: "东风"},
		{nodeRC: "rc_me_off", ownerRC: "user-me", allowGroup: false, ownerName: "我自己"},
	}}
	svc := NewNodeService(nil, newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger()), nil, testLogger())
	svc.SetClaimDB(d)

	list, err := svc.ListUserFriends(ctx, "user-me")
	if err != nil {
		t.Fatalf("ListUserFriends: %v", err)
	}
	if len(list) != 4 {
		t.Fatalf("friends returned = %d, want 4 (filtering is the client's job, the API must not drop rows)", len(list))
	}
	// 投影是批量读一次，不是每人一次。
	if d.queries != 1 {
		t.Fatalf("projection queries = %d, want 1 (batch, never per-friend)", d.queries)
	}
	if want := []string{"rc_b_off", "rc_b_on", "rc_me_off", "rc_human"}; !reflect.DeepEqual(d.lastIDs, want) {
		t.Fatalf("projection ids = %v, want %v (every friend is a candidate)", d.lastIDs, want)
	}

	byID := make(map[string]map[string]interface{}, len(list))
	for _, rec := range list {
		id, _ := rec["userId"].(string)
		byID[id] = rec
	}
	wantBlocked := map[string]bool{
		"rc_b_off":  true,  // 别人的 agent + 主人关了群聊
		"rc_b_on":   false, // 别人的 agent + 主人开着
		"rc_me_off": false, // 我的 agent + 我自己关了 -> 主人豁免
		"rc_human":  false, // 真人
	}
	for id, want := range wantBlocked {
		rec, ok := byID[id]
		if !ok {
			t.Fatalf("friend %q missing from the response", id)
		}
		got, present := rec["groupJoinBlocked"]
		if want {
			if present && got != true {
				t.Errorf("%s.groupJoinBlocked = %#v, want true", id, got)
			}
			if !present {
				t.Errorf("%s.groupJoinBlocked is missing, want true", id)
			}
			continue
		}
		// 不想隐藏的人**不该带这个键**：前端按「有且为 true」判断，带上 false 会
		// 让「缺键」和「false」变成两种语义，日后必然有人只处理其中一种。
		if present {
			t.Errorf("%s.groupJoinBlocked = %#v, want the key to be absent", id, got)
		}
	}
}

// TestListUserFriendsFailsOpenWhenProjectionFails 钉住降级：投影查询失败时
// 好友列表照常返回、一个都不标记，而不是把整个列表变成 5xx。
//
// 理由写在 ListUserFriends 的注释里：显示侧漏标只会让用户多点一次确认、然后被
// 加入侧逐人点名拒绝；而回 5xx 会让「打开通讯录」这个主功能直接不可用。
func TestListUserFriendsFailsOpenWhenProjectionFails(t *testing.T) {
	ctx := context.Background()
	stub := &claimFriendServer{friends: map[string][]string{"user-me": {"rc_b_off", "rc_human"}}}
	srv := newClaimFriendServer(t, stub)
	defer srv.Close()

	d := &friendProjectionDB{queryErr: errors.New("connection reset by peer")}
	svc := NewNodeService(nil, newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger()), nil, testLogger())
	svc.SetClaimDB(d)

	list, err := svc.ListUserFriends(ctx, "user-me")
	if err != nil {
		t.Fatalf("ListUserFriends must degrade, not fail: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("friends returned = %d, want 2 even when the projection query fails", len(list))
	}
	for _, rec := range list {
		if _, present := rec["groupJoinBlocked"]; present {
			t.Fatalf("record %v carries groupJoinBlocked on a failed projection, want fail-open", rec["userId"])
		}
	}
}

// TestListUserFriendsWithoutDBTXMarksNothing 是无 DBTX 的装配（单测 / 未接库）。
// 此时 friendOwnerProjection 直接返回空 map，必须走同一条 fail-open 分支。
func TestListUserFriendsWithoutDBTXMarksNothing(t *testing.T) {
	ctx := context.Background()
	stub := &claimFriendServer{friends: map[string][]string{"user-me": {"rc_b_off"}}}
	srv := newClaimFriendServer(t, stub)
	defer srv.Close()

	svc := NewNodeService(nil, newRongCloudAPIClient("key", "secret", srv.URL, srv.Client(), testLogger()), nil, testLogger())
	list, err := svc.ListUserFriends(ctx, "user-me")
	if err != nil {
		t.Fatalf("ListUserFriends: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("friends returned = %d, want 1", len(list))
	}
	if _, present := list[0]["groupJoinBlocked"]; present {
		t.Fatalf("groupJoinBlocked present without a claim DB, want fail-open")
	}
}
