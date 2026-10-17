package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/idempotency"
	"github.com/sumwai/larkax/internal/observe"
)

// 幂等键那条路的全部逻辑。
//
// # 顺序是「先查，再走，再记」
//
// 查在**任何有副作用的事情之前**。反过来（先预留、撞键了再回滚）会让一次
// 撞键白白冻一笔钱，而解冻失败时那笔钱就永久冻着——那正是幂等键要防的
// 那类问题换了个形态。
//
// 「记」分两步，分属两个事务，这不是疏忽：
//
//   - **建立**记录与预留同一个事务，由落库层做（见 internal/ledger/store 的
//     SaveReservation）。两者必须一起成或一起不成。
//   - **写回结局**（走完了 / 没走完）只能在响应写出去之后做，那时预留那个
//     事务早就提交了，所以它是单独一笔。
//
// 第二步失败时那条记录会停在 in_progress（见 recordOutcome 的注释），
// 这是这个设计里唯一一处「记录与实际不一致」的窗口。

// idempotencyPollInterval 是等待「同键的第一次请求」出结果的轮询间隔。
//
// 用轮询而不是通知：一次撞键本来就是少数情形，而 PostgreSQL 的
// LISTEN/NOTIFY 要一条常驻连接与一套重连逻辑，代价远大于它省下的那几次
// 按主键定位的单行查询。
//
// 100ms 的取舍：客户端感知到的额外延迟不超过它，而一次长达数分钟的请求
// 最多产生几百次单行查询。把间隔调大能省查询，代价是每个撞键的请求都
// 多等那么久——而撞键的正是**已经在等**的那个客户端。
const idempotencyPollInterval = 100 * time.Millisecond

// idempotencyWriteTimeout 是写回幂等结局的时间上限。
//
// 与网关释放预留那一步同一个量级（见 internal/gateway 的 releaseTimeout）：
// 写的都是按主键定位的单行，量级是毫秒；连它都写不进去，说明库已经不接受
// 写入了，把上限放宽也救不回来，而这一步不许挂住一个已经结束的请求。
const idempotencyWriteTimeout = 5 * time.Second

// serveIdempotent 处理一次带了 Idempotency-Key 的请求。
//
// 三种走向：命中一条可以重放的记录 → 原样再发一遍；命中一条不能重放的
// 记录 → 一条 409；没有记录 → 走原路径，并把这次响应留一份供后续重放。
func (h *Handler) serveIdempotent(w http.ResponseWriter, r *http.Request, protocol channel.Protocol, upstream *channel.Request, user User, requestID, key string) {
	if err := idempotency.ValidateKey(key); err != nil {
		// 键本身不合法是调用方的错，当场可诊断。放过去的话，一个超长的键
		// 会进唯一索引，而代价由别人的库承担。
		writeError(w, protocol, http.StatusBadRequest, "invalid Idempotency-Key: "+err.Error())
		return
	}
	hash, err := idempotency.HashRequest(protocol, upstream)
	if err != nil {
		// 算不出指纹就**不做幂等**，而不是回落到一个默认指纹：回落会让两个
		// 不同的请求得到同一个指纹，于是第二个请求拿到第一个的响应——
		// 那是对调用方撒谎，而且看不出来。
		observe.FromContext(r.Context()).Error("计算幂等指纹失败", "err", err)
		writeError(w, protocol, http.StatusInternalServerError, "internal error")
		return
	}
	claim := &idempotency.Claim{APIKeyID: user.KeyID, Key: key, Hash: hash}

	for {
		if h.resolveExisting(w, r, protocol, claim) {
			// 已经写出响应了：要么是重放，要么是一条 409。
			return
		}

		// 没记录：走原路径，响应同时留一份。
		captured := &captureWriter{ResponseWriter: w}
		res := h.serveOnce(captured, r, protocol, upstream, user, requestID, claim)

		if res.claimed {
			// 在「查完」与「建立记录」之间被另一个并发请求抢走了。这不是
			// 错误：那次预留整个回滚了，一分钱没冻、上游一次没调。
			//
			// 等一小会儿再查一次——下一次循环会按那条记录的状态处置。
			// 不先等一下就直接重试，会在这个极短的窗口里空转。
			if !waitForRetry(r.Context(), idempotencyPollInterval) {
				writeIdempotencyError(w, protocol, CodeIdempotencyInProgress, msgIdempotencyInProgress)
				return
			}
			continue
		}

		h.recordOutcome(r, claim, captured, res)
		return
	}
}

// resolveExisting 处理「这个键已经有记录」的情形。
//
// 返回 true 表示已经写出了响应（重放，或一条 409），调用方直接返回；
// 返回 false 表示这个键没有记录，可以照常执行这次请求。
func (h *Handler) resolveExisting(w http.ResponseWriter, r *http.Request, protocol channel.Protocol, claim *idempotency.Claim) bool {
	for {
		rec, err := h.Idempotency.Lookup(r.Context(), claim.APIKeyID, claim.Key)
		switch {
		case errors.Is(err, idempotency.ErrNotFound):
			// 没有记录：可能从来没用过这个键，也可能是**第一次请求的预留
			// 事务回滚了**（余额不足、没有可用渠道），那条记录随事务一起
			// 没了。两种情况都该照常执行——回滚意味着上游一次都没被调用过，
			// 所以这里再跑一遍不会多出一次真实调用。
			return false
		case err != nil:
			// 查不动的时候**不能**当成「没命中」继续走：一次数据库抖动就会
			// 变成一次真的重复调用，而那正是这个功能要防的事。宁可回 500。
			observe.FromContext(r.Context()).Error("读幂等记录失败", "err", err)
			writeError(w, protocol, http.StatusInternalServerError, "internal error")
			return true
		}

		if rec.Hash != claim.Hash {
			// 同一个键配上了一个不同的请求体。回第一次的响应等于对一个新
			// 请求撒谎——客户端以为自己的新请求被处理了。报错是安全的那一侧，
			// 而且当场可诊断。
			writeIdempotencyError(w, protocol, CodeIdempotencyKeyReused, msgIdempotencyKeyReused)
			return true
		}

		switch rec.State {
		case idempotency.StateCompleted:
			writeReplay(w, rec)
			return true
		case idempotency.StateFailed:
			// 第一次没走完，库里没有一份完整的响应可以还给他。重放一段
			// 截断的响应比不重放更坏（见 internal/idempotency 的包注释）。
			writeIdempotencyError(w, protocol, CodeIdempotencyIncomplete, msgIdempotencyIncomplete)
			return true
		}

		// in_progress：同键的第一次请求还在跑。**等**，不立刻报错——
		// 立刻报错的话客户端分辨不出「撞上了自己的并发」与「一次瞬时故障」，
		// 于是它会再重试一次，而那正是要避免的。
		//
		// 上限就是这次请求自己的超时：等下去的是调用方的时间与钱，
		// 由它自己设的上限来结束最合适。
		if !waitForRetry(r.Context(), idempotencyPollInterval) {
			writeIdempotencyError(w, protocol, CodeIdempotencyInProgress, msgIdempotencyInProgress)
			return true
		}
	}
}

// recordOutcome 把这次请求的结局写回幂等记录。
//
// # 它为什么用不带取消的 context
//
// 最需要写它的时刻，恰恰是调用方已经断开、请求 ctx 已经 Done 的那一刻——
// 那种结局要记成 failed，否则同键的下一次请求会去打一次本不该打的上游。
// 拿一个已取消的 ctx 去写，写本身就会失败。与网关释放预留同一个做法
// （见 internal/gateway 的 release）。
//
// # 写不进去的代价要说清楚
//
// 响应已经发给调用方了，这里改不了任何事，所以只记日志。代价是那条记录
// 停在 in_progress：后续同键请求会一直等到自己的超时才拿到 409，
// 而真正的原因只在这条日志里。这是这个设计里唯一一处「记录与实际不一致」
// 的窗口，也是那条日志必须带着键名的原因。
//
// **记录根本不在**是另一回事，它不是「写不进去」：那说明这一次请求的预留
// 整个回滚了（余额不足、没有可用渠道、缺 max_tokens），幂等记录随那个事务
// 一起没了。那是正常路径，只记 Debug——把两种情形混在一起，那条唯一的告警
// 就会在每次余额不足时都响一遍。
func (h *Handler) recordOutcome(r *http.Request, claim *idempotency.Claim, captured *captureWriter, res served) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), idempotencyWriteTimeout)
	defer cancel()

	var err error
	if res.complete {
		err = h.Idempotency.Complete(ctx, claim.APIKeyID, claim.Key,
			captured.status, captured.contentType, captured.body.Bytes())
	} else {
		err = h.Idempotency.Fail(ctx, claim.APIKeyID, claim.Key)
	}
	if err != nil {
		if errors.Is(err, idempotency.ErrNotFound) {
			// 这条记录**根本不在**：这一次请求的预留整个回滚了（余额不足、
			// 没有可用渠道、缺 max_tokens），幂等记录随那个事务一起没了。
			//
			// 这是**正常路径**——用户余额不足是常态，不是故障——所以这里
			// 不能打 ERROR。那条日志是这个设计里唯一能指出「记录停在
			// in_progress」的信号（见上面的注释），而它在正常路径上每次都响，
			// 等于把信号淹掉：一个总在喊的告警与一个从不喊的告警一样没用。
			observe.FromContext(r.Context()).Debug("这次请求没建成幂等记录，没有结局要写回",
				"key", claim.Key)
			return
		}
		observe.FromContext(r.Context()).Error("写幂等结局失败",
			"err", err, "key", claim.Key, "complete", res.complete)
	}
}

// captureWriter 把写给调用方的响应同时留一份下来。
//
// 它只用在幂等那条路上。留的是**真正写出去的字节**，不是「再渲染一遍」：
// 重放要的是逐字节相同，而第二处渲染逻辑迟早与第一处不一致——
// 那种不一致表现为「重试拿到的回答与第一次不一样」，且没有任何东西会报。
type captureWriter struct {
	http.ResponseWriter

	// status 为 0 表示还没写出响应头。
	status      int
	contentType string
	body        bytes.Buffer
}

// WriteHeader 记下真正发出去的状态码与 Content-Type，再透传。
//
// 它实现的是 http.ResponseWriter，所以没有「未导出」这个选项——revive 要的
// 那句话就写在这里。
func (c *captureWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
		// Content-Type 要在**这一行**取：WriteHeader 之后头就发出去了，
		// 而留档的那一份必须与发出去的那一份是同一个。
		c.contentType = c.Header().Get("Content-Type")
	}
	c.ResponseWriter.WriteHeader(status)
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		// 没显式写状态码时 net/http 按 200 发出，这里跟上：留档的状态码
		// 必须与真正发出去的那个一致，否则重放会回一个不同的状态码。
		c.WriteHeader(http.StatusOK)
	}
	c.body.Write(p)
	return c.ResponseWriter.Write(p)
}

// Unwrap 让 http.NewResponseController 能穿透到真正的 ResponseWriter。
//
// 少了它 Flush 会失败，而流式就此退化成攒完再发——一个只在带了幂等键的
// 请求上出现的性能退化，正是那种「只在少数路径上复现」的问题。
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// writeReplay 把第一次的响应原样再发一遍。
//
// 状态码、响应头与响应体都取自留档的那一份：这是「重放」的全部含义。
func writeReplay(w http.ResponseWriter, rec *idempotency.Record) {
	if strings.HasPrefix(rec.ResponseContentType, sseContentTypePrefix) {
		// 流式响应不止 Content-Type 一个头。少了另外两个，一次重放出去的
		// SSE 可以被中间缓存住，也可以被反向代理缓冲——第一次请求特意防住的
		// 那两件事，在重放这条路上漏了回来。
		//
		// 复用 streamHeaders 而不是把三个头抄一遍：抄一遍的话，以后往那里
		// 加头就只在新请求上生效，重放又会悄悄少一个。
		streamHeaders(w.Header())
	}
	if rec.ResponseContentType != "" {
		// 以留档的那一份为准：它才是真正发出去的那个值（可能带着别的
		// charset 写法），streamHeaders 里的只是同一批头的定义。
		w.Header().Set("Content-Type", rec.ResponseContentType)
	}
	status := rec.ResponseStatus
	if status == 0 {
		// 列可空。completed 的行一定有值（见 idempotency.Store.Complete），
		// 但一行也可能是被别的手段写进去的；回一个 200 总好过让
		// WriteHeader(0) panic。
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(rec.ResponseBody)
}

// waitForRetry 等一小会儿，请求被取消（或超时）时返回 false。
//
// 取消就返回 false 而不是继续轮询：调用方不在了，再查下去只是白烧查询。
func waitForRetry(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
