package adminapi

import (
	"net/http"
	"testing"
	"time"
)

// 本文件是 #1135 的返回值一侧：目录组与商家组同时列时，`models` 仍是全局
// 目录，`my-models` 只含自己名下在供的模型。路由形状与「装得起来吗」见
// groups_test.go（那份不连库）。

func TestModelsAndOverviewGroupsServeDifferentViews(t *testing.T) {
	const prefix = "/api/v1/merchant/"
	h := testSurface(t, Config{
		Prefix:    prefix,
		Authorize: func(next http.Handler) http.Handler { return next },
		// 渠道组一起列：商家那侧的真实装配（channels + pricing + overview）
		// 再加上目录组，正是会撞车的那种组合。建房渠道的夹具也走这一份装配。
		Groups: []string{GroupChannels, GroupModels, GroupOverview},
		Loc:    time.UTC,
	})
	db := testDB(t)
	owner := seedUser(t, db, "groups-owner-")
	other := seedUser(t, db, "groups-other-")

	mine := uniqueName("model-mine")
	theirs := uniqueName("model-theirs")
	for _, name := range []string{mine, theirs} {
		seedCatalogModel(t, db, name)
	}
	merchantChannel(t, h, owner, uniqueName("ch-owner"), "openai",
		"https://groups-owner.example.com/v1", []string{mine})
	merchantChannel(t, h, other, uniqueName("ch-other"), "openai",
		"https://groups-other.example.com/v1", []string{theirs})

	// 全局目录：目录里的两行都在，与谁在供、谁在看无关。
	rec := call(t, h, http.MethodGet, prefix+"models", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("全局目录状态码 = %d；正文：%s", rec.Code, rec.Body)
	}
	var catalog []struct {
		CanonicalName string `json:"canonical_name"`
	}
	decode(t, rec, &catalog)
	inCatalog := make(map[string]bool, len(catalog))
	for _, m := range catalog {
		inCatalog[m.CanonicalName] = true
	}
	if !inCatalog[mine] || !inCatalog[theirs] {
		t.Errorf("全局目录里有 %v，期望含 %q 与 %q", inCatalog, mine, theirs)
	}

	// 商家视图：只有自己名下在供的那一个，别人的那条不串进来。
	body := getModels(t, h, owner, "")
	if body.Total != 1 || len(body.Items) != 1 || body.Items[0].Model != mine {
		t.Errorf("商家视图 = %+v，期望只有 %q", body, mine)
	}
}
