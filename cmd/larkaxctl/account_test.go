package main

import (
	"context"
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/channels"
)

// TestAccountAddResolvesChannelByName 钉住 `account add --channel` 认名字。
//
// 它是 channels.Admin.ChannelIDByRef 的既有调用点：channel map / unmap 也走
// 同一个函数，所以这条同时是「名字解析在这条老路径上没有退化」的证据。
// 落空那一半断两件事：
//
//   - 错误链里仍有 channels.ErrChannelNotFound —— exit.go 按这个哨兵把它归进
//     「约束冲突」那一档，包装丢了会静默改掉退出码；
//   - exitCode 是**字面量 4**。不从 exitConflict 常量取期望值：那是拿被测代码
//     自己的表去比自己。
func TestAccountAddResolvesChannelByName(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	channelName := uniqueName("account-channel")
	channelID := createMapChannel(t, channelName, uniqueName("seed-model"))
	accountName := uniqueName("account")

	withStdin(t, testSecret)
	if err := runCaptured(t, func() error {
		return runAccount([]string{"add", accountName, "--channel", channelName})
	}); err != nil {
		t.Fatalf("account add --channel <名字> 失败: %v", err)
	}

	accounts, err := channels.NewAdmin(db, nil, nil).ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("列举账号失败: %v", err)
	}
	var channelIDs []int64
	found := false
	for _, account := range accounts {
		if account.Name != accountName {
			continue
		}
		found = true
		for _, ch := range account.Channels {
			channelIDs = append(channelIDs, ch.ID)
		}
	}
	if !found {
		t.Fatalf("账号列表里没有刚建的 %q", accountName)
	}
	if len(channelIDs) != 1 || channelIDs[0] != channelID {
		t.Fatalf("账号 %q 挂的渠道 = %v，期望 [%d]", accountName, channelIDs, channelID)
	}

	withStdin(t, testSecret)
	var addErr error
	_, _ = captureStreams(t, func() {
		addErr = runAccount([]string{"add", uniqueName("account"), "--channel", uniqueName("no-such-channel")})
	})
	if addErr == nil {
		t.Fatal("不存在的渠道名应当被拒")
	}
	if !errors.Is(addErr, channels.ErrChannelNotFound) {
		t.Fatalf("错误链里没有 channels.ErrChannelNotFound：%v", addErr)
	}
	if got := exitCode(addErr); got != 4 {
		t.Fatalf("渠道名找不到的退出码 = %d，期望 4（%v）", got, addErr)
	}
}
