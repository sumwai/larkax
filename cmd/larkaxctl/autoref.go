package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"
)

// 这一层管的是手工操作的幂等单号：--auto-ref 的生成器，以及「--ref 与 --auto-ref
// 二选一」这一条判据。
//
// 收单号的写命令有 plan grant / plan void、wallet credit / adjust / refund、
// wallet payout request / settle 与 wallet payment create。它们的单号来自同一种
// 现实：银行流水号、回单号、通道给的单号。手工操作时操作者常常手上还没有那个号，
// 于是有了 --auto-ref：显式要一个现生成的单号，命令照样把它打进输出，事后按那个
// 串回查。
//
// **两个都没给是参数错，不是默默生成**。单号是幂等的锚点：默默生成会让「重跑同
// 一条命令」从「幂等命中」变成「真的又执行一次」——对入账与退款来说，那是一次
// 静默的重复扣款。所以自动生成必须由操作者显式要求。

const (
	// autoRefFlagName 是旗标名。两个都没给时的报错要点出它，读的人才能照着改。
	autoRefFlagName = "auto-ref"

	// autoRefPrefix 是自动单号的前缀。读账本的人一眼能认出「这是命令生成的」，
	// 而不是某个外部单据号。
	autoRefPrefix = "MANUAL_"

	// autoRefRandomBytes 是随机段的字节数。6 字节是 48 位，手工操作一年不过几百
	// 笔，撞号要千万笔的量级；而撞号意味着第二次被当成幂等重放、静默不写，所以
	// 这里多留几位。写成十六进制之后是 12 个字符。
	autoRefRandomBytes = 6
)

// autoRefFlag 注册 --auto-ref。
//
// 说明文字留空，与包内其余旗标一致：帮助文案的唯一来源是各文件里的 *Usage 常量
// （见包注释）。
func autoRefFlag(fs *flag.FlagSet) *bool {
	return fs.Bool(autoRefFlagName, false, "")
}

// newAutoRef 生成一个自动单号：MANUAL_<UTC 日期>_<随机段>。
//
// 日期按 UTC 算，与 --ttl / --expires-at 的日历运算同一个口径（见 expiry.go）：
// 同一笔操作在哪台机器、哪个时区敲，单号里的日期都是同一个，对账时不按执行者所在
// 时区漂。
//
// 随机段取自 crypto/rand，不碰数据库序列：生成发生在连库之前，--dry-run 那一路也
// 走它，所以离线就该跑得动。整个串 28 个字符，落进 text 列绰绰有余。
func newAutoRef(now time.Time) (string, error) {
	var buf [autoRefRandomBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成自动单号失败（读不到随机源）: %w", err)
	}
	return autoRefPrefix + now.UTC().Format("20060102") + "_" + hex.EncodeToString(buf[:]), nil
}

// resolveOrderRef 在一条命令的单号上做一次选择。
//
// 三种情况：给了 --ref 就用它；只给 --auto-ref 就现生成一个；两个都没给是参数错，
// 报 missing 那句话——每条命令自己写，说清这个单号在本命令里是什么东西。
//
// 两个都给同样按参数错拒掉：它们是同一个问题的两种回答，同时出现说明操作者心里有
// 两笔单子，这时挑哪一个都可能不是他要的。
func resolveOrderRef(refValue string, autoRef bool, missing error) (string, error) {
	ref := strings.TrimSpace(refValue)
	switch {
	case ref != "" && autoRef:
		return "", usageErr(fmt.Errorf(
			"--ref 与 --%s 只能给一个：有外部单号就用 --ref（重跑才会幂等命中），"+
				"自动生成只在手上还没有单号时用", autoRefFlagName))
	case ref != "":
		return ref, nil
	case autoRef:
		return newAutoRef(time.Now())
	default:
		return "", usageErr(missing)
	}
}

// missingRefError 拼出「两个都没给」时的那句话。
//
// 每条命令的外沿不同（入账、退款、发放、作废、申请、清算、下单），但后半句是
// 同一个出口，所以只在这里写一遍；前半句由调用方给。
func missingRefError(what string) error {
	return errors.New("缺少 --ref 或 --" + autoRefFlagName + "：" + what +
		"；手上有外部单据号就写 --ref，没有就加 --auto-ref 让命令现生成一个")
}
