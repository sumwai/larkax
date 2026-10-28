package main

import (
	"slices"
	"testing"
)

// ── 动作层的帮助入口：欠账已知、可数、只许减 ────────────────────────────
//
// 背景是实测出来的：动作层 153 条子命令里，`-h` 153/153 都打得开（它们落在各条
// 命令自己的 flag 集上），但 `help` 只有 12 条能打——其余 141 条把它当成位置
// 参数（`list 不接受位置参数，得到 "help"`）。
//
// 这不是 #1266 引入的：拿 #1266 之前的二进制对拍过，`account list help` 两侧
// 都是 rc=2。它是**既有欠账**。补它要改命令层（每个 switch action 加一条
// case，或在分派层统一收），属于 P2 的活，不在这一轮。
//
// 那为什么还要在这里立一条门：**欠账可以留，但不能没人知道。** 没有这条门时，
// 「141 条打不开帮助」这件事只存在于某个人的记忆里；有了它，销掉一条就少一条，
// 谁把它改回去就当场红。
//
// 与本文件其他几道门的区别：那些门的判据是「扫到的集合 == 登记表」，两侧都
// 要精确对上。这一条是**租约**——名单里的是已知不合规项，双向检查：
//
//   - 名单里某条其实已经能用了 → 报错，把它删掉（过期租约占着位置会让人把
//     修好的缺陷当成还没修）
//   - 名单外某条不能用 → 报错，这是新欠账，要么修要么登记
//
// 名单**只许减不许增**（见 AGENTS.md 的白名单条）。

// actionHelpMissing 是「`help` 打不开帮助」的已知欠账，按命令路径登记。
//
// 2026-10-28 实测：动作层 153 条，可用 12 条，不可用 141 条。其中
// `migrate create help` 不在这里——它的问题不是「没有入口」而是「help 会被当成
// 迁移文件名写进 //go:embed 目录」，那是另一类缺陷，由
// TestMigrateCreateRejectsNamesThatWouldBecomeFiles 单独钉住并有副作用断言。
// 所以这里是 153 − 12 − 1 = **140** 条。
//
// 销租约的方式：给那条命令接上 help 入口（P2 的事），然后把这一行删掉。
var actionHelpMissing = []string{
	"account add",
	"account list",
	"account remove",
	"account update",
	"admin token create",
	"admin token revoke",
	"alias create",
	"alias delete",
	"alias list",
	"alias resolve",
	"alias update",
	"apikey create",
	"apikey get",
	"apikey list",
	"apikey update",
	"calendar import",
	"channel adapters",
	"channel add",
	"channel create",
	"channel delete",
	"channel disable",
	"channel enable",
	"channel get",
	"channel health",
	"channel list",
	"channel map add",
	"channel map list",
	"channel map remove",
	"channel probe",
	"channel restore",
	"channel sync-models",
	"channel unmap",
	"channel update",
	"channel upstream-models",
	"consent grant",
	"consent list",
	"consent revoke",
	"ledger balance",
	"ledger reconcile",
	"ledger runs",
	"ledger verify",
	"migrate down",
	"migrate down-to",
	"migrate status",
	"migrate up",
	"migrate up-to",
	"migrate version",
	"model add",
	"model alias add",
	"model alias list",
	"model alias remove",
	"model delete",
	"model get",
	"model list",
	"model onboard",
	"model trace",
	"model update",
	"partition check",
	"partition ensure",
	"plan add",
	"plan disable",
	"plan enable",
	"plan get",
	"plan grant",
	"plan key set",
	"plan key show",
	"plan list",
	"plan order set",
	"plan order show",
	"plan subscriptions",
	"plan void",
	"plugin delete",
	"plugin disable",
	"plugin enable",
	"plugin get",
	"plugin kv delete",
	"plugin kv get",
	"plugin kv set",
	"plugin list",
	"plugin publish",
	"price add",
	"price calendar import",
	"price calendar list",
	"price calendar sync",
	"price disable",
	"price enable",
	"price explain",
	"price list",
	"price peak add",
	"price peak list",
	"price peak remove",
	"price show",
	"price tier add",
	"price tier list",
	"price tier remove",
	"price update",
	"settings get",
	"settings list",
	"settings set",
	"snapshot access-log",
	"snapshot allowlist",
	"snapshot cleanup",
	"snapshot get",
	"snapshot redact",
	"upstream-cost create",
	"upstream-cost disable",
	"upstream-cost list",
	"upstream-cost update",
	"usage events",
	"usage requests",
	"usage summary",
	"user add",
	"user delete purge",
	"user delete request",
	"user delete revert",
	"user get",
	"user inspect",
	"user key create",
	"user key list",
	"user key plans set",
	"user key plans show",
	"user list",
	"user password",
	"user plan grant",
	"user plan list",
	"user plan order set",
	"user plan order show",
	"user plan void",
	"user unlock",
	"user update",
	"user usage events",
	"user usage requests",
	"user wallet adjust",
	"user wallet credit",
	"user wallet refund",
	"user wallet show",
	"wallet adjust",
	"wallet create",
	"wallet credit",
	"wallet list",
}

// actionPathsDerived 派生动作层路径：completionActionTable 的每条路径接上它那个
// 动作常量里的每个词。
//
// 与 groupPathsDerived 同源（都从 completionActionTable 出发），差别是这里要多走
// 一步——把表的值（`xxxActions` 常量）读出来。**值里恰好等于表键的那几条是子组，
// 不是动作**，必须滤掉：`price tier` 既在表键里有自己的行，又被 `price` 的动作
// 常量列出来，不滤会把它当成 `price` 的一个动作数两遍。
func actionPathsDerived(t *testing.T) []string {
	t.Helper()

	groupKeys := make(map[string]bool, len(completionActionTable))
	for path := range completionActionTable {
		groupKeys[path] = true
	}

	seen := make(map[string]bool)
	var paths []string
	for group, actions := range completionActionTable {
		for _, action := range actions {
			path := group + " " + action
			if groupKeys[path] || seen[path] {
				continue
			}
			seen[path] = true
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)

	// 少到这个数就说明派生链断了（表、常量、扫描器任一处）。动作层实测 153 条，
	// 留一半的余量：低于它时不是「都合规了」，是这个扫描器坏了。
	if len(paths) < 60 {
		t.Fatalf("只派生出 %d 条动作路径：动作表或常量扫描坏了，这条门会安静地通过",
			len(paths))
	}
	return paths
}

// actionHelpNotApplicable 是「`help` 在这条命令上有别的含义」的豁免，带理由。
//
// 它与 actionHelpMissing 是两件事，不能合并：
//
//   - actionHelpMissing 是**欠账**：该有入口而还没接，P2 补上就销一条
//   - 这里是**不适用**：`help` 在这个位置本来就不是帮助的意思，不该给它接入口
//
// 混在一起的话，销租约的人看到「还差 141 条」不会注意到其中有一条是「敲一下
// 会往源码树里写文件」的病灶，而不是「缺一份说明书」。
//
// 豁免不是「不管了」：TestMigrateCreateRejectsNamesThatWouldBecomeFiles 把这条
// 豁免背后的行为钉死了（不产生文件、rc=2），而上面那条门会在它哪天真的能打出
// 帮助时报「豁免过期」。
var actionHelpNotApplicable = map[string]string{
	"migrate create": "`create` 的第一个位置参数是迁移文件名，`help` 在这里是一个" +
		"合法的名字而不是帮助请求；它被 validateMigrationName 拒掉，因为它会被写成 " +
		"internal/migrate/sql/*_help.sql 并嵌进二进制",
}

// TestEveryActionAnswersHelpForm 钉住动作层的 `help` 入口，按租约把欠账关在
// 笼子里。
//
// 判据只查 `help` 这一档，**不查 `-h`**：`-h` 在动作层 153/153 已经通了，写进来
// 只是一条永远绿的断言。更糟的是它**会在 141 条没有自己短版的命令上也绿**——
// 那些命令的 `-h` 打的是父组（甚至根）的短版，拿它断言「这条命令有短版」是
// 断言了一件不成立的事。扫到了但扫错了对象，比扫不到更坏。
func TestEveryActionAnswersHelpForm(t *testing.T) {
	paths := actionPathsDerived(t)
	missing := make(map[string]bool, len(actionHelpMissing))
	for _, path := range actionHelpMissing {
		missing[path] = true
	}

	answered := 0
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			if reason, exempt := actionHelpNotApplicable[path]; exempt {
				// 这一档不是欠入口：`help` 在这条命令上有别的含义，所以它
				// 打不出帮助是**对的**。但它不能就这么消失在名单外——
				// 豁免要带理由，且下面那条门会把理由背后的行为钉住。
				if ok, _ := runHelpForm(t, path, "help"); ok {
					t.Errorf("%s 被登记为「help 有别的含义」，而它现在能打出帮助了："+
						"豁免过期了，把它从 actionHelpNotApplicable 里删掉", path)
				}
				_ = reason
				return
			}
			ok, _ := runHelpForm(t, path, "help")
			listed := missing[path]

			switch {
			case ok && listed:
				t.Errorf("%s 已经能用 `help` 打帮助，把它从 actionHelpMissing 里删掉："+
					"过期的租约会让下一轮把修好的缺陷当成还没修", path)
			case !ok && !listed:
				t.Errorf("%s 的 `help` 打不出帮助，而它不在 actionHelpMissing 里。"+
					"要么给它接上入口，要么登记成已知欠账——不能没人知道", path)
			case ok:
				answered++
			}
		})
	}

	// 反向锚：「一条都打不开」与「全都修好了」在输出上长得一样，两者都不该
	// 让这条门安静地通过。至少要有 1 条能用的，否则说明不是租约全销了，
	// 而是这条门测的入口整个失效了（比如 runHelpForm 坏了、dispatch 改了）。
	if answered == 0 {
		t.Fatal("一条动作都打不开 `help`：不是欠账修完了，是这条门失效了——" +
			"先确认 runHelpForm 与 dispatch 还能取到帮助")
	}
}
