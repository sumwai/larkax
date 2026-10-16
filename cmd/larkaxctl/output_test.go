package main

import (
	"os"
	"strings"
	"testing"
)

// captureStdout 捕获函数执行期间写到标准输出的内容。
//
// printPartitions 的输出是这条命令唯一的正向证据（删了哪些分区），
// 所以它值得被断言，而不是靠人看一眼终端。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("建管道失败: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()

	fn()

	os.Stdout = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}
