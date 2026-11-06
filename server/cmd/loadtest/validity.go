package main

import (
	"fmt"
	"time"
)

// 已知限制（F3，只是註記，門檻沒有因此調整）：
//   - ps -A 沒有 pid 0，kernel_task 的 CPU 不在總和裡，所以 total / other 是下限
//   - 核心是 4P+4E，8 顆不等價，85% 的飽和門檻偏鬆
//   - Docker VM 的開銷會落在 other（docker stats 只算 container 內的部分）
//   - defaultOtherCoresLimit = 1.0 還沒校準：要在閒置的 Mac 上量過 other 的底噪再定
//
// 一個 step 能不能信的判斷：只看數字，不碰 I/O，方便測試。
// 原則：資料不好也照樣輸出，只是標 valid=false 並寫明原因，絕不隱藏。

type validityInput struct {
	Measured int // 量測期間排程的送出數（不含暖機）

	LagP99       time.Duration // 工具自己的排程延遲 p99
	LagThreshold time.Duration

	Dropped     int // in-flight 上限造成的 dropped（量測期間）
	RateLimited int // 429 的數量（限流介入了）

	ToolCPUCores    float64 // 量測期間工具程序平均用掉幾顆 core
	ToolCPULimitCPU float64 // 超過這個就當作工具自己 CPU 飽和；<= 0 = 不檢查

	ViewersTarget int
	ViewersJoined int // 量測開始時實際在房間裡的連線數（含 sampled）

	// 錯誤（timeout / 傳輸錯誤）加上非 201 的回應。達 failedFracLimit 時 POST p99 不可信：
	// 錯誤只有下限延遲、非 201（例如快速的 5xx）會把 p99 拉得比實際好看
	Failed int

	// 整台機器的 CPU：工具 + server + container 的 core 數加總。MachineJudged=false = 資料不足、判斷不了（見 machineLoad），
	// 此時既不算飽和也不算沒飽和，由呼叫端另外寫警告
	MachineCores  float64
	MachineNumCPU int
	MachineJudged bool

	// 整機所有程序（ps -A）的 CPU 總和與「別人」用掉的 core 數（= 總和 - 工具/server/container）。
	// TotalJudged / OtherJudged = false 時不判斷（由呼叫端另外寫警告）。OtherLimit <= 0 = 不檢查 other
	TotalCores  float64
	TotalJudged bool
	OtherCores  float64
	OtherJudged bool
	OtherLimit  float64

	// 要求整機檢查必須做得成：TotalJudged / OtherJudged 判斷不了時，這個 step 直接無效（而不是只警告）。
	// JudgeNote = 判斷不了的原因，寫進 invalid reason
	RequireMachineCheck bool
	JudgeNote           string
}

const (
	// 錯誤 + 非 201 佔量測筆數的比例達這個值，這個 step 的 POST p99 就不可信
	failedFracLimit = 0.01
	// 整台機器 CPU 用掉 NumCPU 的這個比例以上就當作飽和（工具和 server 搶同一台機器，數字不再代表 server）
	machineSaturationFrac = 0.85
	// 整機（ps -A 總和）至少要有幾個樣本才算數
	minMachineSamples = 2
	// 其他程序用掉的 core 數預設門檻（baseline 要在閒置的機器上量）；可用 -other-cores-limit 調整
	defaultOtherCoresLimit = 1.0
	// 每個 container 在窗口內至少要有幾個 docker stats 樣本才算數
	minDockerSamples = 2
)

// 整台機器用掉幾顆 core = 工具 + 所有 server 程序 + 所有 container。
// 只有每一項都量到才判斷得了：沒給 -server-pids、ps 樣本不足、docker 沒開或樣本太少，都回 judged=false 並寫明原因。
// 沒被量到的程序（瀏覽器、其他 app）不在總和裡，所以總和是下限。docker 的值是窗口內幾個瞬間樣本的平均，不是精確的區間平均
func machineLoad(toolCores float64, r resourceWindow, wantServers, wantContainers int) (cores float64, judged bool, why string) {
	cores = toolCores
	for _, s := range r.Servers {
		cores += s.AvgCores
	}
	minSamples := -1
	for _, c := range r.Containers {
		cores += c.AvgPct / 100
		if minSamples < 0 || c.Samples < minSamples {
			minSamples = c.Samples
		}
	}
	switch {
	case wantServers == 0:
		why = "no -server-pids given, server CPU is unknown"
	case len(r.Servers) < wantServers:
		why = fmt.Sprintf("only %d of %d server processes had enough ps samples", len(r.Servers), wantServers)
	case wantContainers == 0:
		why = "docker stats is off, database / redis CPU is unknown"
	case len(r.Containers) < wantContainers:
		why = fmt.Sprintf("docker stats had samples for only %d of %d containers", len(r.Containers), wantContainers)
	case minSamples < minDockerSamples:
		why = fmt.Sprintf("too few docker stats samples in the window (%d, need at least %d)", minSamples, minDockerSamples)
	default:
		judged = true
	}
	return cores, judged, why
}

// 整機總 CPU（ps -A）與「其他程序」的 core 數 = 總和 - 已量到的（工具 + server + container）。
// ps %cpu 是粗略值（見 machineSample），且 docker VM 程序的 CPU 已包含 container 的部分，所以 other 是約略值；
// 減出負數時當 0（代表量測誤差，不是真的負載）。樣本不足 / 取樣失敗 → totalJudged=false；已量到的部分判斷不了 → otherJudged=false
func machineWide(r resourceWindow, measured float64, measuredJudged bool) (total, other float64, totalJudged, otherJudged bool, why string) {
	if r.MachineSamples < minMachineSamples {
		return 0, 0, false, false, fmt.Sprintf("machine-wide ps -A sampling had %d samples in the window (need at least %d), other programs' CPU is unknown", r.MachineSamples, minMachineSamples)
	}
	total = r.MachineTotalCores
	if !measuredJudged {
		return total, 0, true, false, "other programs' CPU cannot be derived because tool + servers + containers were not fully measured"
	}
	return total, max(total-measured, 0), true, true, ""
}

// 這個 step 預期每秒每個 viewer 應該收到幾個 frame 的總和：每則訊息送給房間內的每個 viewer（viewers*rate，
// rooms > 1 時 viewers 平均分在各房間，一則只送給一個房間，所以要除以房間數），再加每 viewer 每 5 秒一則（presence 等，viewers/5）的底噪。
// 公式是估計，用來和實際的 frames/s 比，差很多 = 有人沒收到或多收了
func expectedFrames(viewers int, rate float64, rooms int) float64 {
	return float64(viewers)*rate/float64(max(rooms, 1)) + float64(viewers)/5
}

// 回傳所有不成立的原因；空 = valid
func evaluateValidity(in validityInput) []string {
	var reasons []string
	if in.Measured == 0 {
		reasons = append(reasons, "no sends were scheduled in the measured window")
	}
	if in.LagThreshold > 0 && in.LagP99 > in.LagThreshold {
		reasons = append(reasons, fmt.Sprintf("tool scheduling lag p99 %.1fms exceeds threshold %.1fms (the tool could not keep its own schedule)",
			durMs(in.LagP99), durMs(in.LagThreshold)))
	}
	if in.Dropped > 0 {
		reasons = append(reasons, fmt.Sprintf("%d sends dropped by the in-flight cap (offered load was not actually offered)", in.Dropped))
	}
	if in.ToolCPULimitCPU > 0 && in.ToolCPUCores >= in.ToolCPULimitCPU {
		reasons = append(reasons, fmt.Sprintf("tool CPU %.2f cores reached the saturation limit %.2f cores", in.ToolCPUCores, in.ToolCPULimitCPU))
	}
	if in.RateLimited > 0 {
		reasons = append(reasons, fmt.Sprintf("%d responses were 429: the rate limiter interfered with the measurement", in.RateLimited))
	}
	if in.Measured > 0 && float64(in.Failed) >= failedFracLimit*float64(in.Measured) {
		reasons = append(reasons, fmt.Sprintf("%d of %d sends were errors (timeout / transport) or non-201 responses (>= %.0f%%): the POST p99 is not trustworthy (errors only count as lower-bound latencies)",
			in.Failed, in.Measured, failedFracLimit*100))
	}
	if in.MachineJudged && in.MachineNumCPU > 0 && in.MachineCores >= machineSaturationFrac*float64(in.MachineNumCPU) {
		reasons = append(reasons, fmt.Sprintf("whole machine busy: tool + servers + containers used %.2f of %d cores (>= %.0f%%): the tool and the server were competing for CPU",
			in.MachineCores, in.MachineNumCPU, machineSaturationFrac*100))
	}
	if in.TotalJudged && in.MachineNumCPU > 0 && in.TotalCores >= machineSaturationFrac*float64(in.MachineNumCPU) {
		reasons = append(reasons, fmt.Sprintf("machine not idle: all processes together used %.2f of %d cores (>= %.0f%%, ps -A estimate, includes the tool and servers): the baseline needs an idle machine",
			in.TotalCores, in.MachineNumCPU, machineSaturationFrac*100))
	}
	if in.OtherJudged && in.OtherLimit > 0 && in.OtherCores >= in.OtherLimit {
		reasons = append(reasons, fmt.Sprintf("other programs used about %.2f cores (>= limit %.2f, ps -A estimate): the baseline needs an idle machine",
			in.OtherCores, in.OtherLimit))
	}
	// 判斷不了 ≠ 沒問題：-require-machine-check 開著時，整機或 other 判斷不了就不能當作 valid
	// （關掉時只有 finishStep 的警告，給臨時跑一下用）。other 的檢查關掉（OtherLimit <= 0）時不要求 OtherJudged
	if in.RequireMachineCheck && (!in.TotalJudged || (in.OtherLimit > 0 && !in.OtherJudged)) {
		reasons = append(reasons, "machine-wide / other-programs CPU could not be judged (-require-machine-check is on): "+in.JudgeNote)
	}
	if in.ViewersJoined < in.ViewersTarget {
		reasons = append(reasons, fmt.Sprintf("only %d of %d viewers were in the room (rejected joins or disconnects)", in.ViewersJoined, in.ViewersTarget))
	}
	return reasons
}

type collapseInput struct {
	Measured      int
	Succeeded     int // 回 201 的數量
	PostP99       time.Duration
	P99Limit      time.Duration // <= 0 = 不檢查
	SampleMissing int
	SampleExpect  int
	Disconnects   int
	ViewersJoined int
}

// 「明顯崩潰」：成功率低於 90%、POST p99 超過上限、sampled 連線漏超過 10%、或超過 10% 的 viewer 斷線。
// mixed 用它決定要不要停掉後面更大的 step（不是 validity：valid 的 step 也可能已經崩潰）
func evaluateCollapse(in collapseInput) (bool, string) {
	if in.Measured > 0 && float64(in.Succeeded) < 0.9*float64(in.Measured) {
		return true, fmt.Sprintf("only %d of %d sends succeeded (201)", in.Succeeded, in.Measured)
	}
	if in.P99Limit > 0 && in.PostP99 > in.P99Limit {
		return true, fmt.Sprintf("POST p99 %.0fms exceeds %.0fms", durMs(in.PostP99), durMs(in.P99Limit))
	}
	if in.SampleExpect > 0 && float64(in.SampleMissing) > 0.1*float64(in.SampleExpect) {
		return true, fmt.Sprintf("%d of %d expected messages missing on sampled connections", in.SampleMissing, in.SampleExpect)
	}
	if in.ViewersJoined > 0 && float64(in.Disconnects) > 0.1*float64(in.ViewersJoined) {
		return true, fmt.Sprintf("%d of %d viewers disconnected", in.Disconnects, in.ViewersJoined)
	}
	return false, ""
}
