package main

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/catalog"
)

// ── 简写与旧名：每一个都要真的落在**同一个格子**上 ────────────────────────
//
// 这一组钉的是 `larkaxctl` 的两种「另一个写法」：
//
//	简写   -u -m -o -v -q -V，以及 --ctx --in --out --im --om 这类短旧名
//	旧名   --canonical --owner --rpm --quota --rate-limit-rpm …
//
// 它们比动词的别名更容易静默失效：flag 包**没有**别名机制，于是最省事的写法
// （把同一个选项注册两次）会得到两个各自为政的格子——`-m x` 写进 A，
// 而命令读的是 B，读出来是空串。它不报错，表现是「另一种写法像没写一样」。
//
// # 为什么比的是解析结果而不是终端输出
//
// 一条命令跑一次要连库，而「两种写法解析出**同一个请求**」正是别名唯一要保证
// 的事，它在纯解析那一层就看得见。所以下面主体是一张表：左边规范写法、
// 右边另一种写法，断言两者解析出同一个请求结构（逐字段 DeepEqual）。
//
// 请求结构里有几项是**指针**（`--ctx 0` 与「没提 --ctx」是两件事，见
// modelCapabilityFlag），DeepEqual 连指针指向的值一起比——这正是那些
// 「给了没生效」的静默失效会在这一层露出来的地方。
//
// 终端那一层另有两条用例（真跑命令，用简写写、用规范名读回来）。

// any1 / any2 把返回请求结构体的解析函数收成同一种形状，好放进一张表。
//
// 用泛型适配而不是改那些函数的签名：它们还有别的调用方，为了用例改返回形状是
// 本末倒置。
func any1[T any](f func([]string) (T, error)) func([]string) ([]any, error) {
	return func(args []string) ([]any, error) {
		v, err := f(args)
		return []any{v}, err
	}
}

func any2[T any, U any](f func([]string) (T, U, error)) func([]string) ([]any, error) {
	return func(args []string) ([]any, error) {
		first, second, err := f(args)
		return []any{first, second}, err
	}
}

// parseAliasCase 是「同一件事，两种写法」的一条。
type parseAliasCase struct {
	name      string
	parse     func([]string) ([]any, error)
	canonical []string
	alias     []string
	// arm 在**每一次**解析之前跑一遍。要它的是读标准输入那条路径：
	// readCredential 会把那个 reader 读干，第二次读到的就是 EOF——
	// 于是「别名读不到凭证」这条假失败会盖住真正要比的东西。
	arm func(*testing.T)
}

// assertSameParse 断言两种写法解析出同一个请求。
func assertSameParse(t *testing.T, tc parseAliasCase) {
	t.Helper()

	parse := func(args []string) []any {
		if tc.arm != nil {
			tc.arm(t)
		}
		got, err := tc.parse(args)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", args, err)
		}
		return got
	}

	want := parse(tc.canonical)
	got := parse(tc.alias)
	// 两个都解析成零值的话，这条用例比的是「两处都没解析出东西」——它照样绿，
	// 却什么都没钉住。参数的拼写随实现改过之后就可能这样，所以这里要拦一下。
	if len(want) == 0 || reflect.ValueOf(want[0]).IsZero() {
		t.Fatal("规范写法解析出来的请求是零值：这条用例没在比任何东西")
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("两种写法解析出的请求不同：\n规范写法 %#v\n另一种  %#v", want, got)
	}
}

// updateModelReq 只取 parseModelUpdate 的第一个返回值。
//
// 第二个是能力标志的原始清单，里面带 func 字段，而 reflect.DeepEqual 对两个
// 非 nil 的切片一律判否（函数只有都为 nil 才相等）——带上它这条用例永远红。
// 「--tools 给了没生效」那一类另有专门用例钉着（见 model_test.go），
// 这里要比的是模型名与那几个上限。
func updateModelReq(args []string) (catalog.UpdateModelRequest, error) {
	req, _, err := parseModelUpdate(args)
	return req, err
}

// TestShortAndLegacyFlagAliasesParseIdentically 逐个选项钉住别名。
func TestShortAndLegacyFlagAliasesParseIdentically(t *testing.T) {
	// --env 那条要从环境变量读凭证（readCredential 不是只解析参数）；
	// --stdin 那条要从标准输入读，它自己用 arm 每次架好。
	t.Setenv("LARKAX_TEST_CREDENTIAL", "test-key-placeholder")

	cases := []parseAliasCase{
		// ── model：-m / --canonical / --ctx / --in / --out / --im / --om ──
		{
			name:      "model create 的 -m 同 --model",
			parse:     any1(parseModelCreate),
			canonical: []string{"--model", "claude-sonnet-5-5", "--display", "展示名"},
			alias:     []string{"-m", "claude-sonnet-5-5", "--display", "展示名"},
		},
		{
			name:      "model create 的 --canonical 同 --model",
			parse:     any1(parseModelCreate),
			canonical: []string{"--model", "claude-sonnet-5-5", "--display", "展示名"},
			alias:     []string{"--canonical", "claude-sonnet-5-5", "--display", "展示名"},
		},
		{
			name:  "model create 的 --ctx / --in / --out 同三个长名",
			parse: any1(parseModelCreate),
			canonical: []string{"--model", "m1", "--display", "d",
				"--context-limit", "200000", "--input-limit", "180000", "--output-limit", "64000"},
			alias: []string{"-m", "m1", "--display", "d",
				"--ctx", "200000", "--in", "180000", "--out", "64000"},
		},
		{
			name:  "model create 的 --im / --om 同两个长名",
			parse: any1(parseModelCreate),
			canonical: []string{"--model", "m1", "--display", "d",
				"--input-modalities", "text,image", "--output-modalities", "text"},
			alias: []string{"-m", "m1", "--display", "d",
				"--im", "text,image", "--om", "text"},
		},
		{
			name:  "model create 的 --modalities-input 是上一版的旧名",
			parse: any1(parseModelCreate),
			canonical: []string{"--model", "m1", "--display", "d",
				"--input-modalities", "text,image", "--output-modalities", "text"},
			alias: []string{"--model", "m1", "--display", "d",
				"--modalities-input", "text,image", "--modalities-output", "text"},
		},
		{
			// 这一条比的是**指针**：`--ctx 0` 是「清空这一栏」，而它必须被
			// 当成「给了」。别名没共享同一个格子时，`-m x --ctx 0` 与
			// `-m x` 会解析出同一个请求——清空静默失效。
			name:      "model update 的 --ctx 0 与 --context-limit 0 一样是「给了」",
			parse:     any1(updateModelReq),
			canonical: []string{"--model", "m1", "--context-limit", "0"},
			alias:     []string{"-m", "m1", "--ctx", "0"},
		},
		{
			name:      "model update 的 --im 给空串一样是「给了」",
			parse:     any1(updateModelReq),
			canonical: []string{"--model", "m1", "--input-modalities", ""},
			alias:     []string{"-m", "m1", "--im", ""},
		},

		// ── price：-u / --owner / --from / --to / --no-expire ──────────────
		{
			name:      "price create 的 -u 与 --owner 同 --user",
			parse:     any2(parsePriceCreate),
			canonical: []string{"--scope", "merchant", "--name", "n", "--expr", "1", "--user", "7", "--by", "9"},
			alias:     []string{"--scope", "merchant", "--name", "n", "--expr", "1", "-u", "7", "--by", "9"},
		},
		{
			name:      "price create 的 --owner 同 --user",
			parse:     any2(parsePriceCreate),
			canonical: []string{"--scope", "merchant", "--name", "n", "--expr", "1", "--user", "7", "--by", "9"},
			alias:     []string{"--scope", "merchant", "--name", "n", "--expr", "1", "--owner", "7", "--by", "9"},
		},
		{
			name:  "price create 的 --from / --to 同两个长名",
			parse: any2(parsePriceCreate),
			canonical: []string{"--scope", "platform", "--name", "n", "--expr", "1", "--by", "9",
				"--effective-from", "2026-01-01T00:00:00Z", "--effective-to", "2026-02-01T00:00:00Z"},
			alias: []string{"--scope", "platform", "--name", "n", "--expr", "1", "--by", "9",
				"--from", "2026-01-01T00:00:00Z", "--to", "2026-02-01T00:00:00Z"},
		},
		{
			name:      "price update 的 -u 同 --user",
			parse:     any2(parsePriceUpdate),
			canonical: []string{"--id", "5", "--user", "7", "--by", "9"},
			alias:     []string{"--id", "5", "-u", "7", "--by", "9"},
		},
		{
			name:      "price update 的 --from / --to 同两个长名",
			parse:     any2(parsePriceUpdate),
			canonical: []string{"--id", "5", "--by", "9", "--effective-from", "2026-01-01T00:00:00Z", "--effective-to", "2026-02-01T00:00:00Z"},
			alias:     []string{"--id", "5", "--by", "9", "--from", "2026-01-01T00:00:00Z", "--to", "2026-02-01T00:00:00Z"},
		},
		{
			name:      "price update 的 --no-expire 同 --clear-effective-to",
			parse:     any2(parsePriceUpdate),
			canonical: []string{"--id", "5", "--by", "9", "--clear-effective-to"},
			alias:     []string{"--id", "5", "--by", "9", "--no-expire"},
		},

		// ── alias：-m / --canonical ────────────────────────────────────────
		{
			name:      "alias create 的 -m 同 --model",
			parse:     any1(parseAliasCreate),
			canonical: []string{"--model", "m1", "--upstream", "upstream-1"},
			alias:     []string{"-m", "m1", "--upstream", "upstream-1"},
		},
		{
			name:      "alias create 的 --canonical 同 --model",
			parse:     any1(parseAliasCreate),
			canonical: []string{"--model", "m1", "--upstream", "upstream-1"},
			alias:     []string{"--canonical", "m1", "--upstream", "upstream-1"},
		},

		// ── channel：-u / --owner / --env / --stdin ────────────────────────
		{
			name:  "channel create 的 -u 与 --owner 同 --user",
			parse: any2(parseChannelCreate),
			arm: func(t *testing.T) {
				withStdin(t, "test-key-placeholder\n")
			},
			canonical: []string{"--name", "n", "--type", "openai", "--base-url", "http://127.0.0.1:1",
				"--models", "m1", "--credential-stdin", "--user", "7"},
			alias: []string{"--name", "n", "--type", "openai", "--base-url", "http://127.0.0.1:1",
				"--models", "m1", "--stdin", "-u", "7"},
		},
		{
			name:  "channel create 的 --env 同 --credential-env",
			parse: any2(parseChannelCreate),
			canonical: []string{"--name", "n", "--type", "openai", "--base-url", "http://127.0.0.1:1",
				"--models", "m1", "--credential-env", "LARKAX_TEST_CREDENTIAL"},
			alias: []string{"--name", "n", "--type", "openai", "--base-url", "http://127.0.0.1:1",
				"--models", "m1", "--env", "LARKAX_TEST_CREDENTIAL"},
		},

		// ── doctor：-m / --env（只在 --live 下有意义）───────────────────────
		{
			name: "doctor 的 -m 同 --model",
			parse: any1(func(args []string) (doctorOptions, error) {
				opts, err := parseDoctorOptions(args)
				return opts, err
			}),
			canonical: []string{"--live", "--model", "m1", "--credential-env", "LARKAX_TEST_CREDENTIAL"},
			alias:     []string{"--live", "-m", "m1", "--env", "LARKAX_TEST_CREDENTIAL"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertSameParse(t, tc) })
	}
}

// TestAliasConflictsStillFireUnderTheAlias 别名不是「另一条宽松的路」：
// 用别名写出来的冲突照样要报同一句话。
//
// 判据都是 fs.Visit 记下的**实际写出来的名字**（见 flagGiven），所以一个漏登记
// 的别名在这里的表现是「冲突没报、命令按后者安静地跑」——静默失效里最坏的一类，
// 它一次改了账本上的两个方向。
//
// price update 那几条都带着 --by：它是必填项，而这几条要比的是别名与冲突那一层，
// 不是错误之间的先后。少了它，命令会提前死在缺少 --by 上，这几条要证的事就没人看着了
// ——报的还是「错误信息里该提到 X」，红的样子和真出问题一模一样。
func TestAliasConflictsStillFireUnderTheAlias(t *testing.T) {
	t.Setenv("LARKAX_TEST_CREDENTIAL", "test-key-placeholder")

	cases := []struct {
		name  string
		run   func([]string) error
		args  []string
		wants []string
	}{
		{
			name:  "price update：--no-expire 与 --to 不能一起给",
			run:   runPrice,
			args:  []string{"update", "--id", "5", "--by", "ops", "--no-expire", "--to", "2026-02-01T00:00:00Z"},
			wants: []string{"--clear-effective-to", "--effective-to"},
		},
		{
			name:  "price update：--no-expire 与 --effective-to 也不能一起给",
			run:   runPrice,
			args:  []string{"update", "--id", "5", "--by", "ops", "--clear-effective-to", "--effective-to", "2026-02-01T00:00:00Z"},
			wants: []string{"--clear-effective-to", "--effective-to"},
		},
		{
			name:  "alias list：-m 与 --channel 不能一起给",
			run:   runAlias,
			args:  []string{"list", "--channel", "3", "-m", "m1"},
			wants: []string{"--channel 与 --model 只能给一个"},
		},
		{
			name:  "alias list：--canonical 与 --channel 也不能一起给",
			run:   runAlias,
			args:  []string{"list", "--channel", "3", "--canonical", "m1"},
			wants: []string{"--channel 与 --model 只能给一个"},
		},
		{
			name:  "price update：-u 给空串要拒（多半是变量没展开）",
			run:   runPrice,
			args:  []string{"update", "--id", "5", "--by", "ops", "-u", ""},
			wants: []string{"--user 不能是空串"},
		},
		{
			// 这两条钉的是「哪些选项只在 --live 下有意义」那道检查：
			// 它按 fs.Visit 记的名字判，所以 -m / --env 必须也在它查的名单里。
			// 漏了的表现是「我给了 --model，检查照样说一切正常」。
			name:  "doctor：不带 --live 时 -m 必须报错",
			run:   runDoctor,
			args:  []string{"-m", "m1"},
			wants: []string{"--model", "--live"},
		},
		{
			name:  "doctor：不带 --live 时 --env 必须报错",
			run:   runDoctor,
			args:  []string{"--env", "LARKAX_TEST_CREDENTIAL"},
			wants: []string{"--credential-env", "--live"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runOnce(t, tc.run, tc.args)
			if r.err == nil {
				t.Fatalf("应当报错，实际通过（stdout=%q stderr=%q）", r.stdout, r.stderr)
			}
			for _, want := range tc.wants {
				if !strings.Contains(r.err.Error(), want) {
					t.Errorf("错误信息里该提到 %q：%v", want, r.err)
				}
			}
		})
	}
}

// TestLongOnlyFlagsRejectShortSpellings 只有长写法的那些参数，写短了必须失败。
//
// 它们是**故意**不给简写的：`--channel` 与 `--rate-limit-rpm` 这种写短了容易
// 看错也容易按错，要的就是完整写出来。这条约束只在「短写法真的不可用」时成立，
// 而它没有任何编译期保障——所以逐个点一条用例。失败的样子是 flag 包那句
// “flag provided but not defined”，它比静默地把 `-c 1` 当成别的东西好得多。
func TestLongOnlyFlagsRejectShortSpellings(t *testing.T) {
	t.Setenv("LARKAX_TEST_CREDENTIAL", "test-key-placeholder")

	cases := []struct {
		name string
		run  func([]string) error
		args []string
		flag string
	}{
		{
			name: "--channel 没有 -c（price create）",
			run:  runPrice,
			args: []string{"create", "--scope", "channel", "--name", "n", "--expr", "1", "-c", "1"},
			flag: "-c",
		},
		{
			name: "--channel 没有 -c（alias create）",
			run:  runAlias,
			args: []string{"create", "--model", "m1", "--upstream", "u1", "-c", "1"},
			flag: "-c",
		},
		{
			name: "--effective-to 没有 -t",
			run:  runPrice,
			args: []string{"create", "--scope", "platform", "--name", "n", "--expr", "1", "-t", "2026-02-01T00:00:00Z"},
			flag: "-t",
		},
		{
			name: "--ref 没有 -r",
			run:  runWallet,
			args: []string{"credit", "--user", "1", "--amount", "1", "--gateway", "manual", "-r", "order-1"},
			flag: "-r",
		},
		{
			name: "--credential-env 没有 -e",
			run:  runChannel,
			args: []string{"create", "--name", "n", "--type", "openai", "--base-url", "http://127.0.0.1:1",
				"--models", "m1", "-e", "LARKAX_TEST_CREDENTIAL"},
			flag: "-e",
		},
		{
			name: "--rpm（旧名 --rate-limit-rpm）没有 -r",
			run:  runAPIKey,
			args: []string{"create", "--user", "1", "-r", "60"},
			flag: "-r",
		},
		{
			// --idempotency-key 是这一项上一版的正式写法，--ref 是现在的规范名：
			// 两个都只有长写法。
			name: "--idempotency-key / --ref 都没有 -i",
			run:  runWallet,
			args: []string{"credit", "--user", "1", "--amount", "1", "-i", "order-1"},
			flag: "-i",
		},
		{
			// price create 上有 --models 而没有 --model，所以 -m 在这里是
			// 「没定义」，不是 --models 的简写。这两个名字挨得近，
			// 正是最容易顺手写成 -m 的地方。
			name: "-m 是 --model 的简写，不是 --models 的",
			run:  runPrice,
			args: []string{"create", "--scope", "platform", "--name", "n", "--expr", "1", "-m", "m1"},
			flag: "-m",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runOnce(t, tc.run, tc.args)
			if r.err == nil {
				t.Fatalf("%s 应当报错，实际通过（stdout=%q stderr=%q）", tc.flag, r.stdout, r.stderr)
			}
			if !strings.Contains(r.err.Error(), tc.flag) {
				t.Errorf("错误信息里该提到 %s：%v", tc.flag, r.err)
			}
		})
	}
}

// TestShortOutputFormatFlagIsAccepted 钉住 -o：它由最外层摘，写在子命令前或后都认。
//
// 与 --format 走同一条路（format_test.go 有它自己的一组），这里只确认**短写法**
// 也进了那条路——把 -o 加进白名单时最容易漏的正是格式那一处。
func TestShortOutputFormatFlagIsAccepted(t *testing.T) {
	for _, args := range [][]string{
		{"-o", "json", "version"},
		{"version", "-o", "json"},
	} {
		stdout, _, err := runWithArgs(t, args...)
		if err != nil {
			t.Fatalf("%v 跑失败: %v", args, err)
		}
		assertOnlyJSON(t, stdout)
	}
}

// TestShortFlagsDriveRealCommands 用简写把东西真的建出来，再用规范名读回来。
//
// 上面那张表比的是解析结果，这一条过一遍真实的写入与读取路径：`-u` 建的 key
// 要落在对的那个用户名下，`-m` 建的模型要能被 `--model` 查到。别名与规范名
// 分家的表现有两层——解析层（那张表管）与「同一个值有没有一路带到 SQL 里」
// （这条管），两层都钉住了才算完。
func TestShortFlagsDriveRealCommands(t *testing.T) {
	testDB(t)
	setupEnv(t)

	owner := createTestUser(t)
	userRef := strconv.FormatInt(owner, 10)

	t.Run("-u 建的 key 落在对的那个用户下", func(t *testing.T) {
		if r := runOnce(t, runAPIKey, []string{
			"create", "-u", userRef, "--rate-limit-rpm", "60", "--quota-limit", "12.5",
		}); r.err != nil {
			t.Fatalf("apikey create -u 失败: %v", r.err)
		}

		// 用规范名读回来。顺带把两个长旧名（--rate-limit-rpm / --quota-limit）
		// 也验一遍：它们走的是同一条写入路径，读出来的数必须与给的一致。
		stdout, _, err := runWithArgs(t, "apikey", "list", "--user", userRef, "-o", "json")
		if err != nil {
			t.Fatalf("apikey list 失败: %v", err)
		}
		var doc struct {
			Keys []struct {
				RateLimitRPM *int `json:"rate_limit_rpm"`
				QuotaLimit   any  `json:"quota_limit"`
			} `json:"keys"`
		}
		if jsonErr := json.Unmarshal([]byte(stdout), &doc); jsonErr != nil {
			t.Fatalf("解不回来: %v\n%s", jsonErr, stdout)
		}
		if len(doc.Keys) != 1 {
			t.Fatalf("这个用户下应当只有刚建的那一把 key，得到 %d 把", len(doc.Keys))
		}
		if doc.Keys[0].RateLimitRPM == nil || *doc.Keys[0].RateLimitRPM != 60 {
			t.Errorf("每分钟额度 = %v，想要 60", doc.Keys[0].RateLimitRPM)
		}
		if got := doc.Keys[0].QuotaLimit; got == nil || !strings.Contains(toText(got), "12.5") {
			t.Errorf("额度上限 = %v，想要 12.5", got)
		}
	})

	t.Run("-m 建的模型能被 --model 查到", func(t *testing.T) {
		name := uniqueName("model")
		if r := runOnce(t, runModel, []string{
			"create", "-m", name, "--display", "展示名", "--ctx", "200000",
		}); r.err != nil {
			t.Fatalf("model create -m 失败: %v", r.err)
		}

		got := runOnce(t, runModel, []string{"get", "--model", name})
		if got.err != nil {
			t.Fatalf("model get --model 失败: %v", got.err)
		}
		if !strings.Contains(got.stdout, name) {
			t.Errorf("--model 查回来的那一条里该有这个规范名：%q", got.stdout)
		}
		// --ctx 是简写，读回来的是它写进去的那个数：这一条钉的是
		// 「简写与它的长名共用同一个格子」在**写入**那一侧也成立。
		if !strings.Contains(got.stdout, "200000") {
			t.Errorf("--ctx 写进去的上限该读得出来：%q", got.stdout)
		}

		// 用旧写法读同一行：--canonical 与 -m 落到同一条记录上。
		legacy := runOnce(t, runModel, []string{"get", "--canonical", name})
		if legacy.err != nil {
			t.Fatalf("model get --canonical 失败: %v", legacy.err)
		}
		if legacy.stdout != got.stdout {
			t.Errorf("--model 与 --canonical 查到的不是同一行：\n--model %q\n--canonical %q",
				got.stdout, legacy.stdout)
		}
	})
}

// toText 把 JSON 里解出来的值（数或字符串）打成一个可比较的字符串。
func toText(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

// TestChannelUpdateRotatesCredentialUnderTheAlias 钉住 --env / --stdin 在
// **update** 上也真的换掉凭证。
//
// # 为什么这条要连库、要看密文
//
// parseChannelUpdate 判「这次到底改什么」时读的是 fs.Visit 那份清单，而
// Visit 记的是**实际写出来的名字**：写 `--env FOO` 时它记的是 "env"。
// 只查规范名（"credential-env"）的表现是这一项被判成「没给」——命令报成功、
// 退出码 0，而**库里那一列还是旧凭证**。看输出看不出任何异常，所以断言只能
// 落在库上：把密文解开，看它是不是这次给的那把。
func TestChannelUpdateRotatesCredentialUnderTheAlias(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	id := createTestChannel(t, uniqueName("channel"), uniqueName("model"))
	idArg := strconv.FormatInt(id, 10)

	// 解开当前密文，看它是不是这把。
	credentialInDB := func() string {
		t.Helper()
		plaintext, err := testSealer(t).Open(channelEnvelope(t, db, id))
		if err != nil {
			t.Fatalf("解不开渠道 %d 的凭证: %v", id, err)
		}
		return string(plaintext)
	}

	if got := credentialInDB(); got != testSecret {
		t.Fatalf("建出来的渠道凭证 = %q，期望 %q", got, testSecret)
	}

	t.Run("--env", func(t *testing.T) {
		const rotated = "rotated-by-env-placeholder"
		t.Setenv("LARKAX_TEST_ROTATED_CREDENTIAL", rotated)

		if r := runOnce(t, runChannel, []string{
			"update", "--id", idArg, "--env", "LARKAX_TEST_ROTATED_CREDENTIAL",
		}); r.err != nil {
			t.Fatalf("channel update --env 失败: %v", r.err)
		}
		if got := credentialInDB(); got != rotated {
			t.Errorf("--env 那句话没换掉凭证：库里还是 %q", got)
		}
	})

	t.Run("--stdin", func(t *testing.T) {
		const rotated = "rotated-by-stdin-placeholder"
		withStdin(t, rotated+"\n")

		if r := runOnce(t, runChannel, []string{
			"update", "--id", idArg, "--stdin",
		}); r.err != nil {
			t.Fatalf("channel update --stdin 失败: %v", r.err)
		}
		if got := credentialInDB(); got != rotated {
			t.Errorf("--stdin 那句话没换掉凭证：库里还是 %q", got)
		}
	})

	// 反面对照：只给 --id 时「没有要改的东西」仍然报错——别名那条路走通了，
	// 不等于这一项变成了「不给也算给」。
	t.Run("什么都没给仍然报错", func(t *testing.T) {
		r := runOnce(t, runChannel, []string{"update", "--id", idArg})
		if r.err == nil {
			t.Fatal("什么都没给时应当报错")
		}
		if !strings.Contains(r.err.Error(), "至少要改一项") {
			t.Errorf("错误信息应当是「至少要改一项」：%v", r.err)
		}
	})

	// 另一条路一起改的时候才是**真正静默**的那一种：别的字段给了，于是
	// 「至少要改一项」那道检查放行，而凭证那一项在被判成「没给」之后既不进
	// 请求、也不报任何错——命令输出看起来与成功轮换逐字一样。
	t.Run("与别的字段一起给时也不能被静默跳过", func(t *testing.T) {
		const rotated = "rotated-alongside-priority-placeholder"
		t.Setenv("LARKAX_TEST_ROTATED_CREDENTIAL", rotated)

		if r := runOnce(t, runChannel, []string{
			"update", "--id", idArg, "--priority", "7", "--env", "LARKAX_TEST_ROTATED_CREDENTIAL",
		}); r.err != nil {
			t.Fatalf("channel update --priority --env 失败: %v", r.err)
		}
		if got := credentialInDB(); got != rotated {
			t.Errorf("与 --priority 一起给时凭证被静默跳过了：库里还是 %q", got)
		}
	})
}
