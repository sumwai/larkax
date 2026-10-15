package tokens

// 本文件是启发式估算：没有词表时怎么猜一段文本有多少 token。
//
// # 为什么不能用「字符数 ÷ 4」
//
// 「英文 4 字符 ≈ 1 token」是流传最广的经验值，但它的分母只对英文散文成立：
//
//	英文散文        0.19 token/字符
//	JSON / 代码     0.26 ~ 0.29
//	纯数字          0.46 ~ 0.59
//	中文            0.73 ~ 0.82
//	长密钥/摘要串    0.65 ~ 0.67
//	emoji           1.1 ~ 2.1
//
// 最大与最小差了十倍。用一个常数去除字符数，等于对中文低估三倍、
// 对英文高估两倍——而中文恰恰是这套服务最需要正确处理的那一类输入。
//
// # 实际做法
//
// 1. 按 Unicode 类别把文本切成若干「段」（run）：连续的同类字符为一段。
//    类共七种：拉丁字母、数字、中日韩、其他字母、空白、标点、符号。
// 2. 每段贡献 max(权重[类] × 段长, 1)。
//    下限 1 是必须的：BPE 对任何**非空**的一段都至少产生一个 token，
//    哪怕这段「看起来」很常见的词在词表里根本不作为一个整体存在。
//    没有下限的话，`\n\n` 这类短空白段会被算成 0.2 个 token，
//    而实测是 1 个。
// 3. 对超长且不中断的字母数字串施加额外下限（见 runFloor）。
//    这类串是密钥、摘要、base64 的形态，BPE 压不动它们，实测接近逐字符。
// 4. 乘以该族的偏置 bias。
//
// 每一步的常数都来自实测标定，见 COUNTER.md。
//
// # 为什么是七类而不是「CJK / 非 CJK」
//
// 两分法会把「数字」和「中文」混在一起，而它们的密度差 40%；
// 中文输入里数字（版本号、金额、日期）出现得很频繁，混在一起会让
// 这类输入的估算系统性偏低。七类是能明确区分、又不至于让标定样本
// 稀释到某几类没有足够观测的最小划分。

import (
	"unicode"
	"unicode/utf8"
)

// charClass 是字符类。
type charClass uint8

const (
	classLatin charClass = iota
	classDigit
	classCJK
	classOtherLetter // 西里尔、阿拉伯、希腊、天城文……以及组合记号
	classSpace
	classPunct
	classSymbol // emoji、箭头、数学符号等
	numClasses
)

// classOf 判定一个字符属于哪一类。
//
// 用 unicode 包的类别查询而不是正则：正则在长文本上要跑一遍完整扫描，
// 而这里本来就是逐个字符走，查表更快也更好读。
//
// ASCII 走一条快路径：真实流量里 ASCII 占绝大多数，而 unicode.Is*
// 系列对每个字符都要查区间表。
func classOf(r rune) charClass {
	if r < utf8.RuneSelf {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z':
			return classLatin
		case '0' <= r && r <= '9':
			return classDigit
		case r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f':
			return classSpace
		case unicode.IsPunct(r):
			return classPunct
		default:
			return classSymbol
		}
	}

	switch {
	// 中日韩统一表意文字 + 假名 + 谚文。它们的共同点是每个字的信息量大、
	// 在词表里的覆盖率低，于是 token 密度显著高于其他文字。分开列是为了
	// 一个具体的原因：韩文的音节块在现代词表里几乎覆盖不到（每个音节
	// 都要拆），密度介于汉字与假名之间，合并进 classOtherLetter 会拉低
	// 整类的权重，把韩文输入估算得偏低。
	case unicode.Is(unicode.Han, r),
		unicode.Is(unicode.Hiragana, r),
		unicode.Is(unicode.Katakana, r),
		unicode.Is(unicode.Hangul, r):
		return classCJK
	case unicode.IsLetter(r), unicode.IsMark(r):
		return classOtherLetter
	case unicode.Is(unicode.Digit, r):
		return classDigit
	case unicode.Is(unicode.Space, r):
		return classSpace
	case unicode.Is(unicode.Punct, r):
		return classPunct
	default:
		return classSymbol
	}
}

// weightTable 是一族模型的估算权重。
type weightTable struct {
	// weight 是每一类字符的 token/字符 密度。
	weight [numClasses]float64
	// bias 是整体偏置，用来把中位数附近的分布整体推高。
	// 标定时取的偏置使训练语料上约 90% 的样本「估算值 ≥ 真实值」，
	// 因为估少了等于少收钱（理由见包注释）。
	bias float64
	// runFloorLen / runFloorDensity：长度达到 runFloorLen 的连续
	// 字母数字串，每字符至少按 runFloorDensity 个 token 计。
	runFloorLen     int
	runFloorDensity float64
}

// 具体的权重表。每个数字都是标定产物，来源见 COUNTER.md。
var (
	// tableOpenAI 用 o200k_base 的真实分词结果标定。
	tableOpenAI = weightTable{
		weight: [numClasses]float64{
			classLatin:       0.2299,
			classDigit:       0.6101,
			classCJK:         0.7322,
			classOtherLetter: 0.3452,
			classSpace:       0.1817,
			classPunct:       0.3516,
			classSymbol:      1.3800,
		},
		bias:            0.974,
		runFloorLen:     24,
		runFloorDensity: 0.65,
	}

	// tableUnknown 覆盖「不知道是哪一家」的情形。
	//
	// 逐类取 o200k 与 cl100k 两张标定表的较大值：
	// 中文与「其他字母」在 cl100k 下密度高得多（同文本 token 数可达 o200k
	// 的 2 倍），取较大值才能让未知模型的估算在两套词表上都不偏低。
	tableUnknown = weightTable{
		weight: [numClasses]float64{
			classLatin:       0.2299,
			classDigit:       0.6136,
			classCJK:         1.1196,
			classOtherLetter: 0.8624,
			classSpace:       0.1926,
			classPunct:       0.3516,
			classSymbol:      1.8033,
		},
		bias:            0.967,
		runFloorLen:     24,
		runFloorDensity: 0.65,
	}

	// tableClaude 是**推理出来的**，不是标定的。
	//
	// 依据：Anthropic 不公开词表，且 Claude 4.7+ 换了新 tokenizer，
	// 同文本 token 数约为旧词表的 1.3 倍。旧词表也不公开，所以没有任何
	// 可用来标定的真值。这里的做法是拿 tableUnknown 乘 1.3 再留余量。
	// 使用方应把它当作「量级正确、具体数字不可依赖」，见 ClaudeBounds.Basis。
	tableClaude = weightTable{
		weight: [numClasses]float64{
			classLatin:       0.2989,
			classDigit:       0.7977,
			classCJK:         1.4555,
			classOtherLetter: 1.1211,
			classSpace:       0.2504,
			classPunct:       0.4571,
			classSymbol:      2.3444,
		},
		bias:            0.757,
		runFloorLen:     24,
		runFloorDensity: 0.65,
	}
)

// tableFor 选该族的权重表。
func tableFor(f Family) weightTable {
	switch f {
	case FamilyOpenAI:
		return tableOpenAI
	case FamilyClaude:
		return tableClaude
	default:
		return tableUnknown
	}
}

// estimate 估算一段文本的 token 数。
//
// 返回浮点而不是取整：把取整留给最后一步，多次估算相加时能少丢一点精度，
// 而「先取整再相加」会让每一段各丢半个 token，长输入上累积成几十个。
//
// 空文本返回 0 而不是某个下限：空文本确实没有 token，给个非零值会让
// 「没有内容」与「有一点点内容」分不开。
func estimate(text string, t weightTable) float64 {
	if text == "" {
		return 0
	}

	total := 0.0

	// 第一步：逐段累加。
	var runLen int
	var runClass charClass
	first := true
	for _, r := range text {
		c := classOf(r)
		if first || c != runClass {
			if !first {
				total += runValue(t, runClass, runLen)
			}
			runLen, runClass, first = 1, c, false
			continue
		}
		runLen++
	}
	if !first {
		total += runValue(t, runClass, runLen)
	}

	// 第二步：对超长的字母数字串施加额外下限。
	total += runFloorExtra(text, t)
	return total * t.bias
}

// runValue 是一段同类字符的贡献。
//
// 下限 1 的理由：BPE 的分段单位是「段」，而任何非空段至少产生一个 token。
// 实测印证：`\n\n \t\t ` 这种 5 字符的空白串是 3 个 token（三个空白段），
// 而不是按密度算出的 0.5 个。
func runValue(t weightTable, c charClass, n int) float64 {
	v := t.weight[c] * float64(n)
	if v < 1 {
		return 1
	}
	return v
}

// isCodeish 判断一个字符是否属于「不可压缩串」的候选集合。
//
// 包括字母、数字，以及 base64 / 十六进制 / 标识符里常见的 +/= -_ 四个符号。
// 它们出现在实测样本里的形态是密钥、摘要、UUID、base64 图片数据。
func isCodeish(r rune) bool {
	if r < utf8.RuneSelf {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
			return true
		case r == '+' || r == '/' || r == '=' || r == '-' || r == '_':
			return true
		}
		return false
	}
	return false
}

// runFloorExtra 计算超长字母数字串相对普通线性估算需要补加的部分。
//
// # 为什么需要这一步
//
// BPE 词表里收录的是**自然语言里的词片段**。密钥、摘要、base64 这类串
// 不属于任何自然语言，词表里没有它们的片段，于是分词退化成逐字符甚至
// 逐字节：实测 hex 摘要 0.67 token/字符，base64 0.63，随机字母串 0.53。
// 而线性模型给拉丁/数字的密度只有 0.23 ~ 0.61，会低估三成。
//
// # 为什么限定「长且不中断」
//
// 短串（`main.go`、`v1.2`、`read_file`）是正常的标识符，词表里通常
// 有它们的片段，按普通密度算就是对的。而真实密钥与摘要都在 20 字符以上。
// 24 这个阈值取自实测：24 字符以下的最坏低估在 -30% 以内，
// 24 字符以上低估迅速加深到 -35%。阈值取整到 24 而不是 20 或 32，
// 是因为它是「不再可能是普通词」的那个长度附近的一个好记的数。
//
// # 为什么密度取 0.65
//
// 实测的不可压缩串密度：hex 64 字符 0.672、hex 36 字符 0.500、
// base64 80 字符 0.625、随机字母串（4~48 字符）0.52 ~ 0.58。
// 取 0.65 是这批观测的上沿，方向仍然是偏高。
//
// 与线性部分**取较大值而不是相加**：线性部分本来也给这段算了贡献，
// 直接把下限当作总量会让长串被算两次。
func runFloorExtra(text string, t weightTable) float64 {
	if t.runFloorLen <= 0 {
		return 0
	}
	extra := 0.0
	runLen := 0
	// body 记录这段串在线性部分已经得到的贡献，用于与下限比较。
	body := 0.0
	flush := func() {
		if runLen >= t.runFloorLen {
			if f := t.runFloorDensity * float64(runLen); f > body {
				extra += f - body
			}
		}
		runLen, body = 0, 0
	}
	for _, r := range text {
		if isCodeish(r) {
			runLen++
			body += t.weight[classOf(r)]
			continue
		}
		flush()
	}
	flush()
	return extra
}
