package handler

// 讨论主持「解绑原用户」这一步曾经把 rongcloud_node.owner_user_id 写成 NULL，
// 而该列在 564 号迁移里是 UUID NOT NULL —— 于是每次保存都在 UPDATE 上撞约束，
// 配置其实已经落库，前端却恒定收到 502「解除该节点原用户绑定失败」。
//
// 这类「代码写法和 schema 对不上」的错，handler 层跑不出来（要真库），普通单测
// 也不会红，所以单独用一条静态守卫钉住：schema 一天是 NOT NULL，代码就一天不许
// 往这一列写 NULL。守卫自己带负向样本（TestOwnerUserIDGuardDetectsViolation），
// 免得哪天正则写坏了它变成永远绿的摆设。

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ownerUserIDNullAssign 抓的是 SQL 里对 owner_user_id 赋 NULL 的写法，兼容任意
// 空白与大小写（owner_user_id = NULL / owner_user_id=NULL / OWNER_USER_ID=null）。
var ownerUserIDNullAssign = regexp.MustCompile(`(?i)owner_user_id\s*=\s*null`)

// ownerUserIDScan 是守卫的扫描内核，root 下所有非测试 .go 文件逐行过一遍。
// scanned/sawColumnMention 两个返回值供调用方做正控：路径写错或过滤条件不对时，
// 「零命中」会伪装成通过。
func ownerUserIDScan(root string) (scanned int, sawColumnMention bool, offenders []string, err error) {
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		scanned++
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "owner_user_id") {
				sawColumnMention = true
			}
			// 整行注释跳过：注释里要能讨论这段历史，不该被守卫拦住。
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if ownerUserIDNullAssign.MatchString(line) {
				offenders = append(offenders, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	return scanned, sawColumnMention, offenders, walkErr
}

// rongcloudNodeOwnerColumnIsNotNull 从 564 号迁移里把 rongcloud_node 的建表块
// 抽出来，判断 owner_user_id 是否带 NOT NULL。第二个返回值报告「建表块找到了」，
// 用来给调用方当正控 —— 找不到建表块时绝不能安静地判通过。
func rongcloudNodeOwnerColumnIsNotNull(t *testing.T) (notNull bool, blockFound bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", "564_rongcloud_tables.up.sql"))
	if err != nil {
		t.Fatalf("读取 564_rongcloud_tables.up.sql 失败: %v", err)
	}
	text := string(raw)
	start := strings.Index(text, "CREATE TABLE IF NOT EXISTS rongcloud_node (")
	if start < 0 {
		return false, false
	}
	rest := text[start:]
	end := strings.Index(rest, ");")
	if end < 0 {
		return false, false
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "owner_user_id" {
			return strings.Contains(line, "NOT NULL"), true
		}
	}
	return false, false
}

// TestRongcloudNodeOwnerUserIDIsNotNull 是守卫的前提断言：如果哪天有人放开这一
// 列的 NOT NULL，这条会主动变红，提示下面那条守卫需要重新评估 —— 而不是让它在
// 空转中悄悄失效。它同时是正控：证明建表块真的被解析到了。
func TestRongcloudNodeOwnerUserIDIsNotNull(t *testing.T) {
	notNull, found := rongcloudNodeOwnerColumnIsNotNull(t)
	if !found {
		t.Fatal("没有在 564_rongcloud_tables.up.sql 里找到 rongcloud_node 的 owner_user_id 定义")
	}
	if !notNull {
		t.Fatal("rongcloud_node.owner_user_id 已不再是 NOT NULL：请复核 " +
			"clawAdminUnbindHostNode 的注释与 TestNoCodeWritesNullOwnerUserID 是否仍然成立")
	}
}

// TestNoCodeWritesNullOwnerUserID 扫描 server/internal 下的非测试源码，要求没有
// 任何一处把 owner_user_id 赋成 NULL。
func TestNoCodeWritesNullOwnerUserID(t *testing.T) {
	const root = ".." // server/internal
	scanned, sawMention, offenders, err := ownerUserIDScan(root)
	if err != nil {
		t.Fatalf("遍历 %s 失败: %v", root, err)
	}
	// 正控：真的扫到了源码，且真的在源码里见过这一列的名字。
	if scanned < 50 {
		t.Fatalf("只扫描到 %d 个 .go 文件，正控失败（路径或过滤条件不对）", scanned)
	}
	if !sawMention {
		t.Fatal("扫描范围内一次都没出现 owner_user_id，正控失败（扫描内容可疑）")
	}
	if len(offenders) > 0 {
		t.Fatalf("owner_user_id 是 NOT NULL（564_rongcloud_tables.up.sql），不能赋 NULL，发现 %d 处：\n%s",
			len(offenders), strings.Join(offenders, "\n"))
	}
}

// TestOwnerUserIDGuardDetectsViolation 是守卫的负向正控：往临时目录里放一份
// 含违规写法的样本，要求扫描器抓得到；同一份样本里的注释行必须不被抓。
// 少了这条，「零命中」永远无法区分「代码干净」和「正则/过滤坏了」。
func TestOwnerUserIDGuardDetectsViolation(t *testing.T) {
	dir := t.TempDir()
	sample := `package sample

func bad() {
	// 注释里提 owner_user_id = NULL 不算违规
	exec("UPDATE rongcloud_node SET owner_user_id = NULL WHERE id = $1")
	exec("update rongcloud_node set OWNER_USER_ID=null where id = $1")
	exec("UPDATE rongcloud_node SET owner_user_id = NULL, updated_at = now() WHERE id = $1")
}

func fine() {
	exec("SELECT rongcloud_user_id FROM rongcloud_node WHERE owner_user_id IS NOT NULL")
}
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(sample), 0o600); err != nil {
		t.Fatalf("写样本失败: %v", err)
	}
	// 测试文件必须被跳过：守卫只管生产代码。
	if err := os.WriteFile(filepath.Join(dir, "sample_test.go"),
		[]byte("package sample\n\n// owner_user_id = NULL in a fixture is fine\n"), 0o600); err != nil {
		t.Fatalf("写测试样本失败: %v", err)
	}

	scanned, sawMention, offenders, err := ownerUserIDScan(dir)
	if err != nil {
		t.Fatalf("扫描临时目录失败: %v", err)
	}
	if scanned != 1 {
		t.Fatalf("期望只扫描 1 个非测试 .go 文件（_test.go 应被跳过），实际 %d", scanned)
	}
	if !sawMention {
		t.Fatal("样本里明明有 owner_user_id，sawColumnMention 却是 false")
	}
	if len(offenders) != 3 {
		t.Fatalf("期望抓到 3 处违规（三种写法各一处，注释行不算），实际 %d 处：\n%s",
			len(offenders), strings.Join(offenders, "\n"))
	}
}
