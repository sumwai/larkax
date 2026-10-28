package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
)

// 这一组用例钉的是 `migrate create` 的**写入面**：它往 `internal/migrate/sql/`
// 写文件，而那个目录是 //go:embed sql/*.sql —— 写进去的东西下一次构建就进二进制，
// 别人部署时 `migrate up` 会真的执行它。
//
// 为什么这一组必须在**临时目录**里跑，不能在包的目录里跑：
//
//	go test 的工作目录是 cmd/larkaxctl/，那里没有 internal/migrate/sql。goose 在
//	那种环境下打开文件会失败（rc=1），于是「不产生文件」的断言**永远是绿的**——
//	它测的不是「命令拒绝了这个名字」，是「这个目录不存在」。跳过与通过在输出上
//	长得一样（见 LESSONS.md 第 16 条），一条永远绿的安全用例比没有更坏。
//
// 所以每条用例都 t.Chdir 到一个临时目录，并**先建好 migrationsDir 这一层完整
// 路径**（它是相对 cwd 的常量，少一层就又变回「写不进去」）。

// migrationTreeSnapshot 把一份目录树读成「相对路径 → 内容哈希」。
//
// 比文件数而不比内容是不够的：同一条命令若改成既写 up 又写 down、或者写了一对
// 同名但内容不同的文件，数目不变而内容变了。比哈希才看得出「这一趟到底动了什么」。
func migrationTreeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()

	snap := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(raw)
		snap[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("读目录树 %s 失败：%v", root, err)
	}
	return snap
}

// describeSnapshotDiff 把两份快照的差异说成人话：新增、删除、改了内容。
func describeSnapshotDiff(before, after map[string]string) string {
	var added, removed, changed []string
	for name, sum := range after {
		old, ok := before[name]
		switch {
		case !ok:
			added = append(added, name)
		case old != sum:
			changed = append(changed, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)

	var b strings.Builder
	if len(added) > 0 {
		fmt.Fprintf(&b, "新增 %v；", added)
	}
	if len(removed) > 0 {
		fmt.Fprintf(&b, "删除 %v；", removed)
	}
	if len(changed) > 0 {
		fmt.Fprintf(&b, "内容变了 %v；", changed)
	}
	if b.Len() == 0 {
		return "目录树没有变化"
	}
	return strings.TrimSuffix(b.String(), "；")
}

// sandboxForMigrateCreate 造一个「看起来像仓库根」的临时目录，并切进去。
//
// 位置与层数都必须是 **migrationsDir 的原文**（internal/migrate/sql）：这条命令
// 把目标目录写成了相对于 cwd 的常量，少一层目录它就会打不开文件、静默变成
// 「不产生副作用」——那正是这一组用例要避免的假绿。
//
// 返回的是那个 sql 目录的绝对路径。目录里**预置一个哨兵文件**：它证明「读到的
// 树」不是空的，也把「新增又删除」这种情况逼出来（只看最后有几个文件时，
// 建一个再删一个与什么都没做长得一样）。
func sandboxForMigrateCreate(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	sqlDir := filepath.Join(root, filepath.FromSlash(migrationsDir))
	if err := os.MkdirAll(sqlDir, 0o755); err != nil {
		t.Fatalf("建 %s 失败：%v", sqlDir, err)
	}
	if err := os.WriteFile(filepath.Join(sqlDir, "sentinel.md"),
		[]byte("哨兵：这一份不是迁移文件，只用来证明目录读得到\n"), 0o644); err != nil {
		t.Fatalf("写哨兵文件失败：%v", err)
	}

	t.Chdir(root)
	return sqlDir
}

// TestMigrateCreateRejectsNamesThatWouldBecomeFiles 钉住「拒掉的名字一个文件都不许留」。
//
// 两条输入各自代表一种「会静默变成一个看起来正常的迁移文件」的写法：
//
//   - `help`：不带横线，是 `migrate <子命令> help` 这条帮助入口的词，HelpOnly
//     认不出它（它只认 -h/--help），于是它一路走到 goose 的写入点。
//   - 空串：落成 `*_.sql`，今天就能触发。
//
// **`-h extra` 不在这张表里**：HelpOnly 放宽成「第一个参数是求助旗标」之后，
// 它被这一层收掉、打出帮助、rc=0，不再走到验证这一关。它仍然不许建文件，
// 但判据不再是「报参数错」——见 TestMigrateCreateTreatsLeadingHelpFlagAsHelp。
//
// 全部断言**目录树逐字节不变**，不是只数文件：见 migrationTreeSnapshot。
func TestMigrateCreateRejectsNamesThatWouldBecomeFiles(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"help 这个词", []string{"create", "help"}},
		{"空串", []string{"create", ""}},
		{"以横线开头的普通词", []string{"create", "-x"}},
		{"非 ASCII", []string{"create", "中文名"}},
		{"带点的词", []string{"create", "a.b"}},
		{"数字开头", []string{"create", "1abc"}},
		// 与「空串」各挡一种放宽方式：空串挡「正则被放成 *」，这条挡
		// 「正则被放成 [a-zA-Z0-9_]*$」（尾下划线放行）。两条少一条，
		// 那种放宽就不会被任何断言照出来。
		{"以下划线结尾", []string{"create", "x_"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sqlDir := sandboxForMigrateCreate(t)
			before := migrationTreeSnapshot(t, sqlDir)

			err := runMigrate(tc.args)

			after := migrationTreeSnapshot(t, sqlDir)
			if diff := describeSnapshotDiff(before, after); diff != "目录树没有变化" {
				t.Fatalf("%v 产生了副作用：%s\n"+
					"这个目录是 //go:embed sql/*.sql——写进去的东西下次构建就进二进制，"+
					"别人部署时 migrate up 会真的执行它。", tc.args, diff)
			}
			if err == nil {
				t.Fatalf("%v 应当报参数错（退出码 2），却成功了：这个名字留下了文件或者被当成合法输入", tc.args)
			}
			if code := cli.ExitCode(err); code != 2 {
				t.Errorf("%v 的退出码是 %d，想要 2（参数错）：%v", tc.args, code, err)
			}
		})
	}
}

// TestMigrateCreateTreatsLeadingHelpFlagAsHelp 钉住 `-h` 打头就打帮助，一个文件都不建。
//
// 这一条脱胎于上一个用例里的 `-h extra` 那一档。原先 HelpOnly 判「参数恰好一个」，
// 两个词时让路，`-h` 一路走到 goose 的写入点——snakeCase 把它洗成 `h`，建出
// `*_h.sql`（那个目录是 //go:embed sql/*.sql）。后来 validateMigrationName 挡住了
// 它，但那是**第二道**防线：命令本来就不该把这个参数当成迁移名。
//
// HelpOnly 放宽到「第一个参数是求助旗标」之后，这一档在更早的地方就被收掉，
// 报的是「你要帮助」而不是「这个名字不合法」。所以判据从「rc=2」变成
// 「rc=0 + 打出用法 + 一个文件都不建」。
//
// 它仍然必须有目录树断言：`-h` 打头却建出文件，正是这条用例要防的那件事。
func TestMigrateCreateTreatsLeadingHelpFlagAsHelp(t *testing.T) {
	for _, args := range [][]string{
		{"create", "-h"},
		{"create", "-h", "extra"},
		{"create", "--help", "extra"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			sqlDir := sandboxForMigrateCreate(t)
			before := migrationTreeSnapshot(t, sqlDir)

			out, errOut := captureStreams(t, func() {
				if err := runMigrate(args); err != nil {
					t.Errorf("%v 应当打帮助并成功（退出码 0），得到 %v", args, err)
				}
			})

			after := migrationTreeSnapshot(t, sqlDir)
			if diff := describeSnapshotDiff(before, after); diff != "目录树没有变化" {
				t.Fatalf("%v 产生了副作用：%s\n"+
					"求助旗标打头就该打用法，不该被当成迁移名——那个目录是 "+
					"//go:embed sql/*.sql，写进去的东西下次构建就进二进制。", args, diff)
			}

			// **必须断言真的打出了帮助**，不能只断言「没副作用 + 不报错」。
			// 少了这一条，把这一档改成「什么都不做、静默返回 nil」也全绿——
			// 「跳过」与「通过」在输出里长得一样（LESSONS 16）。
			if !strings.Contains(out+errOut, "larkaxctl migrate") {
				t.Errorf("%v 没有打出 migrate 的用法，输出是 %q："+
					"这一档要的是「打帮助」，不是「安静地什么都不做」",
					args, out+errOut)
			}
		})
	}
}

// TestMigrateCreateWritesTheFileItShould 是上一组的**反向锚**。
//
// 只断言「不产生文件」的那一组，在 `migrate create` 整个坏掉（RunContext 永远
// 不执行、写入点被短路）时同样是绿的——那时它证明的不是「拒绝生效」，是
// 「什么都没发生」。这一条用一条**合法**输入证明写入路径当时是通的，上一组的
// 「不动」才有意义。
//
// 它同时是 goose 行为的快照：`adduser` 落成 `*_adduser.sql`。哪天 goose 改了
// snakeCase 的拼法，或者校验被改松/改严到把这条合法名挡掉，这里会红。
func TestMigrateCreateWritesTheFileItShould(t *testing.T) {
	sqlDir := sandboxForMigrateCreate(t)
	before := migrationTreeSnapshot(t, sqlDir)

	if err := runMigrate([]string{"create", "adduser"}); err != nil {
		t.Fatalf("migrate create adduser 应当成功，得到 %v", err)
	}

	after := migrationTreeSnapshot(t, sqlDir)
	var added []string
	for name := range after {
		if _, ok := before[name]; !ok {
			added = append(added, name)
		}
	}
	if len(added) != 1 {
		t.Fatalf("migrate create adduser 应当正好多出一个文件，多了 %d 个：%v\n"+
			"这一条是反向锚：它红说明写入路径根本不通，那么"+
			"TestMigrateCreateRejectsNamesThatWouldBecomeFiles 的「没有副作用」也无意义。",
			len(added), added)
	}
	if !regexp.MustCompile(`^\d{14}_adduser\.sql$`).MatchString(added[0]) {
		t.Errorf("建出的文件名是 %q，想要 <14 位时间戳>_adduser.sql："+
			"goose 的 snakeCase 不拆驼峰，`AddUser` 也落成 adduser", added[0])
	}
}
