package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// 報告：每個 scenario 產一個 JSON 檔 + 一張簡短的文字表。不合格的 step 照印，標 INVALID。

type stepSpec struct {
	Viewers int     `json:"viewers"`
	Rate    float64 `json:"messages_per_sec"`
	Rooms   int     `json:"rooms"`
}

type framesResult struct {
	Total  int64   `json:"total"`
	PerSec float64 `json:"per_sec"`
	// 估計值 viewers*rate/rooms + viewers/5（見 expectedFrames），Ratio = PerSec / Expected；Expected 為 0 時 Ratio 是 0
	Expected float64 `json:"frames_expected_per_sec"`
	Ratio    float64 `json:"frames_actual_over_expected"`
}

type cpuResult struct {
	ToolCores      float64        `json:"tool_cores"`
	ToolLimitCores float64        `json:"tool_saturation_limit_cores"`
	Resources      resourceWindow `json:"resources"`

	// 工具 + server + container 的 core 數加總（下限，見 machineLoad）；MachineJudged=false 時不能拿來判斷是否飽和
	MachineCores  float64 `json:"machine_cores"`
	MachineJudged bool    `json:"machine_saturation_judged"`
	MachineNote   string  `json:"machine_saturation_note,omitempty"` // 判斷不了的原因

	// 整機所有程序的 CPU（ps -A，粗略）與其他程序用掉的 core 數（= 總和 - 上面量到的），見 machineWide
	TotalCores  float64 `json:"machine_total_cores_all_processes"`
	TotalJudged bool    `json:"machine_total_judged"`
	OtherCores  float64 `json:"other_cores"`
	OtherJudged bool    `json:"other_cores_judged"`
	OtherNote   string  `json:"other_cores_note,omitempty"`
}

type loadAvgResult struct {
	Start [3]float64 `json:"start"`
	End   [3]float64 `json:"end"`
}

type viewersResult struct {
	Target          int `json:"target"`
	AliveAtStart    int `json:"alive_at_measure_start"`
	SampledJoined   int `json:"sampled_joined"`
	ClosesAfterJoin int `json:"closes_after_join"`
}

type stepResult struct {
	Index          int      `json:"index"`
	Status         string   `json:"status"` // ran | not_run | aborted
	Spec           stepSpec `json:"spec"`
	Valid          bool     `json:"valid"`
	InvalidReasons []string `json:"invalid_reasons,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
	Collapsed      bool     `json:"collapsed"`
	CollapseReason string   `json:"collapse_reason,omitempty"`
	Note           string   `json:"note,omitempty"`

	MeasureSeconds float64         `json:"measure_seconds"`
	Ramp           rampResult      `json:"ramp"`
	Viewers        viewersResult   `json:"viewers_result"`
	Send           sendSummary     `json:"send"`
	Receive        receiveSummary  `json:"receive"`
	Frames         framesResult    `json:"frames"`
	CPU            cpuResult       `json:"cpu"`
	LoadAvg        loadAvgResult   `json:"load_avg"`
	Timing         []timingSummary `json:"server_timing,omitempty"`
	Handler        handlerSummary  `json:"handler_post_timing"` // 來自 /metrics，見 metrics.go

	// 不輸出：timing 彙整要用的窗口
	windowFrom time.Time `json:"-"`
	windowTo   time.Time `json:"-"`
}

type fileDescriptors struct {
	Needed uint64 `json:"needed"`
	Soft   uint64 `json:"soft_limit"`
	Hard   uint64 `json:"hard_limit"`
}

type runReport struct {
	Scenario       string          `json:"scenario"`
	StartedAt      string          `json:"started_at"`
	FinishedAt     string          `json:"finished_at"`
	GoVersion      string          `json:"go_version"`
	NumCPU         int             `json:"num_cpu"`
	GOMAXPROCS     int             `json:"gomaxprocs"`
	NOFILE         fileDescriptors `json:"nofile"`
	Servers        []string        `json:"servers"`
	Options        map[string]any  `json:"options"`
	LoadAvgAtStart [3]float64      `json:"load_avg_at_start"`
	Prefix         string          `json:"data_prefix"`
	Users          int             `json:"users"`
	Rooms          int             `json:"rooms"`
	RejectReasons  map[string]int  `json:"reject_reasons_run_total,omitempty"`
	// 每個 server log 檔「整個檔案」的「踢慢 client」行數（不只這次 run：包含檔案裡更早的內容、暖機、連線建立、step 之間）；step 內的見 server_timing
	SlowKicksLogFile map[string]int `json:"slow_client_kicks_log_file_total,omitempty"`
	Aborted          bool           `json:"aborted"`
	Steps            []stepResult   `json:"steps"`
	Cleanup          *cleanupResult `json:"cleanup,omitempty"`
	Notes            []string       `json:"notes,omitempty"`
}

// 寫 JSON（先寫暫存檔再 rename，Ctrl-C 時不會留下寫一半的檔案）
func writeReport(path string, rep *runReport) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Clean(path))
}

func ms(v float64) string {
	if v >= 100 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

func lat4(l latencySummary) string {
	if l.N == 0 {
		return "-"
	}
	return fmt.Sprintf("%s/%s/%s/%s", ms(l.P50Ms), ms(l.P95Ms), ms(l.P99Ms), ms(l.MaxMs))
}

// 印給人看的摘要表。INVALID 的 step 不省略，原因印在表後
func printTable(w io.Writer, rep *runReport) {
	fmt.Fprintf(w, "\nscenario=%s  num_cpu=%d  load_avg_start=%.2f %.2f %.2f  nofile=%d  (latencies in ms: p50/p95/p99/max)\n",
		rep.Scenario, rep.NumCPU, rep.LoadAvgAtStart[0], rep.LoadAvgAtStart[1], rep.LoadAvgAtStart[2], rep.NOFILE.Soft)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "step\tviewers\tmsg/s\tvalid\tPOST latency\tsend->recv\tmissing\tframes/s\t201/429/err/drop\tlag p50/p99\ttool cpu\tserver cpu")
	for _, s := range rep.Steps {
		if s.Status == "not_run" {
			fmt.Fprintf(tw, "%d\t%d\t%.0f\tNOT RUN\t\t\t\t\t\t\t\t\n", s.Index+1, s.Spec.Viewers, s.Spec.Rate)
			continue
		}
		valid := "ok"
		if !s.Valid {
			valid = "INVALID"
		}
		if s.Collapsed {
			valid += "+COLLAPSED"
		}
		fmt.Fprintf(tw, "%d\t%d/%d\t%.0f\t%s\t%s\t%s\t%d/%d\t%.0f\t%d/%d/%d/%d\t%s/%s\t%.2f\t%s\n",
			s.Index+1, s.Viewers.AliveAtStart, s.Viewers.Target, s.Spec.Rate, valid,
			lat4(s.Send.PostAll), lat4(s.Receive.Latency),
			s.Receive.Missing, s.Receive.Expected, s.Frames.PerSec,
			s.Send.Created, s.Send.RateLimited, s.Send.Errors, s.Send.Dropped,
			ms(s.Send.Lag.P50Ms), ms(s.Send.Lag.P99Ms), s.CPU.ToolCores, serverCPUText(s.CPU.Resources))
	}
	tw.Flush()
	for _, s := range rep.Steps {
		for _, r := range s.InvalidReasons {
			fmt.Fprintf(w, "step %d INVALID: %s\n", s.Index+1, r)
		}
		if s.Collapsed {
			fmt.Fprintf(w, "step %d collapsed: %s\n", s.Index+1, s.CollapseReason)
		}
		for _, r := range s.Warnings {
			fmt.Fprintf(w, "step %d warning: %s\n", s.Index+1, r)
		}
		if s.Note != "" {
			fmt.Fprintf(w, "step %d note: %s\n", s.Index+1, s.Note)
		}
		if s.Status == "ran" && len(s.Ramp.Reasons) > 0 {
			fmt.Fprintf(w, "step %d rejected joins: %s\n", s.Index+1, formatReasons(s.Ramp.Reasons))
		}
		if s.Status == "ran" {
			fmt.Fprintf(w, "step %d machine_cores=%s of %d  frames/s actual=%.0f expected=%.0f (ratio %.2f)  closes_after_join=%d\n",
				s.Index+1, machineText(s.CPU), rep.NumCPU, s.Frames.PerSec, s.Frames.Expected, s.Frames.Ratio, s.Viewers.ClosesAfterJoin)
			fmt.Fprintf(w, "step %d whole machine (ps -A): %s\n", s.Index+1, otherText(s.CPU))
			fmt.Fprintf(w, "step %d handler POST mean (from /metrics, all statuses): %s\n", s.Index+1, handlerText(s.Handler))
		}
		for _, t := range s.Timing {
			fmt.Fprintf(w, "step %d server timing (%s): lines=%d CreateMessage n=%d mean=%.1fms p50<=%sms p99<=%sms; pool acquires=%d empty=%d canceled=%d acquired_max=%d/%d\n",
				s.Index+1, filepath.Base(t.Path), t.Lines, t.CreateCount, t.CreateMeanMs, ms(t.CreateP50Ms), ms(t.CreateP99Ms),
				t.AcquireCount, t.EmptyAcquire, t.CanceledAcq, t.AcquiredMax, t.MaxConns)
			for _, n := range t.Notes {
				fmt.Fprintf(w, "step %d server timing note (%s): %s\n", s.Index+1, filepath.Base(t.Path), n)
			}
			// acquired_max / idle_min 是各行「當下」值，不是區間內的真正峰值
			fmt.Fprintf(w, "step %d breakdown (%s): create_mean=%.2fms = acquire_mean %.2fms (acquire_total/all acquires) + query_mean ~%.2fms (approx: create_mean - acquire_mean; acquire count may include non-CreateMessage acquires); limiter Allow mean=%.2fms (n=%d, err=%d); redis PUBLISH mean=%.2fms (n=%d, err=%d); slow-client kicks in window=%d; idle_min=%d (instantaneous)\n",
				s.Index+1, filepath.Base(t.Path), t.CreateMeanMs, t.AcquireMeanMs, t.QueryMeanMs,
				t.LimiterMeanMs, t.LimiterCount, t.LimiterErrors, t.PublishMeanMs, t.PublishCount, t.PublishErrors, t.SlowKicksInWindow, t.IdleMin)
		}
		if c := containerText(s.CPU.Resources); c != "" && s.Status == "ran" {
			fmt.Fprintf(w, "step %d containers cpu%%: %s\n", s.Index+1, c)
		}
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
	for _, path := range sortedKeys(rep.SlowKicksLogFile) {
		fmt.Fprintf(w, "slow-client kicks in the whole log file (%s, not only this run): %d\n", filepath.Base(path), rep.SlowKicksLogFile[path])
	}
	if len(rep.RejectReasons) > 0 {
		fmt.Fprintf(w, "rejected joins over the run: %s\n", formatReasons(rep.RejectReasons))
	}
	if rep.Cleanup != nil {
		c := rep.Cleanup
		fmt.Fprintf(w, "cleanup: messages=%d rooms=%d users=%d redis_keys=%d leftover=%d err=%q\n", c.Messages, c.Rooms, c.Users, c.RedisKey, c.Leftover, c.Error)
	}
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func machineText(c cpuResult) string {
	if !c.MachineJudged {
		return fmt.Sprintf("%.2f (NOT JUDGED: %s)", c.MachineCores, c.MachineNote)
	}
	return fmt.Sprintf("%.2f", c.MachineCores)
}

func otherText(c cpuResult) string {
	if !c.TotalJudged {
		return fmt.Sprintf("NOT JUDGED: %s", c.OtherNote)
	}
	if !c.OtherJudged {
		return fmt.Sprintf("total_all_processes=%.2f, other_cores NOT JUDGED: %s", c.TotalCores, c.OtherNote)
	}
	return fmt.Sprintf("total_all_processes=%.2f other_cores=%.2f (ps -A estimate)", c.TotalCores, c.OtherCores)
}

func handlerText(h handlerSummary) string {
	if len(h.Servers) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(h.Servers))
	for _, s := range h.Servers {
		if s.Note != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", s.Server, s.Note))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %.2fms (n=%d)", s.Server, s.MeanMs, s.Count))
	}
	return fmt.Sprintf("%.2fms (n=%d) [%s]", h.MeanMs, h.Count, strings.Join(parts, "; "))
}

func serverCPUText(r resourceWindow) string {
	if len(r.Servers) == 0 {
		return "-"
	}
	pids := make([]string, 0, len(r.Servers))
	for pid := range r.Servers {
		pids = append(pids, pid)
	}
	sort.Strings(pids)
	parts := make([]string, 0, len(pids))
	for _, pid := range pids {
		parts = append(parts, fmt.Sprintf("%.2f", r.Servers[pid].AvgCores))
	}
	return strings.Join(parts, "+")
}

func containerText(r resourceWindow) string {
	if len(r.Containers) == 0 {
		return ""
	}
	names := make([]string, 0, len(r.Containers))
	for n := range r.Containers {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		c := r.Containers[n]
		parts = append(parts, fmt.Sprintf("%s avg=%.1f max=%.1f (n=%d)", n, c.AvgPct, c.MaxPct, c.Samples))
	}
	return strings.Join(parts, "; ")
}
