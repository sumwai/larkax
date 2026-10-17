package credential

// hintRunes 是辨识提示取末尾几个字符。
//
// 取四位而不是更多：提示要能被运维一眼扫过去，而它的用途只是「这几条渠道里
// 哪一条挂的是我刚发的那个凭证」。凭证本身有 32 字节以上的随机量，
// 四位足够区分同一批里的几条，又不至于让一份清单变成凭证的一部分。
const hintRunes = 4

// Hint 取明文凭证的末几位，用于界面辨识（落 channels.credential_hint）。
//
// 它**不是**摘要，也不能用来还原凭证：取的是明文尾部，与信封加密无关——
// 调用点在封装之前（见 internal/channels 的 Create / Update），
// 密文一旦生成就再也拿不到明文了。
//
// 按**字符**而不是按字节切。凭证是各家上游自己发的串，不保证是 ASCII；
// 按字节切会把一个多字节字符切成半个，打出来是乱码，而乱码的提示比没有
// 提示更坏——它看起来像一个真实的凭证尾巴。ASCII 串上两种切法相同。
//
// 短于 hintRunes 时原样返回：那种凭证本身就短到没有「末几位」可辨，
// 截成空串会让界面把它显示成「这条渠道没有凭证」。
func Hint(plaintext string) string {
	runes := []rune(plaintext)
	if len(runes) <= hintRunes {
		return plaintext
	}
	return string(runes[len(runes)-hintRunes:])
}
