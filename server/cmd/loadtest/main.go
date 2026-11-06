// Command loadtest 是 danmaku 的本機壓測工具：模擬大量 WS 觀眾與 POST 發彈幕，量測延遲、漏收與資源使用，
// 用來回答「寫入是不是第一個瓶頸」（ROADMAP M6）。
//
// 數字只能拿來比較前後：工具與 server 跑在同一台機器上，會搶 CPU，不代表容量上限。
// 安全：只准打 localhost / 127.0.0.1 的 server、TEST_DATABASE_URL 必須是本機的 danmaku_test、Redis 必須是本機 db 15。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/jay41chen/danmaku/server/internal/auth"
)

type options struct {
	scenario     string
	servers      string
	out          string
	serverPIDs   string
	serverLogs   string
	containers   string
	steps        string
	writeRate    float64
	fanoutRate   float64
	rooms        int
	perRoom      int
	roomRate     float64
	warmup       time.Duration
	duration     time.Duration
	drain        time.Duration
	settle       time.Duration
	rampRate     float64
	sampled      int
	users        int
	maxInFlight  int
	postTimeout  time.Duration
	lagThreshold time.Duration
	toolCPUFrac  float64
	collapseP99  time.Duration
	cont         bool
	monInterval  time.Duration
	otherLimit   float64
	requireCheck bool
}

const defaultSteps = "100x10,500x20,1000x50,2500x100,5000x100"

func parseFlags(args []string) (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	fs.StringVar(&o.scenario, "scenario", "", "write | mixed | fanout | rooms")
	fs.StringVar(&o.servers, "servers", "", "comma-separated server base URLs (localhost / 127.0.0.1 only), e.g. http://127.0.0.1:6388,http://127.0.0.1:6389")
	fs.StringVar(&o.out, "out", "", "JSON result path (default loadtest-<scenario>-<time>.json in the current directory)")
	fs.StringVar(&o.serverPIDs, "server-pids", "", "comma-separated server PIDs to sample CPU with ps")
	fs.StringVar(&o.serverLogs, "server-logs", "", "comma-separated server log files containing LOADTEST_TIMING_LOG lines")
	fs.StringVar(&o.containers, "docker-containers", "danmaku-db,danmaku-redis", "containers to sample with `docker stats --no-stream` (empty = off)")
	fs.StringVar(&o.steps, "steps", defaultSteps, "mixed/fanout steps as VIEWERSxMSGS_PER_SEC list (fanout uses only the viewer counts)")
	fs.Float64Var(&o.writeRate, "write-rate", 100, "write scenario: POST per second")
	fs.Float64Var(&o.fanoutRate, "fanout-rate", 5, "fanout scenario: POST per second")
	fs.IntVar(&o.rooms, "rooms", 500, "rooms scenario: number of rooms")
	fs.IntVar(&o.perRoom, "viewers-per-room", 10, "rooms scenario: viewers per room")
	fs.Float64Var(&o.roomRate, "room-rate", 1, "rooms scenario: messages per second per room")
	fs.DurationVar(&o.warmup, "warmup", 5*time.Second, "warm-up before measuring (sends happen but are excluded from stats)")
	fs.DurationVar(&o.duration, "duration", 30*time.Second, "measured duration per step")
	fs.DurationVar(&o.drain, "drain", 3*time.Second, "wait after the last send for sampled connections to receive")
	fs.DurationVar(&o.settle, "settle", 2*time.Second, "pause after ramp-up before sending")
	fs.Float64Var(&o.rampRate, "ramp-rate", 100, "new WS connections per second while ramping up")
	fs.IntVar(&o.sampled, "sampled", 4, "number of sampled (JSON-parsing) connections")
	fs.IntVar(&o.users, "users", 0, "number of seeded users that POST (0 = auto: 3x the highest rate, at least 50)")
	fs.IntVar(&o.maxInFlight, "max-inflight", 1024, "cap on in-flight POSTs; exceeding it counts as dropped and invalidates the step")
	fs.DurationVar(&o.postTimeout, "post-timeout", 10*time.Second, "per-POST timeout")
	fs.DurationVar(&o.lagThreshold, "lag-threshold", 50*time.Millisecond, "step is invalid when the tool's scheduling lag p99 exceeds this")
	fs.Float64Var(&o.toolCPUFrac, "tool-cpu-frac", 0.8, "step is invalid when the tool's CPU reaches this fraction of GOMAXPROCS cores")
	fs.DurationVar(&o.collapseP99, "collapse-p99", 2*time.Second, "mixed: a step whose POST p99 exceeds this counts as collapsed")
	fs.BoolVar(&o.cont, "continue", false, "mixed/fanout: keep running after an invalid or collapsed step")
	fs.Float64Var(&o.otherLimit, "other-cores-limit", defaultOtherCoresLimit, "step is invalid when other programs use this many cores (ps -A total minus tool/servers/containers); <= 0 = off")
	fs.BoolVar(&o.requireCheck, "require-machine-check", true, "a step is invalid when whole-machine / other-programs CPU cannot be judged, and -server-pids is required; false = warning only (ad hoc runs)")
	fs.DurationVar(&o.monInterval, "monitor-interval", 5*time.Second, "interval of background ps / docker stats sampling")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return o, nil
}

// "100x10,500x20" → steps
func parseSteps(s string) ([]stepSpec, error) {
	var out []stepSpec
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, r, ok := strings.Cut(part, "x")
		viewers, err1 := strconv.Atoi(v)
		rate, err2 := strconv.ParseFloat(r, 64)
		if !ok || err1 != nil || err2 != nil || viewers < 0 || rate <= 0 {
			return nil, fmt.Errorf("bad step %q: want VIEWERSxRATE, e.g. 100x10", part)
		}
		out = append(out, stepSpec{Viewers: viewers, Rate: rate, Rooms: 1})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no steps given")
	}
	return out, nil
}

// 依 scenario 組出要跑的 steps
func buildSteps(o *options) ([]stepSpec, error) {
	switch o.scenario {
	case "write":
		if o.writeRate <= 0 {
			return nil, fmt.Errorf("-write-rate must be positive")
		}
		return []stepSpec{{Viewers: 0, Rate: o.writeRate, Rooms: 1}}, nil
	case "mixed":
		return parseSteps(o.steps)
	case "fanout":
		steps, err := parseSteps(o.steps)
		if err != nil {
			return nil, err
		}
		if o.fanoutRate <= 0 {
			return nil, fmt.Errorf("-fanout-rate must be positive")
		}
		for i := range steps {
			steps[i].Rate = o.fanoutRate
		}
		return steps, nil
	case "rooms":
		if o.rooms <= 0 || o.perRoom <= 0 || o.roomRate <= 0 {
			return nil, fmt.Errorf("-rooms, -viewers-per-room and -room-rate must be positive")
		}
		return []stepSpec{{Viewers: o.rooms * o.perRoom, Rate: float64(o.rooms) * o.roomRate, Rooms: o.rooms}}, nil
	}
	return nil, fmt.Errorf("-scenario must be one of write, mixed, fanout, rooms")
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parsePIDs(s string) ([]int, error) {
	var pids []int
	for _, p := range splitList(s) {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("bad PID %q", p)
		}
		pids = append(pids, n)
	}
	return pids, nil
}

// 提高本程序的 NOFILE soft limit 到至少 need；回傳實際的 soft / hard。拿不到足夠的就回 error
func raiseNoFile(need uint64) (soft, hard uint64, err error) {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		return 0, 0, err
	}
	if rl.Cur < need {
		// macOS 的 hard limit 常是 unlimited，但實際上限是 kern.maxfilesperproc：從想要的值往下試
		for _, try := range []uint64{need, 65536, 49152, 24576, 10240} {
			if try < need || (rl.Max < 1<<62 && try > rl.Max) {
				continue
			}
			if syscall.Setrlimit(syscall.RLIMIT_NOFILE, &syscall.Rlimit{Cur: try, Max: rl.Max}) == nil {
				break
			}
		}
		syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl)
	}
	if rl.Cur < need {
		return rl.Cur, rl.Max, fmt.Errorf("NOFILE soft limit is %d but this run needs at least %d: use fewer viewers", rl.Cur, need)
	}
	return rl.Cur, rl.Max, nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

// 回傳 exit code：0 完成（step 可能 INVALID，看報告）；1 設定 / 安全檢查 / 執行失敗；2 清理沒成功；130 被中斷
func run(args []string) (code int) {
	o, err := parseFlags(args)
	if err != nil {
		return 1
	}
	sc := &scrubber{}
	fail := func(format string, a ...any) int {
		fmt.Fprintln(os.Stderr, "loadtest: "+sc.scrub(fmt.Sprintf(format, a...)))
		return 1
	}

	// ---- 安全檢查：先於任何連線與寫入 ----
	jwtSecret := os.Getenv("JWT_SECRET")
	dbURL := os.Getenv("TEST_DATABASE_URL")
	redisURL := os.Getenv("TEST_REDIS_URL")
	sc.add(jwtSecret)
	servers, err := checkServerURLs(splitList(o.servers))
	if err != nil {
		return fail("refusing to run: %v", err)
	}
	dbPass, err := checkDatabaseURL(dbURL)
	if err != nil {
		return fail("refusing to run: %v", err)
	}
	sc.add(dbPass)
	redisPass, err := checkRedisURL(redisURL)
	if err != nil {
		return fail("refusing to run: %v", err)
	}
	sc.add(redisPass)
	if err := auth.ValidateJWTSecretLength(jwtSecret); err != nil {
		return fail("JWT_SECRET: %v", err)
	}
	steps, err := buildSteps(o)
	if err != nil {
		return fail("%v", err)
	}
	pids, err := parsePIDs(o.serverPIDs)
	if err != nil {
		return fail("%v", err)
	}
	if o.requireCheck && len(pids) == 0 {
		return fail("-require-machine-check is on but -server-pids is missing: server CPU cannot be measured, so every step would be invalid (pass -server-pids, or -require-machine-check=false for an ad hoc run)")
	}
	if o.sampled < 1 || o.duration <= 0 || o.warmup < 0 || o.rampRate <= 0 || o.maxInFlight < 1 {
		return fail("invalid flag value (-sampled >= 1, -duration > 0, -warmup >= 0, -ramp-rate > 0, -max-inflight >= 1)")
	}

	// ---- 規模 ----
	maxViewers, maxRate, maxRooms := 0, 0.0, 1
	for _, s := range steps {
		maxViewers = max(maxViewers, s.Viewers, o.sampled)
		maxRate = max(maxRate, s.Rate)
		maxRooms = max(maxRooms, s.Rooms)
	}
	users := o.users
	if users <= 0 {
		users = max(50, int(3*maxRate+0.999))
	}
	needFD := uint64(maxViewers + o.maxInFlight + 256)
	soft, hard, fdErr := raiseNoFile(needFD)
	if fdErr != nil {
		return fail("%v", fdErr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	issuer, err := auth.NewTokenIssuer(jwtSecret, accessTokenTTL)
	if err != nil {
		return fail("token issuer: %v", err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fail("connect database: %s", sc.err(err))
	}
	defer pool.Close()
	rdbOpt, _ := redis.ParseURL(redisURL) // checkRedisURL 已經確認過能解析
	rdb := redis.NewClient(rdbOpt)
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fail("connect redis: %s", sc.err(err))
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(new(int64)); err != nil {
		return fail("database has no users table (start a server once to run migrations): %s", sc.err(err))
	}

	// ---- 建資料；從這一行起，任何離開路徑都要清理 ----
	prefix, runTag := newRunPrefix()
	startedAt := time.Now()
	rep := &runReport{
		Scenario: o.scenario, StartedAt: startedAt.Format(time.RFC3339), GoVersion: runtime.Version(),
		NumCPU: runtime.NumCPU(), GOMAXPROCS: runtime.GOMAXPROCS(0),
		NOFILE:  fileDescriptors{Needed: needFD, Soft: soft, Hard: hard},
		Servers: servers, Prefix: prefix, Users: users, Rooms: maxRooms,
		Options: map[string]any{
			"steps": o.steps, "warmup": o.warmup.String(), "duration": o.duration.String(), "drain": o.drain.String(),
			"ramp_rate": o.rampRate, "sampled": o.sampled, "max_inflight": o.maxInFlight,
			"lag_threshold": o.lagThreshold.String(), "tool_cpu_frac": o.toolCPUFrac, "continue": o.cont, "other_cores_limit": o.otherLimit, "require_machine_check": o.requireCheck,
			"server_pids": pids, "docker_containers": o.containers,
		},
		Steps: []stepResult{},
	}
	rep.LoadAvgAtStart, _ = readLoadAvg()
	outPath := o.out
	if outPath == "" {
		outPath = fmt.Sprintf("loadtest-%s-%s.json", o.scenario, startedAt.Format("20060102-150405"))
	}

	data, seedErr := seedData(ctx, pool, prefix, users, maxRooms, issuer)
	var userIDs []int64
	if data != nil {
		userIDs = data.userIDs
	}
	defer func() {
		cr := cleanupData(pool, rdb, prefix, userIDs)
		cr.Error = sc.scrub(cr.Error) // 錯誤訊息可能帶出連線字串裡的密碼，進 JSON / 表格前先遮
		rep.Cleanup = &cr
		rep.FinishedAt = time.Now().Format(time.RFC3339)
		if err := writeReport(outPath, rep); err != nil {
			fmt.Fprintln(os.Stderr, "loadtest: write report:", sc.err(err))
		}
		printTable(os.Stdout, rep)
		fmt.Printf("\nresult written to %s\n", outPath)
		if cr.Error != "" || cr.Leftover != 0 {
			fmt.Fprintf(os.Stderr, "loadtest: CLEANUP INCOMPLETE for prefix %s: %s (leftover %d)\n", prefix, cr.Error, cr.Leftover)
			code = 2
		}
	}()
	if seedErr != nil {
		return fail("seed data: %s", sc.err(seedErr))
	}
	fmt.Printf("seeded prefix=%s users=%d rooms=%d; NOFILE soft=%d hard=%d (needed %d)\n", prefix, users, maxRooms, soft, hard, needFD)

	httpClient := newHTTPClient(o.maxInFlight, o.postTimeout)
	if err := preflight(ctx, httpClient, servers, data.rooms[0]); err != nil {
		return fail("preflight: %s", sc.err(err))
	}

	mon := newMonitor(pids, splitList(o.containers), o.monInterval, sc.scrub)
	monCtx, monStop := context.WithCancel(ctx)
	defer monStop()
	mon.start(monCtx)
	if w := idleWarning(monCtx, mon, pids, runtime.NumCPU(), o.otherLimit); w != "" {
		fmt.Fprintln(os.Stderr, "loadtest: WARNING: "+w)
		rep.Notes = append(rep.Notes, w)
	}

	vp := newViewerPool(viewerConfig{
		Servers: servers, Rooms: data.rooms, Run: runTag, Sampled: o.sampled,
		RampRate: o.rampRate, JoinTimeout: 10 * time.Second, MaxDialing: 128,
	})
	defer vp.closeAll()

	r := &runner{
		cfg: runnerConfig{
			Run: runTag, Rooms: data.rooms, Servers: servers, Tokens: data.tokens, Sampled: o.sampled,
			Settle: o.settle, Warmup: o.warmup, Measure: o.duration, Drain: o.drain, MaxInFlight: o.maxInFlight,
			LagThreshold: o.lagThreshold, ToolCPUFrac: o.toolCPUFrac, CollapseP99: o.collapseP99,
			NumCPU: runtime.NumCPU(), ServerPIDs: len(pids), DockerTargets: len(splitList(o.containers)), OtherCoresLimit: o.otherLimit, RequireMachineCheck: o.requireCheck,
		},
		pool: vp, mon: mon,
		client: &poster{client: httpClient, servers: servers, tokens: data.tokens, run: runTag},
	}

	stopAfter := o.scenario == "mixed" || o.scenario == "fanout"
	stopped := false
	for i, spec := range steps {
		if stopped {
			note := "not run: an earlier step was invalid or collapsed (use -continue to run anyway)"
			if rep.Aborted {
				note = "not run: the run was interrupted"
			}
			rep.Steps = append(rep.Steps, stepResult{Index: i, Spec: spec, Status: "not_run", Note: note})
			continue
		}
		fmt.Printf("step %d/%d: %d viewers x %.0f msg/s ...\n", i+1, len(steps), spec.Viewers, spec.Rate)
		sr := r.runStep(ctx, i, spec)
		rep.Steps = append(rep.Steps, sr)
		rep.RejectReasons = vp.reasons()
		writeReport(outPath, rep)
		if ctx.Err() != nil {
			rep.Aborted = true
			stopped = true
			rep.Notes = append(rep.Notes, "interrupted: remaining steps were not run")
			continue
		}
		if stopAfter && !o.cont && (!sr.Valid || sr.Collapsed) {
			stopped = true
		}
	}
	vp.closeAll()

	// server 的計時行每 5 秒才寫一次：等最後一個區間寫出來再讀
	if logs := splitList(o.serverLogs); len(logs) > 0 && !rep.Aborted {
		sleepCtx(ctx, 6*time.Second)
		attachTiming(rep, logs)
	} else if len(logs) == 0 {
		rep.Notes = append(rep.Notes, "slow-client kicks were NOT counted and the server timing breakdown (create / acquire / limiter / publish) is missing: no -server-logs given")
	}
	if rep.Aborted {
		return 130
	}
	return 0
}

// 讀 server log 檔，把計時行附到每個有跑的 step
func attachTiming(rep *runReport, logs []string) {
	for _, path := range logs {
		recs, err := readTimingLines(path)
		if err != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("cannot read timing log %s: %v", path, err))
			continue
		}
		kicks, err := readSlowKickTimes(path)
		if err != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("cannot count slow-client kicks in %s: %v", path, err))
		} else {
			if rep.SlowKicksLogFile == nil {
				rep.SlowKicksLogFile = map[string]int{}
			}
			rep.SlowKicksLogFile[path] = len(kicks)
		}
		for i := range rep.Steps {
			s := &rep.Steps[i]
			if s.Status != "ran" {
				continue
			}
			ts := summarizeTiming(path, recs, s.windowFrom, s.windowTo)
			ts.SlowKicksInWindow = countInWindow(kicks, s.windowFrom, s.windowTo)
			s.Timing = append(s.Timing, ts)
		}
	}
}
