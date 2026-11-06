package main

import (
	"strings"
	"testing"
	"time"
)

func goodInput() validityInput {
	return validityInput{
		Measured: 3000, LagP99: 5 * time.Millisecond, LagThreshold: 50 * time.Millisecond,
		ToolCPUCores: 0.5, ToolCPULimitCPU: 6, ViewersTarget: 100, ViewersJoined: 100,
	}
}

func TestEvaluateValidity(t *testing.T) {
	if r := evaluateValidity(goodInput()); len(r) != 0 {
		t.Fatalf("healthy step reasons = %v, want none", r)
	}

	cases := []struct {
		name   string
		mutate func(*validityInput)
		want   string
	}{
		{"lag p99 over threshold", func(in *validityInput) { in.LagP99 = 51 * time.Millisecond }, "scheduling lag"},
		{"any dropped send", func(in *validityInput) { in.Dropped = 1 }, "dropped"},
		{"tool cpu saturated", func(in *validityInput) { in.ToolCPUCores = 6 }, "tool CPU"},
		{"429 seen", func(in *validityInput) { in.RateLimited = 1 }, "429"},
		{"viewers short", func(in *validityInput) { in.ViewersJoined = 99 }, "viewers"},
		{"nothing measured", func(in *validityInput) { in.Measured = 0 }, "no sends"},
	}
	for _, tc := range cases {
		in := goodInput()
		tc.mutate(&in)
		reasons := evaluateValidity(in)
		if len(reasons) != 1 || !strings.Contains(reasons[0], tc.want) {
			t.Errorf("%s: reasons = %v, want exactly one containing %q", tc.name, reasons, tc.want)
		}
	}

	// 邊界：剛好等於門檻不算超過
	in := goodInput()
	in.LagP99 = 50 * time.Millisecond
	if r := evaluateValidity(in); len(r) != 0 {
		t.Errorf("lag exactly at threshold reasons = %v, want none", r)
	}

	// 多個原因都要列出來，不能只回第一個
	in = goodInput()
	in.Dropped, in.LagP99, in.RateLimited = 5, time.Second, 2
	if r := evaluateValidity(in); len(r) != 3 {
		t.Errorf("reasons = %v, want 3", r)
	}
}

func TestEvaluateCollapse(t *testing.T) {
	ok := collapseInput{Measured: 1000, Succeeded: 1000, PostP99: 20 * time.Millisecond, P99Limit: 2 * time.Second,
		SampleExpect: 4000, SampleMissing: 0, ViewersJoined: 100}
	if c, why := evaluateCollapse(ok); c {
		t.Fatalf("healthy step collapsed: %s", why)
	}
	for name, mutate := range map[string]func(*collapseInput){
		"success rate below 90%":   func(in *collapseInput) { in.Succeeded = 899 },
		"POST p99 over the limit":  func(in *collapseInput) { in.PostP99 = 3 * time.Second },
		"more than 10% missing":    func(in *collapseInput) { in.SampleMissing = 401 },
		"more than 10% disconnect": func(in *collapseInput) { in.Disconnects = 11 },
	} {
		in := ok
		mutate(&in)
		if c, _ := evaluateCollapse(in); !c {
			t.Errorf("%s: not reported as collapsed", name)
		}
	}
	in := ok
	in.Succeeded = 900
	if c, _ := evaluateCollapse(in); c {
		t.Error("exactly 90% success should not count as collapsed")
	}
}

// 一個 step 的驗證規則有確實接到結果上：被中斷的不算 valid、也不判成崩潰
func TestFinishStepAppliesRules(t *testing.T) {
	cfg := runnerConfig{LagThreshold: 50 * time.Millisecond, CollapseP99: 2 * time.Second}
	res := stepResult{Status: "ran"}
	res.Send.Measured, res.Send.Created, res.Send.Dropped = 100, 100, 3
	res.Viewers.Target, res.Viewers.AliveAtStart = 10, 10
	got := finishStep(res, cfg)
	if got.Valid || len(got.InvalidReasons) != 1 {
		t.Errorf("valid = %v reasons = %v, want invalid because of dropped sends", got.Valid, got.InvalidReasons)
	}

	res.Send.Dropped = 0
	if got := finishStep(res, cfg); !got.Valid {
		t.Errorf("clean step reasons = %v, want valid", got.InvalidReasons)
	}

	res.Status = "aborted"
	res.Send.Created = 10 // 只成功 10%：被中斷的不能被判成崩潰
	got = finishStep(res, cfg)
	if got.Valid || got.Collapsed {
		t.Errorf("aborted step valid=%v collapsed=%v, want invalid and not collapsed", got.Valid, got.Collapsed)
	}
}

// 錯誤 + 非 201 達量測筆數 1% → 不可信（POST p99 的樣本只有下限 / 被快速失敗拉低）；差一筆就不算
func TestEvaluateValidityFailedFraction(t *testing.T) {
	in := goodInput() // Measured = 3000 → 門檻 30 筆
	in.Failed = 29
	if r := evaluateValidity(in); len(r) != 0 {
		t.Errorf("29 of 3000 failed: reasons = %v, want none", r)
	}
	in.Failed = 30
	r := evaluateValidity(in)
	if len(r) != 1 || !strings.Contains(r[0], "not trustworthy") {
		t.Errorf("30 of 3000 failed: reasons = %v, want one 'not trustworthy' reason", r)
	}
}

// 整台機器飽和：>= 85% NumCPU 無效；判斷不了的時候不能當成飽和，也不能當成沒飽和而默默通過
func TestEvaluateValiditySaturation(t *testing.T) {
	in := goodInput()
	in.MachineNumCPU, in.MachineJudged = 10, true
	in.MachineCores = 8.4
	if r := evaluateValidity(in); len(r) != 0 {
		t.Errorf("8.4 of 10 cores: reasons = %v, want none", r)
	}
	in.MachineCores = 8.5
	r := evaluateValidity(in)
	if len(r) != 1 || !strings.Contains(r[0], "whole machine busy") {
		t.Errorf("8.5 of 10 cores: reasons = %v, want the saturation reason", r)
	}
	in.MachineJudged = false // 資料不足：不判斷
	if r := evaluateValidity(in); len(r) != 0 {
		t.Errorf("not judged but invalid: %v", r)
	}
}

func TestMachineLoad(t *testing.T) {
	full := resourceWindow{
		Servers:    map[string]procCPU{"1": {AvgCores: 1.5}, "2": {AvgCores: 1}},
		Containers: map[string]containerCPU{"db": {AvgPct: 200, Samples: 3}, "redis": {AvgPct: 50, Samples: 2}},
	}
	cores, judged, why := machineLoad(0.5, full, 2, 2)
	if !judged || why != "" || cores < 5.49 || cores > 5.51 { // 0.5 + 2.5 + 2.5
		t.Errorf("full = %.2f %v %q, want 5.50 judged", cores, judged, why)
	}

	cases := []struct {
		name           string
		r              resourceWindow
		servers, conts int
		want           string
	}{
		{"no -server-pids", resourceWindow{Containers: full.Containers}, 0, 2, "no -server-pids"},
		{"a server missing", resourceWindow{Servers: map[string]procCPU{"1": {}}, Containers: full.Containers}, 2, 2, "server processes"},
		{"docker off", resourceWindow{Servers: full.Servers}, 2, 0, "docker stats is off"},
		{"a container missing", resourceWindow{Servers: full.Servers, Containers: map[string]containerCPU{"db": {Samples: 5}}}, 2, 2, "only 1 of 2 containers"},
		{"too few docker samples", resourceWindow{Servers: full.Servers, Containers: map[string]containerCPU{"db": {Samples: 5}, "redis": {Samples: 1}}}, 2, 2, "too few docker stats samples"},
	}
	for _, tc := range cases {
		_, judged, why := machineLoad(0.5, tc.r, tc.servers, tc.conts)
		if judged || !strings.Contains(why, tc.want) {
			t.Errorf("%s: judged = %v why = %q, want not judged and %q", tc.name, judged, why, tc.want)
		}
	}
}

func TestExpectedFrames(t *testing.T) {
	if got := expectedFrames(100, 10, 1); got != 1020 { // 100*10 + 100/5
		t.Errorf("expectedFrames(100,10,1) = %v, want 1020", got)
	}
	// 多房間：每則訊息只送給自己房間的 viewer
	if got := expectedFrames(5000, 500, 500); got != 6000 { // 5000*500/500 + 1000
		t.Errorf("expectedFrames(5000,500,500) = %v, want 6000", got)
	}
}

// 新規則有接到 step 結果上：失敗比例、機器飽和、判斷不了的警告、frames 預期值
func TestFinishStepNewRules(t *testing.T) {
	cfg := runnerConfig{LagThreshold: 50 * time.Millisecond, CollapseP99: 2 * time.Second, NumCPU: 8, ServerPIDs: 1, DockerTargets: 1}
	base := stepResult{Status: "ran", Spec: stepSpec{Viewers: 100, Rate: 10, Rooms: 1}}
	base.Send.Measured, base.Send.Responses, base.Send.Created = 1000, 1000, 1000
	base.Viewers.Target, base.Viewers.AliveAtStart = 100, 100
	base.CPU.ToolCores = 0.5
	base.CPU.Resources = resourceWindow{
		Servers:    map[string]procCPU{"1": {AvgCores: 1}},
		Containers: map[string]containerCPU{"db": {AvgPct: 100, Samples: 3}},
	}
	base.Frames.PerSec = 510

	got := finishStep(base, cfg)
	if !got.Valid || !got.CPU.MachineJudged || got.CPU.MachineCores < 2.49 || got.CPU.MachineCores > 2.51 {
		t.Errorf("healthy: valid = %v judged = %v cores = %.2f reasons = %v", got.Valid, got.CPU.MachineJudged, got.CPU.MachineCores, got.InvalidReasons)
	}
	if got.Frames.Expected != 1020 || got.Frames.Ratio != 0.5 {
		t.Errorf("frames expected/ratio = %v/%v, want 1020/0.5", got.Frames.Expected, got.Frames.Ratio)
	}

	failing := base
	failing.Send.Errors, failing.Send.Responses, failing.Send.Created = 10, 990, 990 // 1%
	if got := finishStep(failing, cfg); got.Valid {
		t.Error("1% errors: step still valid")
	}
	failing.Send.Errors, failing.Send.Responses, failing.Send.Created = 0, 1000, 990 // 1% 非 201（例如 500）
	if got := finishStep(failing, cfg); got.Valid {
		t.Error("1% non-201 responses: step still valid")
	}

	busy := base
	busy.CPU.ToolCores = 5.8 // 5.8 + 1 + 1 = 7.8 >= 0.85 * 8
	if got := finishStep(busy, cfg); got.Valid {
		t.Errorf("machine at %.2f of 8 cores: step still valid", got.CPU.MachineCores)
	}

	noPIDs := base
	noPIDs.CPU.Resources = resourceWindow{Containers: base.CPU.Resources.Containers}
	cfgNoPIDs := cfg
	cfgNoPIDs.ServerPIDs = 0
	got = finishStep(noPIDs, cfgNoPIDs)
	if !got.Valid || got.CPU.MachineJudged || got.CPU.MachineNote == "" {
		t.Errorf("no pids: valid = %v judged = %v note = %q, want valid but explicitly not judged", got.Valid, got.CPU.MachineJudged, got.CPU.MachineNote)
	}
	found := false
	for _, w := range got.Warnings {
		if strings.Contains(w, "could NOT be judged") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want a 'could NOT be judged' warning", got.Warnings)
	}
}

// 整機總和與其他程序：用假讀數驗證規則
func TestMachineWide(t *testing.T) {
	r := resourceWindow{MachineTotalCores: 4, MachineSamples: 3}
	total, other, tj, oj, why := machineWide(r, 2.5, true)
	if !tj || !oj || why != "" || total != 4 || other != 1.5 {
		t.Errorf("ok = %v %v %v %v %q, want 4 / 1.5 judged", total, other, tj, oj, why)
	}
	// 減出負數（ps 低估）→ 當 0
	if _, other, _, oj, _ := machineWide(r, 5, true); !oj || other != 0 {
		t.Errorf("negative other = %v judged %v, want 0", other, oj)
	}
	// 已量到的部分不完整：總和可判斷，other 不行
	if _, _, tj, oj, why := machineWide(r, 2.5, false); !tj || oj || why == "" {
		t.Errorf("unmeasured parts: total judged %v other judged %v why %q", tj, oj, why)
	}
	// 樣本不足 / 取樣失敗：都判斷不了，並說明原因
	if _, _, tj, oj, why := machineWide(resourceWindow{MachineSamples: 1}, 1, true); tj || oj || !strings.Contains(why, "ps -A") {
		t.Errorf("too few samples: %v %v %q", tj, oj, why)
	}
}

func TestEvaluateValidityOtherPrograms(t *testing.T) {
	in := goodInput()
	in.MachineNumCPU, in.OtherLimit = 10, 1.0
	in.TotalJudged, in.OtherJudged = true, true
	in.TotalCores, in.OtherCores = 8.4, 0.99
	if r := evaluateValidity(in); len(r) != 0 {
		t.Errorf("total 8.4/10, other 0.99: reasons = %v, want none", r)
	}
	in.TotalCores = 8.5
	if r := evaluateValidity(in); len(r) != 1 || !strings.Contains(r[0], "machine not idle") {
		t.Errorf("total 8.5/10: reasons = %v", r)
	}
	in.TotalCores, in.OtherCores = 3, 1.0
	if r := evaluateValidity(in); len(r) != 1 || !strings.Contains(r[0], "other programs") {
		t.Errorf("other 1.0: reasons = %v", r)
	}
	in.OtherLimit = 0 // 關掉 other 檢查
	if r := evaluateValidity(in); len(r) != 0 {
		t.Errorf("limit off: reasons = %v", r)
	}
	in.OtherLimit = 1
	in.TotalJudged, in.OtherJudged = false, false // 判斷不了：不默默擋，也不默默通過（finishStep 會給警告）
	in.TotalCores, in.OtherCores = 9, 5
	if r := evaluateValidity(in); len(r) != 0 {
		t.Errorf("not judged: reasons = %v", r)
	}
	// 要求整機檢查時，判斷不了本身就是無效原因
	in.RequireMachineCheck, in.JudgeNote = true, "no -server-pids given"
	if r := evaluateValidity(in); len(r) != 1 || !strings.Contains(r[0], "could not be judged") {
		t.Errorf("require-machine-check with nothing judged: reasons = %v, want 1 invalid reason", r)
	}
	in.TotalJudged, in.TotalCores = true, 3 // 只有 other 判斷不了也算
	if r := evaluateValidity(in); len(r) != 1 {
		t.Errorf("other not judged: reasons = %v, want 1", r)
	}
	in.OtherLimit = 0 // other 檢查關掉就不要求 other
	if r := evaluateValidity(in); len(r) != 0 {
		t.Errorf("other check off: reasons = %v, want none", r)
	}
}

func TestFinishStepMachineWide(t *testing.T) {
	cfg := runnerConfig{NumCPU: 8, ServerPIDs: 1, DockerTargets: 1, OtherCoresLimit: 1}
	base := stepResult{Status: "ran", Spec: stepSpec{Viewers: 100, Rate: 10, Rooms: 1}}
	base.Send.Measured, base.Send.Responses, base.Send.Created = 1000, 1000, 1000
	base.Viewers.Target, base.Viewers.AliveAtStart = 100, 100
	base.CPU.ToolCores = 0.5
	base.CPU.Resources = resourceWindow{
		Servers:    map[string]procCPU{"1": {AvgCores: 1}},
		Containers: map[string]containerCPU{"db": {AvgPct: 100, Samples: 3}},
		// 量到 2.5 core，總和 3 → other 0.5
		MachineTotalCores: 3, MachineSamples: 4,
	}
	got := finishStep(base, cfg)
	if !got.Valid || !got.CPU.OtherJudged || got.CPU.OtherCores != 0.5 {
		t.Errorf("quiet: valid %v other %v judged %v reasons %v", got.Valid, got.CPU.OtherCores, got.CPU.OtherJudged, got.InvalidReasons)
	}
	noisy := base
	noisy.CPU.Resources.MachineTotalCores = 4 // other 1.5
	if got := finishStep(noisy, cfg); got.Valid {
		t.Error("other 1.5 cores: step still valid")
	}
	failed := base
	failed.CPU.Resources.MachineSamples = 0 // 取樣失敗
	got = finishStep(failed, cfg)
	warned := false
	for _, w := range got.Warnings {
		warned = warned || strings.Contains(w, "idleness") && strings.Contains(w, "could NOT be judged")
	}
	if !got.Valid || got.CPU.TotalJudged || !warned {
		t.Errorf("sampling failed (check off): valid %v judged %v warnings %v, want valid-by-rule but an explicit warning", got.Valid, got.CPU.TotalJudged, got.Warnings)
	}
	// 同一個取樣失敗，-require-machine-check 開著 → 無效，且原因寫明判斷不了；警告仍在
	strict := cfg
	strict.RequireMachineCheck = true
	got = finishStep(failed, strict)
	reasoned := false
	for _, r := range got.InvalidReasons {
		reasoned = reasoned || strings.Contains(r, "could not be judged")
	}
	if got.Valid || !reasoned {
		t.Errorf("sampling failed (check on): valid %v reasons %v, want invalid with a 'could not be judged' reason", got.Valid, got.InvalidReasons)
	}
	// 沒給 -server-pids（other 判斷不了）：檢查開著一樣無效
	noPIDs := base
	noPIDs.CPU.Resources.Servers = nil
	strictNoPIDs := strict
	strictNoPIDs.ServerPIDs = 0
	if got := finishStep(noPIDs, strictNoPIDs); got.Valid {
		t.Errorf("no server pids (check on): valid, reasons %v", got.InvalidReasons)
	}
	// 判斷得了的正常 step 在檢查開著時仍然 valid
	if got := finishStep(base, strict); !got.Valid {
		t.Errorf("quiet step (check on): reasons %v, want valid", got.InvalidReasons)
	}
}

func TestParsePSAllAndIdleWarning(t *testing.T) {
	s, err := parsePSAll("  1  10.0\n  2   5.5\n  3 100.0\nbad line\n")
	if err != nil || s.TotalPct != 115.5 || s.ByPID[3] != 100 {
		t.Fatalf("parse = %+v %v", s, err)
	}
	if _, err := parsePSAll("garbage\n"); err == nil {
		t.Error("garbage parsed without error")
	}
	// pid 3 是工具 / server：扣掉後 other = 0.155
	if w := idleWarningFor(s, []int{3}, 8, 1); w != "" {
		t.Errorf("idle machine warned: %q", w)
	}
	if w := idleWarningFor(s, []int{3}, 8, 0.1); !strings.Contains(w, "other programs") {
		t.Errorf("busy others not warned: %q", w)
	}
	if w := idleWarningFor(s, []int{3}, 1, 1); !strings.Contains(w, "all processes use") {
		t.Errorf("saturated total not warned: %q", w)
	}
}
