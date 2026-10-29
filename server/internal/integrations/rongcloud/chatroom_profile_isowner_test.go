package rongcloud

import "testing"

// ---- chatroomViewData 的 isOwner 判定 ----------------------------------------
//
// 讨论组 / 聊天室（前端 `type: 'room'`）与群聊是同一类问题：房主身份原先由客户端
// 拿 `ownerId` 与本地两套 id 做字符串比对猜出来，而 `owner_rc_user_id` 在这一列
// 被 JOIN 出来之前是 NULL、`ownerRCID()` 返回空串 —— 老房间上真正的房主被判成
// 非房主，「移除成员」入口整块消失，而「添加成员」不判权限所以还在。
//
// 所以这组用例的重点与群聊那份一致：不是「房主能拿到 true」，而是**几个不该给
// true 的场合绝不能给 true**。
//
// 判定逻辑在 `ownerViewpoint`（群聊/聊天室共用一处），这里测的是
// `chatroomViewData` 把它正确接到了房主 id 与观察者 id 上。

// fakeChatroomView 是最小的 chatroomView 实现，只用来喂 chatroomViewData。
// `ownerRCID()` 这里刻意用**人肉 trim 后返回值**的方式模拟：真实实现会在
// owner_rc_user_id 为 NULL 时返回空串，本 stub 也照此返回 ownerRC。
type fakeChatroomView struct {
	id       string
	roomName string
	portrait string
	ownerRC  string
	hostNode interface{}
	rounds   int32
	kind     string
}

func (v fakeChatroomView) rcChatroomID() string            { return v.id }
func (v fakeChatroomView) name() string                    { return v.roomName }
func (v fakeChatroomView) portraitUri() string             { return v.portrait }
func (v fakeChatroomView) ownerRCID() string               { return v.ownerRC }
func (v fakeChatroomView) hostBusinessNodeID() interface{} { return v.hostNode }
func (v fakeChatroomView) maxRounds() int32                { return v.rounds }
func (v fakeChatroomView) createdAt() interface{}          { return "2026-10-01T00:00:00Z" }
func (v fakeChatroomView) updatedAt() interface{}          { return "2026-10-02T00:00:00Z" }
func (v fakeChatroomView) conversationKind() string        { return v.kind }

func stubRoom() fakeChatroomView {
	return fakeChatroomView{
		id:       "rc_room_1",
		roomName: "方案室",
		portrait: "https://cdn.example.com/room1.png",
		ownerRC:  "rc_owner",
		hostNode: "node-host",
		rounds:   8,
		kind:     "discussion",
	}
}

func TestChatroomViewDataMarksOwnerFromOwnerViewpoint(t *testing.T) {
	rec := chatroomViewData(stubRoom(), nil, false, "rc_owner")

	if got := rec["isOwner"]; got != true {
		t.Fatalf("isOwner = %v, want true for the owner's own viewpoint", got)
	}

	// 加字段不能把原有契约弄丢：web 与移动端都在读这些键。
	want := map[string]interface{}{
		"chatroomId":        "rc_room_1",
		"chatroomName":      "方案室",
		"portraitUri":       "https://cdn.example.com/room1.png",
		"ownerId":           "rc_owner",
		"hostNodeId":        "node-host",
		"openclawMaxRounds": int32(8),
		"createTime":        "2026-10-01T00:00:00Z",
		"updateTime":        "2026-10-02T00:00:00Z",
		"conversationKind":  "discussion",
	}
	for key, expected := range want {
		got, ok := rec[key]
		if !ok {
			t.Fatalf("rec is missing key %q (rec = %v)", key, rec)
		}
		if got != expected {
			t.Fatalf("rec[%q] = %v, want %v", key, got, expected)
		}
	}
}

func TestChatroomViewDataKeepsMembersAlongsideIsOwner(t *testing.T) {
	// includeMembers=true 的那几条路径（getInfo / update / create / setHost /
	// setDiscussionTeam）回包是「房间资料 + 成员」，isOwner 不能把成员挤掉。
	members := []map[string]interface{}{
		{"memberId": "rc_owner", "memberType": "user"},
		{"memberId": "rc_agent", "memberType": "ai"},
	}
	rec := chatroomViewData(stubRoom(), members, true, "rc_owner")

	if got := rec["isOwner"]; got != true {
		t.Fatalf("isOwner = %v, want true", got)
	}
	if got := rec["memberCount"]; got != 2 {
		t.Fatalf("memberCount = %v, want 2", got)
	}
	aiMembers, ok := rec["aiMembers"].([]map[string]interface{})
	if !ok || len(aiMembers) != 1 || aiMembers[0]["memberId"] != "rc_agent" {
		t.Fatalf("aiMembers = %v, want only the ai member", rec["aiMembers"])
	}
}

func TestChatroomViewDataIsNotOwnerForOtherMembers(t *testing.T) {
	rec := chatroomViewData(stubRoom(), nil, false, "rc_member")

	if got := rec["isOwner"]; got != false {
		t.Fatalf("isOwner = %v, want false for a plain member", got)
	}
	// ownerId 必须保留原值：别的端还靠它显示「房主：xxx」。
	if got := rec["ownerId"]; got != "rc_owner" {
		t.Fatalf("ownerId = %v, want the room value untouched", got)
	}
}

func TestChatroomViewDataRefusesOwnerWhenOwnerRCIDIsEmpty(t *testing.T) {
	// owner_rc_user_id 为 NULL → ownerRCID() 返回 ""。此时**任何人**都不是房主。
	// 第二个子用例（viewer 也是空）专防 `"" == ""` 撞成 true —— 那是把房主权限
	// 白送给「所有取不到发送者的调用」。
	room := stubRoom()
	room.ownerRC = ""

	cases := []struct {
		name     string
		viewerID string
	}{
		{name: "空 owner + 真实观察者", viewerID: "rc_owner"},
		{name: "空 owner + 空观察者", viewerID: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := chatroomViewData(room, nil, false, tc.viewerID)
			if got := rec["isOwner"]; got != false {
				t.Fatalf("isOwner = %v, want false when the room has no owner", got)
			}
			// ownerId 仍是空串原值，不被 isOwner 的判定污染。
			if got := rec["ownerId"]; got != "" {
				t.Fatalf("ownerId = %v, want the empty original value", got)
			}
		})
	}
}

func TestChatroomViewDataRefusesOwnerWhenViewerIsEmpty(t *testing.T) {
	// 内部调用 / 未登录 / 取不到发送者：房主存在，但「不知道你是谁」。
	if got := chatroomViewData(stubRoom(), nil, false, "")["isOwner"]; got != false {
		t.Fatalf("isOwner = %v, want false when the viewer is unknown", got)
	}
	if got := chatroomViewData(stubRoom(), nil, false, "   ")["isOwner"]; got != false {
		t.Fatalf("isOwner = %v, want false when the viewer is blank", got)
	}
}

func TestChatroomViewDataTrimsWhitespaceAroundOwnerAndViewer(t *testing.T) {
	room := stubRoom()
	room.ownerRC = "  rc_owner "

	if got := chatroomViewData(room, nil, false, " rc_owner\t")["isOwner"]; got != true {
		t.Fatalf("isOwner = %v, want true after trimming both sides", got)
	}
}

func TestChatroomViewDataJudgesEachRoomIndependently(t *testing.T) {
	// getList 是批量接口：同一个观察者对不同房间答案不同，逐条判定、互不串味。
	owned := stubRoom()
	mine := stubRoom()
	mine.id = "rc_room_2"
	mine.roomName = "别人的房"
	mine.ownerRC = "rc_other"
	orphan := stubRoom()
	orphan.id = "rc_room_3"
	orphan.roomName = "无主老房"
	orphan.ownerRC = ""

	rooms := []fakeChatroomView{owned, mine, orphan}
	recs := make([]map[string]interface{}, 0, len(rooms))
	for _, room := range rooms {
		recs = append(recs, chatroomViewData(room, nil, false, "rc_owner"))
	}

	wantOwnerFlags := []bool{true, false, false}
	wantOwnerIDs := []string{"rc_owner", "rc_other", ""}
	wantIDs := []string{"rc_room_1", "rc_room_2", "rc_room_3"}
	for i := range recs {
		if got := recs[i]["isOwner"]; got != wantOwnerFlags[i] {
			t.Fatalf("recs[%d].isOwner = %v, want %v", i, got, wantOwnerFlags[i])
		}
		if got := recs[i]["ownerId"]; got != wantOwnerIDs[i] {
			t.Fatalf("recs[%d].ownerId = %v, want %q", i, got, wantOwnerIDs[i])
		}
		// 顺序即入参顺序：前端按列表顺序渲染会话。
		if got := recs[i]["chatroomId"]; got != wantIDs[i] {
			t.Fatalf("recs[%d].chatroomId = %v, want %q", i, got, wantIDs[i])
		}
	}
}
