package channel

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// settingConsumerDirs 是「可能按 Setting 做判断」的那些目录，相对本包目录。
//
// 只列适配器与选路层，不列管理面：管理面读写的是 channels.setting 这一列
// （写进去、列出来），它不是「按这个键影响请求」的那类读者，而那句提示说的
// 正是后者。把管理面也扫进来的话，这条用例会对着一份正常的写入路径报红。
var settingConsumerDirs = []string{
	"openaiadapter",
	"anthropicadapter",
	"responsesadapter",
	"systemoneadapter",
	"httpx",
	filepath.Join("..", "route"),
}

// TestSettingConsumersAgreeWithDeclaration 钉住 SettingReadByAdapters 是不说谎的。
//
// 那一句「今天没有任何适配器读取 setting」会被管理面打给使用者，而它是
// 一份手写声明——没有人会在给适配器加一个键之后还记得回来改它。声明错了
// 不会让任何东西报错，只会让命令继续说着已经不成立的话。
//
// 所以反过来查：只要适配器或选路层的**非注释**代码里出现了 Setting，
// 声明就必须是 true。注释不算读——已经有一处注释在说这条归属，而它
// 恰恰是「还没有读者」这条说明的依据。
//
// 扫到 0 个文件时判失败而不是通过：目录改名之后这条用例会安静地什么都
// 不检查，而它的输出与「干净」长得一样（LESSONS.md 第 16 条）。
func TestSettingConsumersAgreeWithDeclaration(t *testing.T) {
	scanned := 0
	var consumers []string

	for _, dir := range settingConsumerDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读目录 %s 失败: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读 %s 失败: %v", path, err)
			}
			scanned++
			for i, line := range strings.Split(string(src), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				if strings.Contains(line, "Setting") {
					consumers = append(consumers, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
				}
			}
		}
	}

	if scanned == 0 {
		t.Fatalf("一个适配器/选路层的 .go 文件都没扫到，这条判据等于没跑")
	}
	// 声明为 true 时没有可自动验证的一侧：读者在适配器里的哪一行、读的是
	// 哪个键，文本扫描判不出来（它连注释都要跳过，否则上面那句解释归属的
	// 注释会被当成读者）。那时这条用例只剩一句「目录还扫得到文件」。
	if SettingReadByAdapters {
		return
	}
	if len(consumers) > 0 {
		t.Fatalf("SettingReadByAdapters 还是 false，但下面这些地方已经在读 Setting 了；"+
			"先把它改成 true，否则管理面会继续打着「没有任何适配器读取」：\n  %s",
			strings.Join(consumers, "\n  "))
	}
}

// TestSettingUnreadNoteTracksDeclaration 钉住那一句话跟着声明走。
//
// 它是管理面两处提示的唯一文案来源，而它只有两种形态：没读者时给一句
// 带下一步的话，有读者时给空串（调用方据此不打）。写成与声明无关的常量
// 也能编译、也能通过「有没有打出来」那条用例——那正是要防的：
// 声明改了而文案不变，提示会一直打下去。
func TestSettingUnreadNoteTracksDeclaration(t *testing.T) {
	note := SettingUnreadNote()
	if SettingReadByAdapters {
		if note != "" {
			t.Fatalf("声明为已接线时不该再有提示，得到 %q", note)
		}
		return
	}
	if note == "" {
		t.Fatal("还没有读者时提示不能是空串：否则管理面什么都不会说")
	}
	// 只说「不支持」不够，要给出下一步：谁解释、什么时候会变。
	for _, want := range []string{"适配器", "不会影响请求路径", "生效"} {
		if !strings.Contains(note, want) {
			t.Errorf("提示里没有 %q：%s", want, note)
		}
	}
}
