package rongcloud

import "testing"

// ---- groupProfileView 的 isOwner 判定 ----------------------------------------
//
// isOwner 是为了替掉客户端「拿 ownerId 与本地 id 字符串比对」那套猜法：
// 群镜像的 owner_id 对老群（历史群 / 迁移群 / 早期后端建的群）是空串，客户端
// 一比就把真正的群主判成非群主，「移除成员」整个入口跟着消失。所以这组用例的
// 重点不是「群主能拿到 true」，而是**几个不该给 true 的场合绝不能给 true**。

// ownerProfile 造一条字段齐全的群资料，用来同时钉住「原有字段一个不少」。
func ownerProfile() GroupProfile {
	return GroupProfile{
		GroupID:     "group-1",
		GroupName:   "虾说测试群",
		OwnerID:     "user-owner",
		PortraitURI: "https://cdn.example.com/g1.png",
		Description: "只用来跑单测的群",
		MemberCount: 7,
		MaxRounds:   12,
		Stopped:     true,
	}
}

func TestGroupProfileViewMarksOwnerFromOwnerViewpoint(t *testing.T) {
	view := groupProfileView(ownerProfile(), "user-owner")

	if got := view["isOwner"]; got != true {
		t.Fatalf("isOwner = %v, want true for the owner's own viewpoint", got)
	}

	// 加字段不能顺手把原有契约弄丢：这些键 web 与移动端都在读。
	want := map[string]interface{}{
		"id":                "group-1",
		"groupId":           "group-1",
		"groupName":         "虾说测试群",
		"name":              "虾说测试群",
		"ownerId":           "user-owner",
		"memberCount":       7,
		"portraitUri":       "https://cdn.example.com/g1.png",
		"avatar":            "https://cdn.example.com/g1.png",
		"description":       "只用来跑单测的群",
		"openclawMaxRounds": 12,
		"openclawStopped":   true,
	}
	for key, expected := range want {
		got, ok := view[key]
		if !ok {
			t.Fatalf("view is missing key %q (view = %v)", key, view)
		}
		if got != expected {
			t.Fatalf("view[%q] = %v, want %v", key, got, expected)
		}
	}
}

func TestGroupProfileViewIsNotOwnerForOtherMembers(t *testing.T) {
	view := groupProfileView(ownerProfile(), "user-member")

	if got := view["isOwner"]; got != false {
		t.Fatalf("isOwner = %v, want false for a plain member", got)
	}
	// ownerId 必须保留镜像原值：别的端还靠它显示「群主：xxx」。
	if got := view["ownerId"]; got != "user-owner" {
		t.Fatalf("ownerId = %v, want the mirror value untouched", got)
	}
}

func TestGroupProfileViewRefusesOwnerWhenOwnerIDIsEmpty(t *testing.T) {
	// 这条是整个改动的核心回归：owner_id 为空的老群，**任何人**都不是群主。
	// 如果哪天有人把判定写成「ownerID == viewerID」而漏了空值判断，
	// 「取不到发送者（""）」会与「owner 也是空」撞成 true —— 那是把群管理权限
	// 白送出去。所以既要测「有 viewer 也不给」，也要测「viewer 也是空」。
	profile := ownerProfile()
	profile.OwnerID = ""

	cases := []struct {
		name     string
		viewerID string
	}{
		{name: "空 owner + 真实观察者", viewerID: "user-owner"},
		{name: "空 owner + 空观察者", viewerID: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := groupProfileView(profile, tc.viewerID)["isOwner"]; got != false {
				t.Fatalf("isOwner = %v, want false when the mirror has no owner", got)
			}
		})
	}
}

func TestGroupProfileViewRefusesOwnerWhenViewerIsEmpty(t *testing.T) {
	// 未登录 / 取不到发送者时 fail-closed：群主存在，但「不知道你是谁」。
	if got := groupProfileView(ownerProfile(), "")["isOwner"]; got != false {
		t.Fatalf("isOwner = %v, want false when the viewer is unknown", got)
	}
	// 纯空白同理（command params 里常见 " " 这种值）。
	if got := groupProfileView(ownerProfile(), "   ")["isOwner"]; got != false {
		t.Fatalf("isOwner = %v, want false when the viewer is blank", got)
	}
}

func TestGroupProfileViewTrimsWhitespaceAroundOwnerAndViewer(t *testing.T) {
	// 镜像与消息头都可能带空白，判定前先 TrimSpace，否则群主会因为一个空格
	// 被判成非群主 —— 正是我们要消灭的那类「看起来功能没了」。
	profile := ownerProfile()
	profile.OwnerID = "  user-owner "

	if got := groupProfileView(profile, " user-owner\t")["isOwner"]; got != true {
		t.Fatalf("isOwner = %v, want true after trimming both sides", got)
	}
}

func TestGroupProfileViewsJudgesEachProfileIndependently(t *testing.T) {
	// getProfiles / getMyGroups 是批量接口：同一个 viewer 对不同的群答案不同，
	// 逐条判定、互不串味。
	owned := ownerProfile()
	notOwned := ownerProfile()
	notOwned.GroupID = "group-2"
	notOwned.GroupName = "别人的群"
	notOwned.OwnerID = "user-other"
	orphan := ownerProfile()
	orphan.GroupID = "group-3"
	orphan.GroupName = "无主老群"
	orphan.OwnerID = ""

	views := groupProfileViews([]GroupProfile{owned, notOwned, orphan}, "user-owner")

	if len(views) != 3 {
		t.Fatalf("len(views) = %d, want 3", len(views))
	}
	if got := views[0]["isOwner"]; got != true {
		t.Fatalf("views[0].isOwner = %v, want true", got)
	}
	if got := views[1]["isOwner"]; got != false {
		t.Fatalf("views[1].isOwner = %v, want false", got)
	}
	if got := views[2]["isOwner"]; got != false {
		t.Fatalf("views[2].isOwner = %v, want false for the ownerless group", got)
	}

	// ownerId 必须逐条保留各自原值，不能被 isOwner 覆盖或串位。
	wantOwners := []string{"user-owner", "user-other", ""}
	for i, wantOwner := range wantOwners {
		if got := views[i]["ownerId"]; got != wantOwner {
			t.Fatalf("views[%d].ownerId = %v, want %q", i, got, wantOwner)
		}
	}
	// 顺序即入参顺序：前端按下标对齐群列表。
	wantIDs := []string{"group-1", "group-2", "group-3"}
	for i, wantID := range wantIDs {
		if got := views[i]["groupId"]; got != wantID {
			t.Fatalf("views[%d].groupId = %v, want %q", i, got, wantID)
		}
	}
}

func TestGroupProfileViewsHandlesEmptyInput(t *testing.T) {
	// 空列表要返回空切片而不是 nil：JSON 序列化后分别是 [] 与 null，
	// 客户端对 null 做 .map 会炸。
	views := groupProfileViews(nil, "user-owner")
	if views == nil {
		t.Fatal("views = nil, want an empty non-nil slice")
	}
	if len(views) != 0 {
		t.Fatalf("len(views) = %d, want 0", len(views))
	}
}
