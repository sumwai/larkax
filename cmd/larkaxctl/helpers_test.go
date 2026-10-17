package main

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/credential"
	"github.com/sumwai/larkax/internal/testdb"
)

// testDB 连上集成测试用的库，连不上就跳过。
//
// 这一层要测的是「命令行把字符串变成包要的结构、再把结果打出来」，而这条链
// 中间那一段（写入路径）只有真库才能证明：唯一键撞不撞、外键认不认、
// 写进去的值读出来还是不是同一个。拿内存假装这些，测的是「我以为数据库
// 会怎么做」。
//
// **没有 LARKAX_DB_DSN 时跳过而不是失败**：本机未必起着库。注意「跳过」与
// 「通过」在输出里长得一样，所以这个包也进了 CI 的那道集成测试门禁
// （见 .github/actions/check-integration-tests）。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
}

// TestMain 给这个包建一个独立的库，跑完删掉（见 internal/testdb）。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// testMasterKey 是一把形态上合法的假主密钥：32 个 k 的 base64。
//
// 长度必须对（AES-256 要 32 字节），取值一眼能看出是编的。真实主密钥不进
// 仓库，也不进测试——这里要证明的是「凭证被封装了、解回来是同一把」，
// 不是「这把钥匙有多强」。
func testMasterKey() string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", config.KeySize)))
}

// testSealer 造一个与命令用的同一把钥匙的封装器，用来把写进去的密文解回来。
func testSealer(t *testing.T) *credential.Sealer {
	t.Helper()
	sealer, err := credential.NewSealer([]config.MasterKey{
		{ID: "test", Key: []byte(strings.Repeat("k", config.KeySize))},
	})
	if err != nil {
		t.Fatalf("构造封装器失败: %v", err)
	}
	return sealer
}

// setupEnv 把命令要的环境变量准备好。
//
// 角色固定成 **worker**：那是不需要主密钥的角色，而 channel 的写入命令照样
// 要能跑——它读的是 LARKAX_MASTER_KEYS 本身，与角色无关。这一条在这里被
// 顺带钉住：如果哪天有人把命令改成只认 config.Load 解析好的那份密钥，
// 这个角色下就会失败。
func setupEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LARKAX_ROLE", string(config.RoleWorker))
	t.Setenv("LARKAX_MASTER_KEYS", "test:"+testMasterKey())
}

// 每个用例用独立的标识，避免彼此干扰。
//
// 用「时间戳 + 进程内计数」而不是纯计数：纯计数在同一个库上跑第二次
// `go test` 时会从头开始，撞上上一次留下的行。
var seq struct {
	sync.Mutex
	n int64
}

var base = time.Now().UnixNano() % 1_000_000_000

func uniqueName(prefix string) string {
	seq.Lock()
	defer seq.Unlock()
	seq.n++
	return fmt.Sprintf("%s-%d-%d", prefix, base, seq.n)
}

// createTestUser 建一个用户并返回它的 ID。
//
// 名字每个用例各不相同：user create 不做幂等，重名会直接报错。
func createTestUser(t *testing.T) int64 {
	t.Helper()
	stdout, _ := captureStreams(t, func() {
		if err := runUserCreate([]string{"--name", uniqueName("user")}); err != nil {
			t.Fatalf("user create 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是用户 ID：%q（%v）", stdout, err)
	}
	return id
}

// captureStreams 捕获函数执行期间写到 os.Stdout 与 os.Stderr 的内容。
//
// 命令把结果打到标准输出/标准错误上，所以「打了什么」只有把这两个流接住
// 才断言得了——而「stdout 只有能被接走的那一行」「list 里没有密文」都是
// 契约，不是排版偏好，值得被断言。
func captureStreams(t *testing.T, fn func()) (string, string) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("建 stdout 管道失败: %v", err)
	}
	errR, errW, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatalf("建 stderr 管道失败: %v", pipeErr)
	}

	savedOut, savedErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	done := make(chan [2]string, 1)
	go func() {
		var out, errOut strings.Builder
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = io.Copy(&out, outR) }()
		go func() { defer wg.Done(); _, _ = io.Copy(&errOut, errR) }()
		wg.Wait()
		done <- [2]string{out.String(), errOut.String()}
	}()

	fn()

	os.Stdout, os.Stderr = savedOut, savedErr
	_ = outW.Close()
	_ = errW.Close()
	captured := <-done
	_ = outR.Close()
	_ = errR.Close()
	return captured[0], captured[1]
}

// runCaptured 执行一条命令，吞掉它的输出，只返回错误。
//
// 用例关心的是错误与退出码时用它：命令正常打出来的清单会混进测试输出，
// 而那些行会把真正要看的东西挤走。
func runCaptured(t *testing.T, fn func() error) error {
	t.Helper()
	var err error
	_, _ = captureStreams(t, func() { err = fn() })
	return err
}

// withStdin 把「标准输入」换成给定的内容，用完还原。
//
// 凭证只能从标准输入或环境变量读，所以这条路径必须真的被测到——
// 它是这条命令的一条契约，不能只靠人工敲一次。
func withStdin(t *testing.T, content string) {
	t.Helper()
	saved := credentialInput
	credentialInput = strings.NewReader(content)
	t.Cleanup(func() { credentialInput = saved })
}

// withOSStdin 把 os.Stdin 换成给定的内容，用完还原。
//
// 与 withStdin 分开：那个换的是凭证的注入点，这个是**进程真正的标准输入**。
// 走 os.Stdin 的命令（snapshot redact 从那里读报文）只有把标准输入真的换掉，
// 「读到的到底是不是那段内容」才断言得了。
//
// 内容在命令跑之前就写完并关掉写端：用例里的报文都很小，装得进管道缓冲区，
// 所以不会在「没人读」的时候把自己堵住。
func withOSStdin(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建标准输入管道失败: %v", err)
	}
	if _, err := io.WriteString(w, content); err != nil {
		t.Fatalf("写标准输入失败: %v", err)
	}
	_ = w.Close()

	saved := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = saved
		_ = r.Close()
	})
}

// showPtr 把一个可空值渲染成可读文本，供断言失败时打印。
//
// 直接 %v 一个指向基本类型的指针打出来的是地址（`0xc000...`），而报错信息
// 本身是要给人读的——那串地址看不出实际值是什么，也就看不出这次失败是
// 「值算错了」还是「字段根本没给」。
func showPtr[T any](p *T) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}
