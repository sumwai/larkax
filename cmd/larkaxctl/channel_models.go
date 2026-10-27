package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/upstream"
)

// 本文件是 `channel sync-models` 与 `channel upstream-models` 两条子命令。
//
// 两者是同一份数据的写入与读取：前者去上游拉一次并落库，后者只读库。分开而不
// 合成一条命令的 `--refresh` 开关，是因为它们的性质不同——一条会碰网络与凭证，
// 一条完全不碰；脚本里「查一下现在记着什么」不该因为顺手多点了一个开关就变成
// 一次出网调用。

// runChannelSyncModels 从上游拉取一条渠道的模型清单并落库。
//
// 上游不提供模型列表接口是常态：适配器返回 ErrUnsupported 时这里照样成功
// 返回（退出码 0），库里只把状态记成 unsupported，已有清单一行不动。
func runChannelSyncModels(args []string) error {
	if err := cli.JSONUnsupported("channel sync-models"); err != nil {
		return err
	}

	id, err := parseChannelModelSyncID("sync-models", args)
	if err != nil {
		return cli.UsageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		sealer, err := masterKeySealer()
		if err != nil {
			return err
		}
		registry, err := upstream.Registry()
		if err != nil {
			return fmt.Errorf("装配渠道适配器注册表失败: %w", err)
		}

		result, err := channels.NewModelSync(db, sealer, registry).Sync(ctx, id)
		if err != nil {
			return err
		}
		printModelSyncResult(cli.HumanStderr(), result)
		return nil
	})
}

// runChannelUpstreamModels 打印一条渠道记着的上游模型清单与同步状态。
func runChannelUpstreamModels(args []string) error {
	id, err := parseChannelModelSyncID("upstream-models", args)
	if err != nil {
		return cli.UsageErr(err)
	}

	return withDB(channelTimeout, func(ctx context.Context, db *sql.DB) error {
		models, err := channels.NewModelSync(db, nil, nil).StoredModels(ctx, id)
		if err != nil {
			return err
		}
		if cli.JSONOutput() {
			return cli.EmitJSON(os.Stdout, channelModelsDoc(models), nil)
		}
		printChannelUpstreamModels(cli.HumanStdout(), models)
		return nil
	})
}

// parseChannelModelSyncID 解析两条命令共用的 --id。
//
// 校验放在连库之前：id 写错是命令错（退出码 2），不该先连一次库再报。
func parseChannelModelSyncID(action string, args []string) (int64, error) {
	fs := cli.NewFlagSet("channel "+action, flag.ContinueOnError)
	fs.Usage = func() { cli.PrintUsage(os.Stderr, channelShortUsage) }
	id := fs.Int64("id", 0, "渠道 ID")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if fs.NArg() > 0 {
		return 0, fmt.Errorf("%s 不接受位置参数，得到 %q", action, fs.Arg(0))
	}
	if *id == 0 {
		return 0, fmt.Errorf("缺少 --id：%s 针对一条渠道", action)
	}
	if *id < 0 {
		return 0, fmt.Errorf("--id %d 无效：渠道 ID 从 1 开始", *id)
	}
	return *id, nil
}

// printModelSyncResult 打一次同步的结果。
//
// 写入类命令的回显走 stderr：stdout 留给能被脚本接走的值，而这条命令没有
// 「新实体的 ID」这类值要给出去。上游不支持不是失败，这一句要说清「什么都没
// 改」——否则跑完看到状态是 unsupported 的人会以为清单被清空了。
func printModelSyncResult(w io.Writer, result channels.ModelSyncResult) {
	if result.Status == channels.ModelSyncUnsupported {
		_, _ = fmt.Fprintf(w,
			"渠道 %d（%s）的上游不提供模型列表接口：已记成「上游不支持」，已有清单没有改动（共 %d 个模型）。\n",
			result.ChannelID, result.ChannelName, result.Total)
		return
	}

	_, _ = fmt.Fprintf(w, "渠道 %d（%s）的上游模型清单已同步：共 %d 个模型，新增 %d，下线 %d。\n",
		result.ChannelID, result.ChannelName, result.Total, len(result.Added), len(result.Removed))
	_, _ = fmt.Fprintf(w, "同步时刻：%s。\n", formatTimeValue(result.SyncedAt))
}

// printChannelUpstreamModels 打印一条渠道记着的上游模型清单。
//
// 与 `channel get` 里那一栏 `models` 不是同一份东西：那一栏是选路用的**规范
// 模型名**，这里是上游自己的模型名。两份都在屏幕上时，标题与说明要把这一点
// 说清；混着读会以为清单已经参与选路了。
func printChannelUpstreamModels(w io.Writer, models channels.ChannelModels) {
	p := cli.NewPalette(w)
	_, _ = fmt.Fprintf(w, "渠道 %d（%s）的上游模型清单\n", models.ChannelID, models.ChannelName)
	_, _ = fmt.Fprintf(w, "%s：%s\n", p.Bold("同步状态"), modelSyncStatusLabel(models.Status))
	if !models.AttemptedAt.IsZero() {
		_, _ = fmt.Fprintf(w, "%s：%s\n", p.Bold("最近一次尝试"), formatTimeValue(models.AttemptedAt))
	}
	if !models.SyncedAt.IsZero() {
		_, _ = fmt.Fprintf(w, "%s：%s\n", p.Bold("最近一次成功"), formatTimeValue(models.SyncedAt))
	}
	_, _ = fmt.Fprintln(w, "这一份是上游的模型名，不参与选路：channel get 里那一栏 models 才是选路用的规范名。")

	if len(models.Models) == 0 {
		// 空清单有三种成因，说法不能混：「上游没有这个接口」「从没同步过」
		// 「同步成功但上游回的是空清单」。同一句话说三遍会让人分不清该做什么。
		switch models.Status {
		case channels.ModelSyncUnsupported:
			_, _ = fmt.Fprintln(w, "上游不提供模型列表接口，所以库里没有这份清单。")
		case channels.ModelSyncNever:
			_, _ = fmt.Fprintln(w, cli.EmptyListNote("上游模型", "channel sync-models"))
		default:
			_, _ = fmt.Fprintln(w, "上一次同步拉到的清单是空的。")
		}
		return
	}

	_, _ = fmt.Fprintln(w)
	tw := cli.NewTableWriter(w)
	_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n",
		p.Bold("上游模型名"), p.Bold("首次见到"), p.Bold("最近见到"))
	for _, model := range models.Models {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n",
			p.Bold(model.UpstreamModel),
			formatStampShort(model.FirstSeenAt),
			formatStampShort(model.LastSeenAt))
	}
	_ = tw.Flush()
}

// modelSyncStatusLabel 把同步状态翻成给人看的那一栏。
func modelSyncStatusLabel(status string) string {
	switch status {
	case channels.ModelSyncSynced:
		return "已同步"
	case channels.ModelSyncUnsupported:
		return "上游不支持"
	default:
		return "未同步"
	}
}

// channelModelsDoc 组装 `channel upstream-models --format json` 的机器可读输出。
func channelModelsDoc(models channels.ChannelModels) map[string]any {
	doc := cli.JSONDoc(cli.JSONOK)
	doc["channel"] = map[string]any{
		"id":   models.ChannelID,
		"name": models.ChannelName,
	}
	doc["sync"] = map[string]any{
		"status":       models.Status,
		"attempted_at": cli.JSONTimeOrNull(models.AttemptedAt),
		"synced_at":    cli.JSONTimeOrNull(models.SyncedAt),
	}
	doc["summary"] = map[string]any{"total": len(models.Models)}

	// 从空切片起：一条都没有时打 `[]`，不是 `null`（见 cli 包的 format.go 的 jsonList）。
	entries := make([]map[string]any, 0, len(models.Models))
	for _, model := range models.Models {
		entries = append(entries, map[string]any{
			"upstream_model": model.UpstreamModel,
			"first_seen_at":  cli.JSONTime(model.FirstSeenAt),
			"last_seen_at":   cli.JSONTime(model.LastSeenAt),
		})
	}
	doc["models"] = entries
	return doc
}
