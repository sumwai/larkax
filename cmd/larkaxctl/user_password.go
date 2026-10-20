package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sumwai/larkax/internal/auth"
)

// 本文件是控制台口令的两条运维命令：设口令与解锁。
//
// # 口令不进命令行参数
//
// `user password --password xxx` 这条写法会被记进 shell 历史，而同一个进程的
// 参数在 /proc 里对同机的其他用户可读。所以只有两条来路：`--generate` 让
// 本命令生成一个（随机数在这里，不在任何别处），`--stdin` 从标准输入读
// （管道或交互输入，都不落历史）。
//
// 口令也**不进日志、不回显到 stderr**：stdout 恰好一行，就是新口令。它能被
// 直接接走，也可以在终端里看一眼就消失——留不留在屏幕上由使用者决定。

// generatedPasswordBytes 是生成口令的熵：18 字节 → 24 个 base64url 字符。
//
// 24 个字符超过了策略下限（12），而策略上限（128）远够不着。用 base64url
// 而不是一份「好念的字母表」：口令由用户复制，不是由人转述；而一份去掉
// 易混字符的字母表会伪造出一种「好记」的错觉，实际价值是零。
const generatedPasswordBytes = 18

// maxPasswordInput 是从标准输入读口令的上限。
//
// 策略上限是 128 个字符；按 UTF-8 最坏的四字节算也才 512 字节。给 4 KiB
// 是为了容纳误输入（例如把一整份文件重定向进来）之后再给出清楚的报错，
// 而不是让 ReadAll 一直读到内存尽头。
const maxPasswordInput = 4 << 10

// passwordInput 是 `--stdin` 读口令的来源。
//
// 变量而不是直接写 os.Stdin：为了让这条路径在测试里能走通——「口令从标准
// 输入读」本身就是这条命令的一条契约。
var passwordInput io.Reader = os.Stdin

// runUserPassword 设或重置一个用户的口令。
func runUserPassword(args []string) error {
	if err := jsonUnsupported("user password"); err != nil {
		return err
	}

	user, generate, readStdin, err := parseUserPassword(args)
	if err != nil {
		return usageErr(err)
	}

	// 生成口令在连库之前做：熵源坏了是一次「这次命令根本没开始」的失败，
	// 不该在库里留下一行已经写了一半的状态。
	var password string
	if generate {
		password, err = generatePassword()
		if err != nil {
			return err
		}
	}

	return withDB(userTimeout, func(ctx context.Context, db *sql.DB) error {
		store := auth.New(db)
		info, err := store.UserByRef(ctx, user)
		if err != nil {
			return err
		}

		if readStdin {
			password, err = readPasswordInput()
			if err != nil {
				return usageErr(err)
			}
		}
		// 生成的那一个当然过得了；从标准输入来的要按同一套策略判。
		// 判据与登录侧共用 auth.ValidatePassword，不在这里另写一遍。
		if policyErr := auth.ValidatePassword(info.Username, password); policyErr != nil {
			return usageErr(policyErr)
		}

		hash, err := auth.HashPassword(auth.DefaultKDFParams, password)
		if err != nil {
			return fmt.Errorf("计算口令哈希失败: %w", err)
		}
		// keep 为 nil：吊销这个用户的**全部**会话。运维重置口令的场景是
		// 「我不知道谁还开着控制台」，而一个被重置之后仍然有效的旧会话，
		// 等于这次重置只对下一次登录生效。
		if err := store.SetPassword(ctx, info.ID, hash, nil); err != nil {
			return err
		}

		_, _ = fmt.Fprintln(humanStdout(), password)
		_, _ = fmt.Fprintf(humanStderr(),
			"已为用户 %s 设置新口令，只显示这一次；该用户的其他控制台会话已全部下线\n",
			info.Username)
		return nil
	})
}

// parseUserPassword 解析 `user password` 的参数。
//
// 用户标识既收 `--user`，也收第一个位置参数：设计把这条命令写成
// `user password <user>`，而本仓其余命令一律用 `--user`。两种都收、两边
// 都给时报错——静默挑一个会让「我明明写了另一个」无从解释。
func parseUserPassword(args []string) (user string, generate, readStdin bool, err error) {
	fs := flag.NewFlagSet("user password", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, userShortUsage) }
	userFlag := fs.String("user", "", "")
	stringAlias(fs, userFlag, "u")
	generateFlag := fs.Bool("generate", false, "")
	stdinFlag := fs.Bool("stdin", false, "")
	if err := fs.Parse(reorderUserPasswordArgs(args)); err != nil {
		return "", false, false, err
	}

	switch fs.NArg() {
	case 0:
	case 1:
		if *userFlag != "" {
			return "", false, false, errors.New("用户既出现在位置参数里，又给了 --user：只留一个")
		}
		*userFlag = fs.Arg(0)
	default:
		return "", false, false, fmt.Errorf(
			"password 只收一个用户标识，得到 %q 等 %d 个", fs.Arg(0), fs.NArg())
	}

	if strings.TrimSpace(*userFlag) == "" {
		return "", false, false, errors.New("缺少用户（ID 或用户名）：用 --user 或位置参数给出")
	}
	if *generateFlag == *stdinFlag {
		return "", false, false, errors.New("--generate 与 --stdin 必须给且只给一个")
	}
	return *userFlag, *generateFlag, *stdinFlag, nil
}

// reorderUserPasswordArgs 把位置参数挪到 flag 后面，再交给 flag 包。
//
// 为什么需要这一层：`flag` 包在**第一个非 flag 参数处停止解析**，此后的
// 一切都被当成位置参数。而帮助页写的顺序是
// `user password <ID 或用户名> --generate|--stdin`——照文档写，
// `--generate` 会落进位置参数里，于是报「只收一个用户标识」。
//
// 这一层的规则只有一条：本组里带值的 flag 只有 `--user` / `-u`，其余
// 一律当成布尔 flag 原样交给 `flag` 包（`--generate=false` 这种一整段也
// 原样留下）。三种边界要单独说：
//
//   - `--flag=value` 是一段，拆开会把 `value` 当成位置参数；
//   - 单独一个 `-` 是位置参数（命令行里它常表示标准输入），不是 flag；
//   - `--` 之后全部是位置参数，与 `flag` 包自己的约定一致。
//
// 这样帮助页写的顺序就是能用的顺序，`flag` 包认识的两种顺序也仍然都通。
func reorderUserPasswordArgs(args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			// `--` 之后的一切都是位置参数；`--` 本身不再交给 flag 包
			// （它已经被这一层消化掉了）。
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case arg == "-":
			positional = append(positional, arg)
		case arg == "--user" || arg == "-u":
			flags = append(flags, arg)
			// 带值 flag 的下一个词无论长什么样都是它的值，哪怕以 `-`
			// 开头——`flag` 包自己也是这么收的。没有下一个词就原样留下，
			// 让 flag 包报「需要参数」。
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		case strings.HasPrefix(arg, "--user="), strings.HasPrefix(arg, "-u="):
			flags = append(flags, arg)
		case strings.HasPrefix(arg, "-"):
			flags = append(flags, arg)
		default:
			positional = append(positional, arg)
		}
	}

	return append(flags, positional...)
}

// runUserUnlock 清掉一个账号的失败计数与锁定。
func runUserUnlock(args []string) error {
	if err := jsonUnsupported("user unlock"); err != nil {
		return err
	}

	fs := flag.NewFlagSet("user unlock", flag.ContinueOnError)
	fs.Usage = func() { printUsage(os.Stderr, userShortUsage) }
	user := fs.String("user", "", "")
	stringAlias(fs, user, "u")
	if err := fs.Parse(args); err != nil {
		return usageErr(err)
	}
	if fs.NArg() > 1 {
		return usageErr(fmt.Errorf("unlock 只收一个用户标识，得到 %d 个", fs.NArg()))
	}
	if fs.NArg() == 1 {
		if *user != "" {
			return usageErr(errors.New("用户既出现在位置参数里，又给了 --user：只留一个"))
		}
		*user = fs.Arg(0)
	}
	if strings.TrimSpace(*user) == "" {
		return usageErr(errors.New("缺少用户（ID 或用户名）：用 --user 或位置参数给出"))
	}

	return withDB(userTimeout, func(ctx context.Context, db *sql.DB) error {
		info, err := auth.New(db).UnlockUser(ctx, *user)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(humanStdout(), "已解锁用户 %s\n", info.Username)
		return nil
	})
}

// generatePassword 生成一个满足策略的随机口令。
func generatePassword() (string, error) {
	buf := make([]byte, generatedPasswordBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成口令失败（熵源不可用）: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// readPasswordInput 从标准输入读一个口令。
//
// 去掉行尾的换行（以及 Windows 的 CRLF）：`echo 'xxx' |` 是这条命令最自然的
// 用法，而把那个换行算进口令会让下一次登录永远对不上。行尾空白只去这一层，
// 口令中间的空白与开头的空白都原样保留。
func readPasswordInput() (string, error) {
	data, err := io.ReadAll(io.LimitReader(passwordInput, maxPasswordInput+1))
	if err != nil {
		return "", fmt.Errorf("读取标准输入失败: %w", err)
	}
	if len(data) > maxPasswordInput {
		return "", fmt.Errorf("标准输入超过 %d 字节：这里读的是一个口令，不是文件", maxPasswordInput)
	}
	password := strings.TrimRight(string(data), "\r\n")
	if password == "" {
		return "", errors.New("标准输入是空的")
	}
	return password, nil
}
