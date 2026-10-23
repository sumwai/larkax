package main

import (
	"fmt"
	"strings"

	"github.com/sumwai/larkax/internal/textshape"
)

// 这一层管的是「同一个选项的另一种写法」：**旧名**与**单字母简写**。
//
// 两类都要能继续用。旧名是因为已经有脚本与部署流程在跑它们，改一次名字不该让
// 那些东西坏掉；简写是为了手工输入——`-u alice` 比 `--user alice` 少敲七下，
// 而库里那把 key 的归属人正是天天要写的那一项。
//
// 两类都不列进用法行（列进去等于把它们当正式写法），但要在命令的说明里单起
// 一段说清它们仍然可用：静默存在的别名会让人以为那才是正式写法。
//
// 下面这几个助手注册时**一律不收说明**：它们交给 fs 注册的那一行永远不会被打印——
// 帮助文案的唯一来源是各文件里的 *Usage 常量（见包注释）。要改用户看到的帮助，
// 就改那些常量。

// stringStorage 把一个 *string 接成 flag.Value。
//
// 有了它才能把**两个名字**注册到同一个变量上：flag 包没有别名机制，直接
// fs.String 两次会得到两个各自为政的格子，`--model x` 写不进 `-m` 读的那一格
// ——表现是「另一种写法静默失效」，比报错更难发现。
type stringStorage struct{ p *string }

func (s *stringStorage) String() string {
	if s.p == nil {
		return ""
	}
	return *s.p
}

func (s *stringStorage) Set(v string) error {
	*s.p = v
	return nil
}

// stringAlias 给一个**已经注册过**的字符串选项再挂一个写法。
func stringAlias(fs *flagSet, holder *string, alias string) {
	fs.aliasVar(&stringStorage{holder}, alias)
}

// checkDisplayText 拒绝一个会原样进终端的旗标值里的控制字符。
//
// 判据只有一份（internal/textshape），写入路径自己也会再判一次——这里把它
// 提前到连库之前：一个写错的命令不该先连上生产库再报错，而参数错要的是
// 退出码 2（见 usageErr）。
//
// value 应当是该命令**真的会落库的那个串**：包在写入路径上会先 trim 的字段，
// 这里也要传 trim 之后的值，否则 `--name $'\ta\t'` 会被拒，而它落库时是 `a`。
func checkDisplayText(field, value string) error {
	if err := textshape.Validate(field, value); err != nil {
		return usageErr(err)
	}
	return nil
}

// checkDisplayList 逐个校验一组旗标值（逗号分开的名单）里的控制字符。
//
// 名单项在写入路径上会被 trim（见 channels 的 normalizeModels 与 pricing 的
// normalizePatterns），所以这里也判 trim 之后的值，与落库的那个串一致。
func checkDisplayList(field string, values []string) error {
	for i, value := range values {
		if err := checkDisplayText(fmt.Sprintf("%s 里第 %d 个", field, i+1),
			strings.TrimSpace(value)); err != nil {
			return err
		}
	}
	return nil
}

// boolAlias 给一个**已经注册过**的布尔选项再挂一个写法。
//
// fs.BoolVar 会把指向的格子先赋成它给的默认值，所以这里的 false 顺带是一次
// 赋值：调用点必须在注册那一段（fs.Parse 之前）调用它，别在解析之后补挂——
// 那时它会把使用者刚写上的 true 抹回 false。
func boolAlias(fs *flagSet, holder *bool, alias string) {
	fs.aliasBool(holder, alias)
}

// intAlias 给一个**已经注册过**的整数选项再挂一个写法。
//
// 与 boolAlias 是同一条注意事项：fs.IntVar 会先给指向的格子赋一次默认值。
func intAlias(fs *flagSet, holder *int, alias string) {
	fs.aliasInt(holder, alias)
}

// legacyStringFlag 注册一个选项与它的旧名，两个名字写同一个变量，返回那个变量。
//
// name 是规范名（写进用法行的那个），legacy 是要继续能用的旧名。两个名字都给时
// **后写的赢**，与 flag 包对同一个选项写两遍的处置一致——别名机制不该在这里
// 另发明一条规则。
//
// 要再挂一个简写，就在它后面接一句 stringAlias（见 model.go 的 --model）。
func legacyStringFlag(fs *flagSet, name, legacy, value string) *string {
	holder := new(string)
	*holder = value
	fs.Var(&stringStorage{holder}, name, "")
	stringAlias(fs, holder, legacy)
	return holder
}

// userFlagNames 是「归属用户」这个选项的几种写法：规范名、上一版的旧名、
// 单字母简写。channel / price 两组命令都用它（见 flagGiven）。
var userFlagNames = []string{"user", "owner", "u"}

// modelFlagNames 是「规范模型名」这个选项的几种写法：规范名、上一版的旧名、
// 单字母简写。model / alias / price 三组命令都用它，判「这次到底给没给」时要
// 全查（见 flagGiven）——只查规范名的表现是「`-m x` 被判成没给 --model」，
// 而判它的地方是互斥检查。
var modelFlagNames = []string{"model", "canonical", "m"}

// flagGiven 判断这几个名字里有没有一个这次真的出现在命令行上。
//
// fs.Visit 记的是**实际写出来的那个名字**，所以同一个选项有几种写法（规范名、
// 旧名、简写），这里就要列几种：只查规范名的表现是「这次到底给没给」判错，
// 而判它的地方是互斥检查——判错一次，两个互斥的选项就能一起生效。
func flagGiven(given map[string]bool, names ...string) bool {
	for _, name := range names {
		if given[name] {
			return true
		}
	}
	return false
}

// credentialStdinFlagNames / credentialEnvFlagNames 是「凭证怎么传」这两个
// 选项的几种写法。channel 与 doctor 两组命令都用它们，两处都要拿它判
// 「这次到底给没给」——只查规范名的表现是 `--env FOO` 被当成没给，
// 于是那次凭证轮换被静默跳过，命令报成功、库里那一列没动。
var (
	credentialStdinFlagNames = []string{"credential-stdin", "stdin"}
	credentialEnvFlagNames   = []string{"credential-env", "env"}
)
