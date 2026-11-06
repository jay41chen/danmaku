package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 系統資源取樣：工具自己的 CPU、server 程序的 CPU（ps）、整機所有程序的 CPU（ps -A）、Postgres / Redis container 的 CPU（docker stats，唯讀）、load average。
// 取樣頻率刻意低（預設 5 秒），不讓量測工具本身變成負載；任何一種取樣失敗都只記錄、不中斷壓測。

// 本程序累計用掉的 CPU 秒數（user + system）
func selfCPUSeconds() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return tvSeconds(ru.Utime) + tvSeconds(ru.Stime)
}

func tvSeconds(tv syscall.Timeval) float64 {
	return float64(tv.Sec) + float64(tv.Usec)/1e6
}

// 解析 ps 的 time 欄位：macOS 是 "M:SS.cc" 或 "H:MM:SS.cc"，Linux 可能是 "DD-HH:MM:SS"
func parsePSTime(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty cpu time")
	}
	days := 0.0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, fmt.Errorf("bad cpu time %q", s)
		}
		days, s = float64(n), rest
	}
	total := 0.0
	for _, part := range strings.Split(s, ":") {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, fmt.Errorf("bad cpu time %q", s)
		}
		total = total*60 + v
	}
	return total + days*86400, nil
}

// 解析 docker stats 的 CPUPerc（"12.34%"）
func parseDockerCPU(s string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
}

// 讀 load average（1 / 5 / 15 分鐘）：Linux 讀 /proc/loadavg，macOS 用 sysctl
func readLoadAvg() ([3]float64, error) {
	var out [3]float64
	var text string
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		text = string(b)
	} else {
		b, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
		if err != nil {
			return out, err
		}
		text = string(b)
	}
	fields := strings.Fields(strings.Trim(strings.TrimSpace(text), "{}"))
	if len(fields) < 3 {
		return out, fmt.Errorf("unexpected loadavg %q", text)
	}
	for i := range 3 {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return out, err
		}
		out[i] = v
	}
	return out, nil
}

type psSample struct {
	At     time.Time
	CPUSec float64 // 該程序累計 CPU 秒數
	RSSKB  int64
}

// 用 ps 讀某個 PID 的累計 CPU 時間與 RSS。累計值相減才是區間平均，比 %cpu（macOS 上是衰減平均）準
func readPS(ctx context.Context, pid int) (psSample, error) {
	out, err := exec.CommandContext(ctx, "ps", "-o", "time=,rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return psSample{}, fmt.Errorf("ps pid %d: %w", pid, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return psSample{}, fmt.Errorf("ps pid %d: unexpected output %q", pid, strings.TrimSpace(string(out)))
	}
	cpu, err := parsePSTime(fields[0])
	if err != nil {
		return psSample{}, err
	}
	rss, _ := strconv.ParseInt(fields[1], 10, 64)
	return psSample{At: time.Now(), CPUSec: cpu, RSSKB: rss}, nil
}

type dockerSample struct {
	At  time.Time
	CPU map[string]float64 // container 名稱 → CPU%（Docker 的算法：100% = 一顆 core）
}

// docker stats --no-stream：唯讀，一次取全部指定 container。名稱由使用者指定，用 -- 隔開避免被當 flag
func readDockerStats(ctx context.Context, names []string) (dockerSample, error) {
	args := append([]string{"stats", "--no-stream", "--format", "{{.Name}}\t{{.CPUPerc}}", "--"}, names...)
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return dockerSample{}, fmt.Errorf("docker stats: %w", err)
	}
	s := dockerSample{At: time.Now(), CPU: map[string]float64{}}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, perc, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		v, err := parseDockerCPU(perc)
		if err != nil {
			continue
		}
		s.CPU[name] = v
	}
	if len(s.CPU) == 0 {
		return dockerSample{}, errors.New("docker stats: no usable rows")
	}
	return s, nil
}

// 整台機器所有程序的 CPU：一次 ps -A -o pid=,%cpu=，回傳 %cpu 總和（100 = 一顆 core）與各 PID 的值。
// 精度限制：%cpu 在 macOS / Linux 都不是區間平均（macOS 是衰減平均、Linux 是程序整個生命期的平均），
// 短暫的尖峰會被攤平、剛啟動的程序會被低估；ps 自己和已經結束的程序也量不到。所以它是「這台機器忙不忙」的粗略指標，不是精確的 core 數。
// 選它是因為最便宜：一次 fork、幾十 ms，每 5 秒一次；top -l 要多等 1 秒以上而且更重
type machineSample struct {
	At       time.Time
	TotalPct float64
	ByPID    map[int]float64
}

func parsePSAll(out string) (machineSample, error) {
	s := machineSample{ByPID: map[int]float64{}}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		pct, err2 := strconv.ParseFloat(f[1], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		s.TotalPct += pct
		s.ByPID[pid] = pct
	}
	if len(s.ByPID) == 0 {
		return machineSample{}, errors.New("ps -A: no usable rows")
	}
	return s, nil
}

func readMachineCPU(ctx context.Context) (machineSample, error) {
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,%cpu=").Output()
	if err != nil {
		return machineSample{}, fmt.Errorf("ps -A: %w", err)
	}
	s, err := parsePSAll(string(out))
	s.At = time.Now()
	return s, err
}

// 背景取樣器
type monitor struct {
	pids       []int
	containers []string
	interval   time.Duration
	scrub      func(string) string

	mu         sync.Mutex
	ps         map[int][]psSample
	docker     []dockerSample
	psErrs     int
	dockerErrs int
	dockerOff  bool
	lastErr    string

	machine     []machineSample
	machineErrs int
	machineOff  bool
}

func newMonitor(pids []int, containers []string, interval time.Duration, scrub func(string) string) *monitor {
	return &monitor{pids: pids, containers: containers, interval: interval, scrub: scrub, ps: map[int][]psSample{}}
}

// 立刻對每個 PID 取一次 ps 樣本（step 量測窗口的起點 / 終點用），回傳取樣時間
func (m *monitor) snapshotPS(ctx context.Context) time.Time {
	at := time.Now()
	for _, pid := range m.pids {
		s, err := readPS(ctx, pid)
		m.mu.Lock()
		if err != nil {
			m.psErrs++
			m.lastErr = m.scrub(err.Error())
		} else {
			m.ps[pid] = append(m.ps[pid], s)
		}
		m.mu.Unlock()
	}
	return at
}

// 啟動背景取樣，直到 ctx 取消：ps（有給 -server-pids 才有）、docker（有 container 才有）、整機 ps -A 各一條 goroutine，互不拖慢
func (m *monitor) start(ctx context.Context) {
	if len(m.pids) > 0 {
		go m.loop(ctx, func() { m.snapshotPS(ctx) })
	}
	if len(m.containers) > 0 {
		go m.loop(ctx, func() { m.sampleDocker(ctx) })
	}
	go m.loop(ctx, func() { m.sampleMachine(ctx) })
}

// 整機 ps -A 取樣；連續失敗 3 次且從沒成功過就停用（之後 window 會說「判斷不了」）
func (m *monitor) sampleMachine(ctx context.Context) (machineSample, error) {
	m.mu.Lock()
	off := m.machineOff
	m.mu.Unlock()
	if off {
		return machineSample{}, errors.New("machine-wide sampling disabled after repeated failures")
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s, err := readMachineCPU(cctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.machineErrs++
		m.lastErr = m.scrub(err.Error())
		if m.machineErrs >= 3 && len(m.machine) == 0 {
			m.machineOff = true
		}
		return machineSample{}, err
	}
	m.machine = append(m.machine, s)
	return s, nil
}

func (m *monitor) loop(ctx context.Context, fn func()) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn()
		}
	}
}

// docker stats 一次要 1～2 秒，逾時 15 秒；連續失敗 3 次且從沒成功過就停用，不再浪費（成功過一次之後的失敗只計數、不停用）
func (m *monitor) sampleDocker(ctx context.Context) {
	m.mu.Lock()
	off := m.dockerOff
	m.mu.Unlock()
	if off {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	s, err := readDockerStats(cctx, m.containers)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.dockerErrs++
		m.lastErr = m.scrub(err.Error())
		if m.dockerErrs >= 3 && len(m.docker) == 0 {
			m.dockerOff = true
		}
		return
	}
	m.docker = append(m.docker, s)
}

type procCPU struct {
	AvgCores float64 `json:"avg_cores"`
	MaxCores float64 `json:"max_interval_cores"` // 窗口內相鄰兩個樣本之間的最高值
	RSSMaxMB float64 `json:"rss_max_mb"`
	Samples  int     `json:"samples"`
}

type containerCPU struct {
	AvgPct  float64 `json:"avg_cpu_percent"`
	MaxPct  float64 `json:"max_cpu_percent"`
	Samples int     `json:"samples"`
}

type resourceWindow struct {
	Servers    map[string]procCPU      `json:"server_cpu_by_pid,omitempty"`
	Containers map[string]containerCPU `json:"container_cpu,omitempty"`

	// 整機所有程序的 CPU 總和（ps -A %cpu，粗略，見 machineSample），單位是 core；MachineSamples = 窗口內的樣本數
	MachineTotalCores float64  `json:"machine_total_cores_ps_all,omitempty"`
	MachineSamples    int      `json:"machine_total_samples"`
	Notes             []string `json:"notes,omitempty"`
}

// 彙整窗口 [from, to] 內的樣本。ps：用窗口內第一個與最後一個樣本的累計 CPU 相減；docker：窗口內樣本的平均與最大
func (m *monitor) window(from, to time.Time) resourceWindow {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := resourceWindow{Servers: map[string]procCPU{}, Containers: map[string]containerCPU{}}
	for _, pid := range m.pids {
		var in []psSample
		for _, s := range m.ps[pid] {
			if !s.At.Before(from) && !s.At.After(to.Add(time.Second)) {
				in = append(in, s)
			}
		}
		key := strconv.Itoa(pid)
		if len(in) < 2 {
			w.Notes = append(w.Notes, fmt.Sprintf("pid %d: fewer than 2 ps samples in the window", pid))
			continue
		}
		pc := procCPU{Samples: len(in)}
		first, last := in[0], in[len(in)-1]
		if dt := last.At.Sub(first.At).Seconds(); dt > 0 {
			pc.AvgCores = (last.CPUSec - first.CPUSec) / dt
		}
		for i := 1; i < len(in); i++ {
			if dt := in[i].At.Sub(in[i-1].At).Seconds(); dt > 0 {
				pc.MaxCores = max(pc.MaxCores, (in[i].CPUSec-in[i-1].CPUSec)/dt)
			}
		}
		for _, s := range in {
			pc.RSSMaxMB = max(pc.RSSMaxMB, float64(s.RSSKB)/1024)
		}
		w.Servers[key] = pc
	}
	sums := map[string]*containerCPU{}
	for _, s := range m.docker {
		if s.At.Before(from) || s.At.After(to) {
			continue
		}
		for name, pct := range s.CPU {
			c := sums[name]
			if c == nil {
				c = &containerCPU{}
				sums[name] = c
			}
			c.AvgPct += pct
			c.MaxPct = max(c.MaxPct, pct)
			c.Samples++
		}
	}
	for name, c := range sums {
		c.AvgPct /= float64(c.Samples)
		w.Containers[name] = *c
	}
	for _, ms := range m.machine {
		if !ms.At.Before(from) && !ms.At.After(to) {
			w.MachineTotalCores += ms.TotalPct / 100
			w.MachineSamples++
		}
	}
	if w.MachineSamples > 0 {
		w.MachineTotalCores /= float64(w.MachineSamples)
	}
	if len(m.containers) > 0 && len(sums) == 0 {
		w.Notes = append(w.Notes, "no docker stats samples in the window (sampling failed or window shorter than the interval)")
	}
	if m.psErrs > 0 || m.dockerErrs > 0 || m.machineErrs > 0 {
		w.Notes = append(w.Notes, fmt.Sprintf("sampling errors so far: ps=%d docker=%d machine=%d (last: %s)", m.psErrs, m.dockerErrs, m.machineErrs, m.lastErr))
	}
	return w
}

// 注意（F3，量測的已知限制，門檻未依此調整）：
//   - ps -A 看不到 kernel_task（macOS 沒有 pid 0 的列），核心自己用掉的 CPU 不在「整機總和」裡，總和是下限
//   - 這台機器是 4P+4E 核心，8 顆不等價，所以 85% 的飽和門檻偏鬆（效能核心早就滿了也未必到 85%）
//   - Docker VM 的額外開銷（虛擬化、I/O）會算進 other，不會算進 container 的 docker stats
//   - other 的 1.0 core 門檻還沒校準：要在閒置的 Mac 上先量一次 other 的底噪再決定
//
// 開跑前檢查機器是不是閒置：取一次 ps -A，扣掉工具自己和 -server-pids，剩下的當作「其他程序」。
// 超過門檻或總和 >= 85% NumCPU 回警告文字；取樣失敗也回警告（判斷不了）；一切正常回空字串。
// 只是警告，不中斷：之後每個 step 還會各自判斷。container 的 CPU 沒扣（開跑前通常閒置），所以 other 可能略高
func idleWarning(ctx context.Context, m *monitor, pids []int, numCPU int, limit float64) string {
	s, err := m.sampleMachine(ctx)
	if err != nil {
		return "machine idleness could NOT be judged before the run (ps -A failed: " + m.scrub(err.Error()) + ")"
	}
	return idleWarningFor(s, append([]int{os.Getpid()}, pids...), numCPU, limit)
}

func idleWarningFor(s machineSample, own []int, numCPU int, limit float64) string {
	total := s.TotalPct / 100
	other := total
	for _, pid := range own {
		other -= s.ByPID[pid] / 100
	}
	other = max(other, 0)
	var parts []string
	if numCPU > 0 && total >= machineSaturationFrac*float64(numCPU) {
		parts = append(parts, fmt.Sprintf("all processes use %.2f of %d cores (>= %.0f%%)", total, numCPU, machineSaturationFrac*100))
	}
	if limit > 0 && other >= limit {
		parts = append(parts, fmt.Sprintf("other programs use about %.2f cores (>= limit %.2f)", other, limit))
	}
	if len(parts) == 0 {
		return ""
	}
	return "machine is not idle before the run (ps -A estimate): " + strings.Join(parts, "; ") + ". Steps may be marked invalid; a baseline wants an idle machine"
}
