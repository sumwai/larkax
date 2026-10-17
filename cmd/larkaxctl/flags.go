package main

import "flag"

// 这一层管的是「同一个选项的旧名字」。
//
// 统一命名（见 issue #477 的规范）之后，旧名字要**继续能用**，不能删：已经有
// 脚本与部署流程在跑它们，改一次名字不该让那些东西坏掉。旧名字因此以别名的
// 形态留下来——不列进用法行，但要在命令的说明里提一句，因为静默存在的别名会
// 让人以为那才是正式写法。

// stringStorage 把一个 *string 接成 flag.Value。
//
// 有了它才能把**两个名字**注册到同一个变量上：flag 包没有别名机制，直接
// fs.String 两次会得到两个各自为政的格子，`--canonical x` 写不进 `--model`
// 读的那一格——表现是「旧写法静默失效」，比报错更难发现。
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

// legacyStringFlag 注册一个选项与它的旧名，两个名字写同一个变量，返回那个变量。
//
// name 是规范名（写进用法行的那个），legacy 是要继续能用的旧名。两个名字都给时
// **后写的赢**，与 flag 包对同一个选项写两遍的处置一致——别名机制不该在这里
// 另发明一条规则。
//
// 注意 fs.Visit 记的是**实际写出来的那个名字**，所以调用点判断「这次到底给没给」
// 要用 legacyGiven，不能只看规范名。
func legacyStringFlag(fs *flag.FlagSet, name, legacy, value, usage string) *string {
	holder := new(string)
	*holder = value
	fs.Var(&stringStorage{holder}, name, usage)
	fs.Var(&stringStorage{holder}, legacy, usage+"（旧名，仍可用；新写的命令请用 --"+name+"）")
	return holder
}

// legacyGiven 判断一个选项或它的旧名这次有没有真的出现在命令行上。
func legacyGiven(given map[string]bool, name, legacy string) bool {
	return given[name] || given[legacy]
}
