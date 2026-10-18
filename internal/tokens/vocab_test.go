package tokens

import (
	"context"
	"slices"
	"sort"
	"testing"

	"github.com/tiktoken-go/tokenizer"
)

// 能力上限里的每一种都必须**真的能加载**。
//
// 这条取代了原先按字面量比对的一份清单：那份清单里写着 gpt2，而
// tokenizer.Get 对它返回 ErrEncodingNotSupported（依赖里有这个常量，
// Get 的 switch 里没有对应分支）。按字面量比对时两边都是同一个错，
// 用例照样通过——而 AvailableEncodings 是拿来回答「这台机器支持到什么
// 程度」的，报高了就是谎报。
func TestAvailableEncodingsActuallyLoad(t *testing.T) {
	got := AvailableEncodings()
	// 条数是这一条的正向证据：空列表与「一种都不支持」在输出上分不开。
	if len(got) == 0 {
		t.Fatal("能力列表为空：要么检查没跑成，要么一种编码都不支持")
	}
	for _, name := range got {
		if _, err := tokenizer.Get(tokenizer.Encoding(name)); err != nil {
			t.Errorf("能力列表里的 %s 加载不出来: %v", name, err)
		}
	}
}

// 就绪探针要真的把清单里每一种都加载一次，能加载时才算就绪。
//
// 判据有两半，缺一不可：返回 nil，**并且**词表真的进了内存（Loaded）。
// 只看返回值的话，一个「什么都不做就返回 nil」的实现照样通过——而它在
// 词表全坏掉的进程上会让 /readyz 继续报就绪，那正是这条探针要防的事。
func TestVocabReadyLoadsEveryEncoding(t *testing.T) {
	c := NewCounter()
	if loaded := c.Loaded(); len(loaded) != 0 {
		t.Fatalf("新建的计数器不该已经加载了词表，实际 %v", loaded)
	}

	if err := c.VocabReady(context.Background()); err != nil {
		t.Fatalf("词表加载得出来时就绪探针应当返回 nil，实际 %v", err)
	}

	loaded := c.Loaded()
	for _, name := range AvailableEncodings() {
		if !slices.Contains(loaded, name) {
			t.Errorf("探针跑完之后 %s 还没加载（Loaded = %v）：探针没有真的逐项加载", name, loaded)
		}
	}
}

// 计数器没建起来时探针报错，而不是报就绪。
//
// 生产里不会有这种实例（计数器由装配层建），但就绪检查是**照着探针的返回值**
// 决定摘不摘实例的：在「查不了」的时候返回 nil，等于把「不知道」说成「好」。
// 这一点与 postgres 探针同形——那边同样不在拿不到句柄时报 ok。
func TestVocabReadyRejectsUninitializedCounter(t *testing.T) {
	var nilCounter *Counter
	if err := nilCounter.VocabReady(context.Background()); err == nil {
		t.Error("nil 计数器上应当报错，而不是报就绪")
	}

	zero := &Counter{}
	if err := zero.VocabReady(context.Background()); err == nil {
		t.Error("零值计数器上应当报错，而不是报就绪")
	}
}

// 探针被放弃时不能报就绪。
//
// 加载本身不可中断，放弃点落在两次加载之间；ctx 已经取消说明这一轮探测的
// 结论没人要了（就绪检查带超时）。这时返回 nil 会让一次被取消的探测看起来
// 像一次成功的探测。
func TestVocabReadyReportsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := NewCounter().VocabReady(ctx); err == nil {
		t.Error("ctx 已取消时应当返回错误")
	}
}

// 预热集合必须是能力上限的子集。
//
// 反过来的话，Prepare 会去加载一个「本机不支持」的编码并因此失败——
// 而失败的后果是整轮预热放弃（见 vocabSet.prepare 的返回），
// 连本来能加载的那些也白等了。
func TestPreparedCodecsAreAvailable(t *testing.T) {
	available := AvailableEncodings()
	for _, enc := range preparedCodecs {
		if !slices.Contains(available, string(enc)) {
			t.Errorf("preparedCodecs 里的 %s 不在能力列表里: %v", enc, available)
		}
	}
}

// 预热集合必须覆盖 encodingForModel 的**整个值域**。
//
// 值域从 encodingRules 那张表推导，**不另外抄一份模型名清单**：抄一份的话，
// 表里加了一条规则而清单没跟着改，两边都不会红，而用那种编码的模型第一次
// 调用照样要为词表加载付时间（同一件事实写在两处、谁也不从谁推导，见 #289）。
//
// 这是 #259 的核心判据：只断言「调过 Prepare」测不出它。
func TestPreparedCodecsCoverEveryReachableEncoding(t *testing.T) {
	prepared := map[tokenizer.Encoding]bool{}
	for _, enc := range preparedCodecs {
		prepared[enc] = true
	}

	// 值域 = 表里出现过的每一种编码 + 兜底那个。
	reachable := map[tokenizer.Encoding]bool{defaultEncoding: true}
	for _, rule := range encodingRules {
		reachable[rule.enc] = true
	}
	// 条数是这一条的正向证据：空值域会让下面的循环一次都不跑，
	// 而「一个都没漏」与「什么都没检查」在输出上分不开。
	if len(reachable) < 2 {
		t.Fatalf("值域只算出 %d 种编码，表多半是空的: %v", len(reachable), reachable)
	}

	var missing []string
	for enc := range reachable {
		if !prepared[enc] {
			missing = append(missing, string(enc))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("preparedCodecs 没覆盖到 %v —— 用这些编码的模型第一次调用要为词表加载付时间",
			missing)
	}
}

// 值域里出现的每一种编码都必须**真的能加载**。
//
// 表里写错一个编码名（比如写成一个 tokenizer 常量里不存在的），上面那条
// 用例会要求 Prepare 去加载它，而加载失败会让整轮预热放弃——连本来能加载
// 的那些也白等。这条把失败点前移到用例里。
func TestReachableEncodingsAreLoadable(t *testing.T) {
	for _, rule := range encodingRules {
		if _, err := tokenizer.Get(rule.enc); err != nil {
			t.Errorf("encodingRules 里的 %s 加载不出来: %v", rule.enc, err)
		}
	}
	if _, err := tokenizer.Get(defaultEncoding); err != nil {
		t.Errorf("defaultEncoding %s 加载不出来: %v", defaultEncoding, err)
	}
}
