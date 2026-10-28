package rongcloud

import (
	"strings"
	"testing"
)

// portrait_group_test.go 钉住「群类会话默认头像」的决议表。
//
// 这张表是移动端头像的唯一定义：群聊写进融云群资料（小程序/App 读它），讨论组和
// 聊天室写进 rongcloud_chatroom.portrait_uri（融云聊天室 API 没有头像字段）。所以
// 每条分支都要有断言，尤其是「未知 kind 不许拼出地址」和「自定义头像不许被覆盖」
// 这两条 —— 前者会换回 404 的图，后者会改掉用户自己设的群头像。

func clearGroupPortraitEnv(t *testing.T) {
	t.Helper()
	t.Setenv(GroupPortraitBaseEnv, "")
	t.Setenv("CLAW_DEFAULT_PORTRAIT_URI", "")
}

func TestGroupPortraitURIKindMatrix(t *testing.T) {
	clearGroupPortraitEnv(t)

	cases := []struct {
		name string
		kind string
		want string
	}{
		{"group", GroupPortraitKindGroup, DefaultGroupPortraitBaseURL + "/group.png"},
		{"discussion", GroupPortraitKindDiscussion, DefaultGroupPortraitBaseURL + "/discussion.png"},
		{"chatroom", GroupPortraitKindChatroom, DefaultGroupPortraitBaseURL + "/chatroom.png"},
		{"surrounding spaces", "  group  ", DefaultGroupPortraitBaseURL + "/group.png"},
		{"upper case", "CHATROOM", DefaultGroupPortraitBaseURL + "/chatroom.png"},
		// 未知 / 空 kind 必须回落到统一默认图，绝不能拼出 <base>//.png 这种 404。
		{"empty", "", DefaultUserPortraitURI},
		{"unknown", "voice-room", DefaultUserPortraitURI},
		{"path traversal attempt", "../logo", DefaultUserPortraitURI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GroupPortraitURI(tc.kind); got != tc.want {
				t.Fatalf("GroupPortraitURI(%q) = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}

func TestGroupPortraitBaseEnvOverride(t *testing.T) {
	clearGroupPortraitEnv(t)

	// 末尾斜杠、首尾空白都要吃掉，不能拼出 //group.png。
	t.Setenv(GroupPortraitBaseEnv, "  https://cdn.example.com/claw/avatars/  ")
	want := "https://cdn.example.com/claw/avatars/group.png"
	if got := GroupPortraitURI(GroupPortraitKindGroup); got != want {
		t.Fatalf("override = %q, want %q", got, want)
	}

	// 纯空白视作未设置 → 回到线上基址。
	t.Setenv(GroupPortraitBaseEnv, "   ")
	if got := GroupPortraitBase(); got != DefaultGroupPortraitBaseURL {
		t.Fatalf("blank env must fall back to the canonical base, got %q", got)
	}

	// 群类图源与真人统一默认图是两套 env：覆盖群基址不能影响 default_user.png。
	t.Setenv("CLAW_DEFAULT_PORTRAIT_URI", "https://cdn.example.com/all.png")
	t.Setenv(GroupPortraitBaseEnv, "https://cdn.example.com/claw/avatars")
	if got := GroupPortraitURI(GroupPortraitKindDiscussion); got != "https://cdn.example.com/claw/avatars/discussion.png" {
		t.Fatalf("discussion = %q, want the group base override", got)
	}
	if got := DefaultUserPortrait(); got != "https://cdn.example.com/all.png" {
		t.Fatalf("group base leaked into the unified default: %q", got)
	}
}

func TestGroupPortraitIfEmpty(t *testing.T) {
	clearGroupPortraitEnv(t)

	cases := []struct {
		name     string
		kind     string
		portrait string
		want     string
	}{
		{"empty falls back by kind", GroupPortraitKindChatroom, "", GroupPortraitURI(GroupPortraitKindChatroom)},
		{"spaces fall back by kind", GroupPortraitKindDiscussion, "   ", GroupPortraitURI(GroupPortraitKindDiscussion)},
		{"custom is kept", GroupPortraitKindGroup, "https://cdn.example.com/uploads/g.png", "https://cdn.example.com/uploads/g.png"},
		{"custom is trimmed", GroupPortraitKindGroup, "  https://cdn.example.com/uploads/g.png  ", "https://cdn.example.com/uploads/g.png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GroupPortraitIfEmpty(tc.kind, tc.portrait); got != tc.want {
				t.Fatalf("GroupPortraitIfEmpty(%q, %q) = %q, want %q", tc.kind, tc.portrait, got, tc.want)
			}
		})
	}
}

func TestChatroomPortraitKindMapping(t *testing.T) {
	cases := []struct {
		conversationKind string
		want             string
	}{
		{"discussion", GroupPortraitKindDiscussion},
		{"Discussion", GroupPortraitKindDiscussion},
		{"  discussion  ", GroupPortraitKindDiscussion},
		{"chatroom", GroupPortraitKindChatroom},
		// 空值与未知值一律算聊天室：与 web 端「room 缺 conversationKind 时算聊天室」
		// 的判定一致，历史行也有确定归属。
		{"", GroupPortraitKindChatroom},
		{"  ", GroupPortraitKindChatroom},
		{"group", GroupPortraitKindChatroom},
	}
	for _, tc := range cases {
		t.Run(tc.conversationKind, func(t *testing.T) {
			if got := ChatroomPortraitKind(tc.conversationKind); got != tc.want {
				t.Fatalf("ChatroomPortraitKind(%q) = %q, want %q", tc.conversationKind, got, tc.want)
			}
		})
	}
}

func TestIsGroupDefaultPortrait(t *testing.T) {
	clearGroupPortraitEnv(t)

	defaults := []string{
		DefaultGroupPortraitBaseURL + "/group.png",
		DefaultGroupPortraitBaseURL + "/discussion.png",
		DefaultGroupPortraitBaseURL + "/chatroom.png",
		// 同一张图挂在别的 host 上（env 覆盖 / 迁移 CDN）仍然是默认图：判定只看
		// path，不看域名。
		"https://cdn.example.com/claw-web/group-avatars/group.png",
		DefaultGroupPortraitBaseURL + "/discussion.png?v=2",
	}
	for _, uri := range defaults {
		if !IsGroupDefaultPortrait(uri) {
			t.Fatalf("%q must be recognised as a group default", uri)
		}
	}

	custom := []string{
		"",
		DefaultUserPortraitURI,
		"https://cdn.example.com/uploads/group-avatar.png",
		"https://cdn.example.com/uploads/group-avatars/group.png.bak",
		// 目录名撞车：用户自己传的群头像放在 uploads/group-avatars/ 下，文件名也
		// 可能叫 group.png —— 模糊匹配会把它当默认图覆盖掉，必须判否。
		"https://cdn.example.com/uploads/group-avatars/group.png",
		"https://cdn.example.com/user-media/group.png",
		"https://cdn.example.com/claw-web/group-avatars/avatar.png",
		// 只差一层目录（/claw-web/other/…）也不算。
		"https://cdn.example.com/claw-web/other/group-avatars/group.png",
	}
	for _, uri := range custom {
		if IsGroupDefaultPortrait(uri) {
			t.Fatalf("%q must NOT be treated as a group default", uri)
		}
	}
}

// env 换了基址之后，新基址下的三张图同样算默认图（否则切换到 CDN 后回填会把
// 自己刚写下去的地址当成「自定义头像」，从此再也刷不动）。
func TestIsGroupDefaultPortraitFollowsEnvBase(t *testing.T) {
	clearGroupPortraitEnv(t)
	t.Setenv(GroupPortraitBaseEnv, "https://cdn.example.com/claw/avatars")

	for _, kind := range []string{GroupPortraitKindGroup, GroupPortraitKindDiscussion, GroupPortraitKindChatroom} {
		if !IsGroupDefaultPortrait("https://cdn.example.com/claw/avatars/" + kind + ".png") {
			t.Fatalf("%s under the env base must be a default", kind)
		}
	}
	// 线上常量那套写法在 env 覆盖后仍然认（存量行就是那个 host）。
	if !IsGroupDefaultPortrait(DefaultGroupPortraitBaseURL + "/group.png") {
		t.Fatal("the canonical base must stay recognised after an env override")
	}
}

func TestNeedsGroupPortraitFixMatrix(t *testing.T) {
	clearGroupPortraitEnv(t)

	groupTarget := GroupPortraitURI(GroupPortraitKindGroup)
	discussionTarget := GroupPortraitURI(GroupPortraitKindDiscussion)

	cases := []struct {
		name    string
		current string
		target  string
		want    bool
	}{
		// 已经是对的：幂等跳过，重跑不能每次都往融云打一遍。
		{"already the target", groupTarget, groupTarget, false},

		// 要改的：空值、统一默认图（换图之前所有群都落在这）、历史默认值。
		{"empty", "", groupTarget, true},
		{"whitespace", "   ", groupTarget, true},
		{"unified default", DefaultUserPortraitURI, groupTarget, true},
		{"legacy openclaw", legacyDefaultPortraitURIs[0], groupTarget, true},
		{"legacy static path", "https://newsradar.dreamdt.cn/im-test/api/static/claw/avatar-default.png", groupTarget, true},
		// 类型判错 / 换过图的行要能被纠正（讨论组图被当成群图写进去的场景）。
		{"another kind's default", discussionTarget, groupTarget, true},
		{"group default under an old host", "https://cdn.example.com/claw-web/group-avatars/group.png", groupTarget, true},

		// 不许动的：用户自定义头像。
		{"user upload", "https://cdn.example.com/uploads/u1/group.png", groupTarget, false},
		{"user upload named group-avatars", "https://cdn.example.com/uploads/group-avatars/group.png", groupTarget, false},
		{"third party", "https://lh3.googleusercontent.com/a/abc", groupTarget, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsGroupPortraitFix(tc.current, tc.target); got != tc.want {
				t.Fatalf("NeedsGroupPortraitFix(%q, %q) = %v, want %v", tc.current, tc.target, got, tc.want)
			}
		})
	}
}

// 群类默认图必须是 PNG：原生 App 的头像加载器（Glide / SDWebImage）解不了 SVG，
// 写成 .svg 的后果是手机上白图 —— 而这条链路改完就只能靠肉眼在手机上发现。
func TestGroupPortraitURIsArePNG(t *testing.T) {
	clearGroupPortraitEnv(t)

	for _, kind := range []string{GroupPortraitKindGroup, GroupPortraitKindDiscussion, GroupPortraitKindChatroom} {
		uri := GroupPortraitURI(kind)
		if !strings.HasSuffix(uri, "/"+kind+".png") {
			t.Fatalf("GroupPortraitURI(%q) = %q, want a .png under /group-avatars", kind, uri)
		}
		if strings.Contains(uri, ".svg") {
			t.Fatalf("GroupPortraitURI(%q) = %q, SVG is not renderable on native clients", kind, uri)
		}
	}

	// GroupDefaultPortraitURIs 供回填的 SQL 预筛做等值匹配，必须覆盖三张 + 统一图。
	all := GroupDefaultPortraitURIs()
	if len(all) != 4 {
		t.Fatalf("GroupDefaultPortraitURIs() = %d entries, want 4 (%v)", len(all), all)
	}
	if all[0] != DefaultUserPortrait() {
		t.Fatalf("the first entry must be the unified default, got %q", all[0])
	}
	for _, kind := range []string{GroupPortraitKindGroup, GroupPortraitKindDiscussion, GroupPortraitKindChatroom} {
		found := false
		for _, uri := range all {
			if uri == GroupPortraitURI(kind) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s is missing from GroupDefaultPortraitURIs (%v)", kind, all)
		}
	}
}
