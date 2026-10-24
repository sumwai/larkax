package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// TestPriceTierTakesPositionalLocator 钉住 price tier 的三条命令都从第一个
// 位置参数拿规则名或 ID。
//
// 这组命令没有同名的旗标（没有 --id），定位符只能写成位置参数。收拢逻辑落在
// 共用的 operandSpec 上，三条命令任一处没接上，add / remove 会当场报「缺少
// 定位符」或把规则名当成多余参数——而它们平时没有用例走到，光看编译是看不出来的。
func TestPriceTierTakesPositionalLocator(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	by := createTestUserID(t)
	id := addPriceRule(t, []string{
		"--scope", "platform", "--name", uniqueName("tier"),
		"--models", uniqueName("tier-model"),
		"--schedule-file", writeScheduleFile(t, peakScheduleBase),
	})
	idArg := strconv.FormatInt(id, 10)

	// add：定位符写在最前面，段号打到 stdout。
	t.Run("add", func(t *testing.T) {
		var err error
		stdout, stderr := captureStreams(t, func() {
			err = runPriceTier([]string{"add", idArg, "--above", "50000", "--in", "0.5", "--by", by})
		})
		if err != nil {
			t.Fatalf("price tier add 失败: %v（stderr=%q）", err, stderr)
		}
		if got := strings.TrimSpace(stdout); got != "1" {
			t.Errorf("add 打出的段号 = %q，期望 1", got)
		}
	})

	// list：段号在第一列（机器可读档里是 segment），读回来确认上面那一段在。
	t.Run("list", func(t *testing.T) {
		withFormat(t, formatJSON)
		var err error
		stdout, _ := captureStreams(t, func() { err = runPriceTier([]string{"list", idArg}) })
		if err != nil {
			t.Fatalf("price tier list 失败: %v", err)
		}
		var doc struct {
			Tiers []struct {
				Segment int   `json:"segment"`
				Above   int64 `json:"above"`
			} `json:"tiers"`
		}
		if jsonErr := json.Unmarshal([]byte(stdout), &doc); jsonErr != nil {
			t.Fatalf("price tier list 的 JSON 解不开: %v\n%s", jsonErr, stdout)
		}
		if len(doc.Tiers) != 1 || doc.Tiers[0].Segment != 1 || doc.Tiers[0].Above != 50000 {
			t.Errorf("tier list 那一段 = %+v，期望段号 1、门槛 50000", doc.Tiers)
		}
	})

	// remove：规则名与段号都写位置参数，删完再看一眼清单。
	t.Run("remove", func(t *testing.T) {
		var err error
		_, _ = captureStreams(t, func() {
			err = runPriceTier([]string{"remove", idArg, "1", "--by", by})
		})
		if err != nil {
			t.Fatalf("price tier remove 失败: %v", err)
		}
		schedule := currentSchedule(t, db, id)
		if len(schedule.Overrides) != 0 {
			t.Errorf("删掉唯一一段之后还剩 %d 段覆盖，期望 0 段", len(schedule.Overrides))
		}
	})
}
