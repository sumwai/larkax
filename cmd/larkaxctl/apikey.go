package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/config"
)

const apiKeyUsage = `larkaxctl apikey 管理 API Key。

用法：
  larkaxctl apikey create --user <ID 或用户名> [--name <备注>] [--expires-at <RFC3339>]

create 生成一把新 key，只把哈希入库。

**明文只打印这一次**：库里存的是 SHA-256 哈希，它不可逆，
丢了没有任何办法取回，只能重建一把。

明文单独打到 stdout，其余信息打到 stderr，所以

  larkaxctl apikey create --user alice > key.txt

保存下来的文件里只有明文，不会混进前缀、id 与提示语。

`

// apiKeyTimeout 是这条命令连库与写库的时间上限。
//
// 命令行要有上限，否则一次卡住的连接会挂到人发现为止。
// 30 秒对一条单行插入来说宽得离谱，取这个值只是为了让「卡住了」
// 有个终点，而不是把等待当成正常。
const apiKeyTimeout = 30 * time.Second

func runAPIKey(args []string) error {
	fs := flag.NewFlagSet("apikey", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, apiKeyUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return usageErr(errors.New("apikey 需要子命令"))
	}
	action, rest := rest[0], rest[1:]

	switch action {
	case "create":
		return runAPIKeyCreate(rest)
	default:
		return usageErr(fmt.Errorf("未知的 apikey 子命令 %q", action))
	}
}

func runAPIKeyCreate(args []string) error {
	fs := flag.NewFlagSet("apikey create", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, apiKeyUsage) }
	user := fs.String("user", "", "key 的归属用户：用户 ID 或用户名")
	name := fs.String("name", "", "备注名，用于日后在列表里认出这把 key")
	expiresAt := fs.String("expires-at", "", "过期时间（RFC3339），留空表示不过期")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 0 {
		return usageErr(fmt.Errorf("create 不接受位置参数，得到 %q：用户用 --user 指定", fs.Arg(0)))
	}
	if *user == "" {
		fs.Usage()
		return usageErr(errors.New("缺少 --user"))
	}

	var expires *time.Time
	if *expiresAt != "" {
		t, err := time.Parse(time.RFC3339, *expiresAt)
		if err != nil {
			return usageErr(fmt.Errorf("--expires-at 不是 RFC3339 时间（例如 2026-12-31T00:00:00Z）: %w", err))
		}
		// 已过去的时刻直接拒绝，而不是建一把立刻不可用的 key。
		// 那种情况几乎只可能是写错了年份（2025 写成 2024），
		// 而它的表现是「刚建好的 key 就说无效」——最费时的一类排查。
		// auth.Create 本身不拦：库层允许建一把已过期的 key（造测试数据、
		// 补历史数据都要用到），拦在交互式命令这一层才合适。
		if !t.After(time.Now()) {
			return usageErr(fmt.Errorf("--expires-at %s 已经过去了：这样的 key 建出来就是不可用的",
				t.Format(time.RFC3339)))
		}
		expires = &t
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiKeyTimeout)
	defer cancel()
	db, err := openDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	store := auth.New(db)
	// 先解析用户再建 key：外键约束也会挡下不存在的用户，但那时错误信息
	// 来自数据库（一句外键冲突），而这里能说清是哪个标识没对上。
	owner, err := store.ResolveUser(ctx, *user)
	if err != nil {
		return err
	}

	created, err := store.Create(ctx, auth.CreateParams{
		UserID:    owner.ID,
		Name:      *name,
		ExpiresAt: expires,
	})
	if err != nil {
		return err
	}

	printCreatedKey(os.Stdout, os.Stderr, owner, created, expires)
	return nil
}

// printCreatedKey 打印新 key。
//
// 明文与其余信息分走两个流：`> key.txt` 是保存一把 key 最自然的动作，
// 而元信息混进去会让那个文件没法被程序直接读走（粘贴到客户端配置里时
// 多一行就少一个能用的 key）。
func printCreatedKey(stdout, stderr io.Writer, owner auth.User, c auth.Created, expires *time.Time) {
	expiry := "不过期"
	if expires != nil {
		expiry = expires.Format(time.RFC3339)
	}

	_, _ = fmt.Fprintf(stderr, "已创建 API Key id=%d prefix=%s 用户=%s(id=%d) 过期=%s\n",
		c.ID, c.Prefix, owner.Username, owner.ID, expiry)
	_, _ = fmt.Fprintln(stderr, "明文只打印这一次，请立即保存：库里只有哈希，无法再取回。")
	_, _ = fmt.Fprintln(stdout, c.Plaintext)
}
