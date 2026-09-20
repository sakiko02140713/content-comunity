package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"content-community/internal/model"
	"content-community/internal/repository"
	"content-community/internal/rules"
)

// ============================================================================
// 并行审核引擎
//
// 流水线结构（各阶段同时启动，墙钟耗时趋近于"最慢分支"而非"各分支之和"）：
//
//	                ┌── 阶段1 规则引擎（本地 CPU，毫秒级）
//	请求 ── fan-out ┼── 阶段2 多维度语义审核 ──┬── 广告导流     （LLM）
//	                │                          ├── 辱骂攻击     （LLM）
//	                │                          ├── 色情低俗     （LLM）
//	                │                          ├── 政治敏感     （LLM）
//	                │                          └── 违法违禁     （LLM）
//	                └── 阶段3 分片并发审核（长文切分后逐片并行送审，片内再并行多维度）
//	                                              │
//	                                        fan-in 加权聚合 → 风险分 → 判定
//
// 性能度量：串行等价耗时 = 所有任务耗时之和；实际耗时 = 墙钟时间；
// 加速比 Speedup = 串行等价耗时 / 实际耗时，并发度上限由 Concurrency 控制。
// ============================================================================

// Verdict 审核判定结果。
type Verdict string

const (
	VerdictPass   Verdict = "pass"   // 通过，可直接发布
	VerdictReview Verdict = "review" // 疑似违规，转人工复核
	VerdictReject Verdict = "reject" // 违规，拒绝发布
)

// AuditDimension 语义审核维度定义。
type AuditDimension struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Desc  string `json:"desc"`
	Score int    `json:"score"` // 命中该维度时计入的风险分
	// KO 为 true 表示该维度违规属于"一票否决"类（涉政/涉黄/违法），直接拒绝。
	KO bool `json:"ko"`
}

// dimensionTable 维度表。权重与 KO 策略集中在此处，便于按社区规范调整。
var dimensionTable = []AuditDimension{
	{Key: "ad", Name: "广告导流", Desc: "营销推广、站外导流、联系方式", Score: 30, KO: false},
	{Key: "abuse", Name: "辱骂攻击", Desc: "人身攻击、歧视、网络暴力", Score: 35, KO: false},
	{Key: "porn", Name: "色情低俗", Desc: "色情描写、低俗擦边内容", Score: 60, KO: true},
	{Key: "politics", Name: "政治敏感", Desc: "涉政敏感言论", Score: 60, KO: true},
	{Key: "illegal", Name: "违法违禁", Desc: "违禁品、诈骗、赌博等", Score: 60, KO: true},
}

// Dimensions 返回维度表副本，供接口层展示。
func Dimensions() []AuditDimension {
	out := make([]AuditDimension, len(dimensionTable))
	copy(out, dimensionTable)
	return out
}

// StageTimeline 单个并行阶段的时间线，用于前端甘特图展示。
type StageTimeline struct {
	Name     string      `json:"name"`
	Key      string      `json:"key"`
	StartMs  int64       `json:"start_ms"` // 相对流水线起点的启动时刻
	EndMs    int64       `json:"end_ms"`   // 相对流水线起点的结束时刻
	Duration int64       `json:"duration"` // 该阶段自身耗时
	Workers  int         `json:"workers"`  // 该阶段并发任务数
	Tasks    []TaskTrace `json:"tasks,omitempty"`
	Result   string      `json:"result"`
	Status   string      `json:"status"` // ok / degraded / error
}

// TaskTrace 阶段内单个并行任务的执行轨迹。
//
// StartNs/EndNs 使用纳秒精度：毫秒精度下极快的任务会被量化为 0，
// 无法用来判定"任务是否真的并发重叠"，因此内部并行度测量以纳秒为准，
// 对外的 StartMs/EndMs 仍为毫秒，便于前端直接绘制时间线。
type TaskTrace struct {
	Name     string `json:"name"`
	StartMs  int64  `json:"start_ms"`
	EndMs    int64  `json:"end_ms"`
	Duration int64  `json:"duration"`
	Result   string `json:"result"`
	Status   string `json:"status"`
	Worker   int    `json:"worker"` // 执行该任务的工作协程编号

	StartNs int64 `json:"-"`
	EndNs   int64 `json:"-"`
}

// DimensionResult 单个语义维度的聚合结果。
type DimensionResult struct {
	Key       string   `json:"key"`
	Name      string   `json:"name"`
	Score     int      `json:"score"`
	RiskScore int      `json:"risk_score"`
	Verdict   string   `json:"verdict"`
	Reasons   []string `json:"reasons"`
	Hits      []string `json:"hits"`
	Degraded  bool     `json:"degraded"`
	KO        bool     `json:"ko"`
	ElapsedMs int64    `json:"elapsed_ms"`
}

// AuditRequest 并行审核请求。
type AuditRequest struct {
	ArticleID uint
	UserID    uint
	Title     string
	Content   string
	Tags      string
	// Mode: local 仅本地规则 / semantic 规则+多维度语义 / chunked 长文分片并发审核 /
	//       reply 回复的轻量审核（语义维度并行 + 更小分片）
	Mode string
	// Strategy 并行任务分配策略：dimension（按维度并行）| chunk（按分片并行）
	Strategy string
	// Concurrency 并行度上限（工作协程数）。
	Concurrency int
	// ChunkSize 分片字符数；为 0 时使用默认值。回复等短文本可传更小的值。
	ChunkSize int
	Persist   bool
}

// AuditResult 并行审核的完整结果，同时承载审核结论与并行性能指标。
type AuditResult struct {
	Verdict   Verdict `json:"verdict"`
	RiskScore int     `json:"risk_score"`
	Reason    string  `json:"reason"`

	RuleScore int         `json:"rule_score"`
	Hits      []rules.Hit `json:"hits"`
	Critical  bool        `json:"critical"`

	Dimensions []DimensionResult `json:"dimensions"`

	Stages    []StageTimeline `json:"stages"`
	ElapsedMs int64           `json:"elapsed_ms"`
	// SequentialMs 串行等价耗时：所有叶子任务耗时之和，即"不用并行要花多久"。
	SequentialMs int64 `json:"sequential_ms"`
	// CriticalPathMs 关键路径耗时：各顶层阶段耗时之和（阶段串行、阶段内并行）。
	CriticalPathMs int64 `json:"critical_path_ms"`
	// Speedup 加速比 = SequentialMs / ElapsedMs。
	Speedup         float64 `json:"speedup"`
	MaxConcurrency  int32   `json:"max_concurrency"`
	PeakConcurrency int     `json:"peak_concurrency"`

	Engine    string `json:"engine"`
	Mode      string `json:"mode"`
	Strategy  string `json:"strategy"`
	Workers   int    `json:"workers"`
	Chunks    int    `json:"chunks"`
	TaskCount int    `json:"task_count"`

	Degraded  bool      `json:"degraded"` // 是否因 AI 服务异常而降级
	CacheHit  bool      `json:"cache_hit"`
	CreatedAt time.Time `json:"created_at"`

	dimNames map[string]string
}

// AuditEvent SSE 进度事件。
type AuditEvent struct {
	ElapsedMs int64  `json:"elapsed_ms"`
	Stage     string `json:"stage"`
	State     string `json:"state"` // start / done / error
	Message   string `json:"message"`
	Data      any    `json:"data,omitempty"`
}

// AuditEventListener 事件回调。SSE 接口通过它把并行任务的实时进度推送给浏览器。
type AuditEventListener func(ev AuditEvent)

const (
	defaultConcurrency = 8
	maxConcurrency     = 16
	// chunkSize 单个分片的目标字符数。
	chunkSize = 420
	// auditTimeout 整条流水线的截止时间，超时后所有在途分支被取消。
	auditTimeout = 60 * time.Second
	// chunkCountThreshold 正文超过该长度才启用分片并行策略。
	chunkCountThreshold = 900
)

// ============================================================================
// 调度原语
// ============================================================================

// stageScheduler 负责把任务 fan-out 到固定数量的工作协程，并把结果 fan-in 回主协程。
//
// 采用"信号量 + 结果通道"的方式而非 WaitGroup+切片：语义审核任务是网络 IO 密集型，
// 用带缓冲的结果通道收集可以边完成边处理，也天然规避了并发写切片的数据竞争。
type stageScheduler struct {
	sem      chan struct{}
	peak     int32
	parallel bool
	mu       sync.Mutex
	listener AuditEventListener
	start    time.Time
	workerID int32
}

func newStageScheduler(concurrency int, start time.Time, listener AuditEventListener) *stageScheduler {
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	if concurrency > maxConcurrency {
		concurrency = maxConcurrency
	}
	return &stageScheduler{
		sem:      make(chan struct{}, concurrency),
		start:    start,
		listener: listener,
	}
}

func (s *stageScheduler) emit(stage, state, message string, data any) {
	if s.listener == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listener(AuditEvent{
		ElapsedMs: time.Since(s.start).Milliseconds(),
		Stage:     stage,
		State:     state,
		Message:   message,
		Data:      data,
	})
}

func (s *stageScheduler) rel() int64 {
	return time.Since(s.start).Milliseconds()
}

// relNs 返回相对流水线起点的纳秒数，用于精确测量并行重叠。
func (s *stageScheduler) relNs() int64 {
	return time.Since(s.start).Nanoseconds()
}

// mark 补齐任务轨迹的时间戳与耗时。
//
// 纳秒时间戳用于精确测量并行重叠；耗时先按纳秒测量、最后取整到毫秒，
// 避免毫秒级取整把亚毫秒任务统一压成 0 而低估串行等价耗时。
func (s *stageScheduler) mark(trace TaskTrace) TaskTrace {
	if trace.StartNs == 0 {
		trace.StartNs = s.relNs()
	}
	if trace.EndNs == 0 {
		trace.EndNs = s.relNs()
	}
	if trace.EndNs < trace.StartNs {
		trace.EndNs = trace.StartNs
	}
	if trace.Duration == 0 {
		trace.Duration = (trace.EndNs - trace.StartNs) / int64(time.Millisecond)
	}
	return trace
}

// fanOut 并行执行一组任务并收集结果，返回任务轨迹与实际并行度。
//
// 每个任务在独立协程中运行，最多 concurrency 个任务同时处于执行状态；
// 返回的 TaskTrace 记录了每个任务的起止时刻（相对流水线起点），前端据此绘制并行甘特图。
func (s *stageScheduler) fanOut(ctx context.Context, items []string, fn func(ctx context.Context, item string, worker int) TaskTrace) (traces []TaskTrace, parallel bool) {
	if len(items) == 0 {
		return nil, false
	}

	results := make(chan TaskTrace, len(items))
	var wg sync.WaitGroup
	var running int32
	parallel = true

	for _, item := range items {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()

			// 获取令牌：同时运行的任务数收敛到 concurrency，形成并行窗口。
			select {
			case s.sem <- struct{}{}:
			case <-ctx.Done():
				results <- TaskTrace{Name: item, Status: "error", Result: "任务取消"}
				return
			}

			cur := atomic.AddInt32(&running, 1)
			for {
				old := atomic.LoadInt32(&s.peak)
				if cur <= old || atomic.CompareAndSwapInt32(&s.peak, old, cur) {
					break
				}
			}
			worker := int(atomic.AddInt32(&s.workerID, 1)-1)%cap(s.sem) + 1

			trace := s.mark(fn(ctx, item, worker))

			atomic.AddInt32(&running, -1)
			<-s.sem
			results <- trace
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for t := range results {
		traces = append(traces, t)
	}
	sort.Slice(traces, func(i, j int) bool { return traces[i].StartMs < traces[j].StartMs })

	// 并行判定：时间线重叠（并行度 > 1）或峰值并发计数 > 1。
	// 两者取或：任务极快时时钟精度可能把一个并行窗口量化为同一时刻，
	// 此时用信号量并发计数兜底，避免把并行执行误判成串行。
	peak := int(atomic.LoadInt32(&s.peak))
	parallel = maxOverlap(traces) > 1 || peak > 1
	return traces, parallel
}

// maxOverlap 用扫描线计算任务时间区间的最大重叠数，即实际达到的并行度。
// 优先使用纳秒时间戳，缺失时退化为毫秒。
func maxOverlap(traces []TaskTrace) int {
	type point struct {
		t int64
		d int
	}
	points := make([]point, 0, len(traces)*2)
	for _, t := range traces {
		start, end := t.StartNs, t.EndNs
		if start == 0 && end == 0 {
			start, end = t.StartMs, t.EndMs
		}
		if end < start {
			end = start
		}
		points = append(points, point{start, 1}, point{end, -1})
	}
	sort.Slice(points, func(i, j int) bool {
		if points[i].t == points[j].t {
			// 同一时刻先处理结束事件，避免把"首尾相接"误判为重叠。
			return points[i].d < points[j].d
		}
		return points[i].t < points[j].t
	})
	cur, max := 0, 0
	for _, p := range points {
		cur += p.d
		if cur > max {
			max = cur
		}
	}
	return max
}

// splitChunks 把长正文按段落/句子边界切分为定长分片，并保留少量重叠，
// 避免把一句话从中间切断而丢失语义。
func splitChunks(content string, size int) []string {
	runes := []rune(strings.TrimSpace(content))
	if len(runes) == 0 {
		return nil
	}
	if size <= 0 {
		size = chunkSize
	}
	if len(runes) <= size {
		return []string{string(runes)}
	}

	var chunks []string
	for start := 0; start < len(runes); {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		// 向后寻找最近的句子边界，让分片尽量落在自然断句处。
		if end < len(runes) {
			for i := end; i > start+size/2; i-- {
				switch runes[i-1] {
				case '。', '！', '？', '\n', '；', '.', '!', '?', ';':
					end = i
					goto sliced
				}
			}
		}
	sliced:
		chunks = append(chunks, strings.TrimSpace(string(runes[start:end])))
		if end == len(runes) {
			break
		}
		// 重叠 40 字符，保证跨分片的语义连续性。
		start = end - 40
		if start < 0 {
			start = 0
		}
	}
	return chunks
}

// ============================================================================
// 并行审核主流程
// ============================================================================

// AuditContent 执行一次并行内容审核。
func AuditContent(ctx context.Context, req AuditRequest, listener AuditEventListener) (*AuditResult, error) {
	if strings.TrimSpace(req.Title) == "" && strings.TrimSpace(req.Content) == "" {
		return nil, errors.New("审核内容为空")
	}
	mode := req.Mode
	if mode == "" {
		mode = "semantic"
	}
	concurrency := req.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}

	start := time.Now()
	sched := newStageScheduler(concurrency, start, listener)

	pipelineCtx, cancel := context.WithTimeout(ctx, auditTimeout)
	defer cancel()

	chunks := []string{strings.TrimSpace(req.Title + "\n" + req.Content)}
	size := req.ChunkSize
	if size <= 0 {
		size = chunkSize
	}
	// reply 是语义审核的轻量变体：回复通常较短，用更小的分片即可完整覆盖，
	// 同时保留"规则快速通道 + 多维度并行"的完整拦截能力。
	explicitChunked := mode == "chunked"
	if mode == "reply" {
		mode = "semantic"
		if req.ChunkSize <= 0 {
			size = 220
		}
	}
	if explicitChunked || (mode == "semantic" && len([]rune(req.Content)) > chunkCountThreshold) {
		chunks = splitChunks(req.Content, size)
		if len(chunks) == 0 {
			chunks = []string{req.Title}
		}
		if explicitChunked {
			mode = "chunked"
		}
	}

	strategy := req.Strategy
	if strategy == "" {
		if mode == "chunked" {
			strategy = "chunk"
		} else {
			strategy = "dimension"
		}
	}

	sched.emit("pipeline", "start", fmt.Sprintf("并行审核启动：策略=%s，正文 %d 字，切分 %d 片，并行度 %d",
		strategy, len([]rune(req.Content)), len(chunks), concurrency), map[string]any{
		"strategy":    strategy,
		"chunks":      len(chunks),
		"concurrency": concurrency,
		"dimensions":  len(dimensionTable),
	})

	result := &AuditResult{
		Engine:    "parallel-engine",
		Mode:      mode,
		Strategy:  strategy,
		Chunks:    len(chunks),
		Workers:   concurrency,
		CreatedAt: start,
		dimNames:  map[string]string{},
	}
	for _, d := range dimensionTable {
		result.dimNames[d.Key] = d.Name
	}

	var (
		stages   []StageTimeline
		stagesMu sync.Mutex
	)
	collect := func(stage StageTimeline) {
		stagesMu.Lock()
		stages = append(stages, stage)
		stagesMu.Unlock()
	}

	// ruleOut 承载规则阶段的结论，由规则协程写入、主协程在 wg.Wait() 之后读取，
	// 用互斥锁保证跨协程可见性（WaitGroup 提供 happens-before，锁用于防御性保护）。
	var (
		ruleOut   rules.Result
		ruleOutMu sync.Mutex
	)

	var wg sync.WaitGroup
	// 规则引擎是本地 CPU 计算，即使语义审核走网络，两者也同时启动形成并行。
	wg.Add(1)
	go func() {
		defer wg.Done()
		stage, scanned := runRuleStage(sched, req)
		ruleOutMu.Lock()
		ruleOut = scanned
		ruleOutMu.Unlock()
		collect(stage)
	}()

	// 语义审核（含分片并发）。
	if mode == "semantic" || mode == "chunked" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dimStage, chunkStage, dims := runSemanticStages(pipelineCtx, sched, req, chunks, result.dimNames)
			collect(dimStage)
			if chunkStage != nil {
				collect(*chunkStage)
			}
			result.Dimensions = dims
		}()
	} else {
		result.Mode = "local"
	}

	wg.Wait()

	ruleOutMu.Lock()
	result.RuleScore = ruleOut.Score
	result.Hits = ruleOut.Hits
	result.Critical = ruleOut.Critical
	ruleOutMu.Unlock()

	aggregate(result)

	sort.Slice(stages, func(i, j int) bool { return stages[i].StartMs < stages[j].StartMs })
	result.Stages = stages

	result.ElapsedMs = time.Since(start).Milliseconds()
	if result.ElapsedMs <= 0 {
		result.ElapsedMs = 1
	}

	// 并行收益度量：
	//   sequential_ms = 所有叶子任务耗时之和 —— "如果串行执行需要多久"
	//   critical_path = 各顶层阶段耗时之和   —— 流水线关键路径（阶段之间串行，阶段内部并行）
	//   加速比 = sequential_ms / elapsed_ms
	//
	// 只累加叶子任务（而非顶层阶段）很关键：像"多维度语义审核"这一层的
	// 5 个 LLM 调用已经在阶段内部并行了，其阶段耗时约等于单次调用耗时，
	// 若用它计算会得出接近 1.0 的加速比，严重低估并行收益。
	leaf, taskCount := leafDuration(stages)
	result.SequentialMs = leaf
	result.CriticalPathMs = criticalPath(stages)
	result.TaskCount = taskCount
	if result.SequentialMs < result.ElapsedMs {
		result.SequentialMs = result.ElapsedMs
	}
	result.Speedup = round2(float64(result.SequentialMs) / float64(result.ElapsedMs))
	result.PeakConcurrency = int(atomic.LoadInt32(&sched.peak))
	result.MaxConcurrency = atomic.LoadInt32(&sched.peak)

	sched.emit("pipeline", "done", fmt.Sprintf("并行审核完成：%s（风险分 %d），实际耗时 %d ms，串行等价 %d ms，加速比 %.2fx，峰值并行度 %d",
		result.Reason, result.RiskScore, result.ElapsedMs, result.SequentialMs, result.Speedup, result.PeakConcurrency), result)

	if req.Persist {
		if err := persistAudit(req, result); err != nil {
			sched.emit("persist", "error", "审核记录写入失败: "+err.Error(), nil)
		}
	}
	return result, nil
}

// runRuleStage 执行本地规则扫描阶段，返回阶段时间线与结构化命中结果。
//
// 该阶段不访问网络，耗时通常远小于大模型审核，
// 因此在并行流水线中扮演"快速通道"：能第一时间给出高危内容的拦截信号。
func runRuleStage(sched *stageScheduler, req AuditRequest) (StageTimeline, rules.Result) {
	start := sched.rel()
	startNs := sched.relNs()

	scanned := rules.Scan(req.Title, req.Content)
	if req.Tags != "" {
		tagScan := rules.Scan("", req.Tags)
		if tagScan.Score > 0 {
			scanned.Score = min(100, scanned.Score+tagScan.Score)
			scanned.Hits = append(scanned.Hits, tagScan.Hits...)
			scanned.Critical = scanned.Critical || tagScan.Critical
		}
	}

	ruleCount, patternCount := rules.RuleCount()
	summary := rules.Summary(scanned.Hits)
	if summary == "" {
		summary = fmt.Sprintf("未命中敏感规则（%d 条规则 / %d 条正则）", ruleCount, patternCount)
	}

	end := sched.rel()
	endNs := sched.relNs()
	// 规则扫描通常远快于 1ms，按纳秒测量后取整，避免耗时被压成 0。
	duration := (endNs - startNs) / int64(time.Millisecond)

	trace := TaskTrace{
		Name:     "规则引擎扫描",
		StartMs:  start,
		EndMs:    end,
		Duration: duration,
		Result:   summary,
		Status:   "ok",
		Worker:   1,
		StartNs:  startNs,
		EndNs:    endNs,
	}
	stage := StageTimeline{
		Name:     "规则引擎（本地 CPU 快速通道）",
		Key:      "rules",
		StartMs:  start,
		EndMs:    end,
		Duration: duration,
		Workers:  1,
		Tasks:    []TaskTrace{trace},
		Result:   summary,
		Status:   "ok",
	}

	if scanned.Score > 0 {
		sched.emit("rules", "done", fmt.Sprintf("规则引擎命中 %d 条规则，风险分 %d", len(scanned.Hits), scanned.Score), map[string]any{
			"score": scanned.Score,
			"hits":  scanned.Hits,
		})
	} else {
		sched.emit("rules", "done", "规则引擎未命中敏感规则", map[string]any{"score": 0})
	}
	return stage, scanned
}

// runSemanticStages 并行执行语义审核阶段。
//
// 两种并行策略：
//   - dimension：把同一份内容按审核维度并行送审（5 路并行）
//   - chunk：长文先切分，各分片并行送审，分片内部再并行多维度（二维并行）
func runSemanticStages(ctx context.Context, sched *stageScheduler, req AuditRequest, chunks []string, dimNames map[string]string) (StageTimeline, *StageTimeline, []DimensionResult) {
	agg := newDimAggregator(dimNames)

	if len(chunks) > 1 {
		// ---- 分片并行 ----
		chunkStart := sched.rel()
		traces, parallel := sched.fanOut(ctx, chunks, func(ctx context.Context, chunk string, worker int) TaskTrace {
			idx := 0
			for i, c := range chunks {
				if c == chunk {
					idx = i
					break
				}
			}
			t0 := time.Now()
			startMs := sched.rel()

			// 分片内部再对多个维度并行送审，形成"分片 × 维度"二维并行。
			dimItems := make([]string, 0, len(dimensionTable))
			for _, d := range dimensionTable {
				dimItems = append(dimItems, d.Key)
			}
			dimTraces, _ := sched.fanOut(ctx, dimItems, func(ctx context.Context, dim string, w int) TaskTrace {
				dStart := sched.rel()
				res, err := AIAuditChunk(ctx, req.Title, chunk, idx, len(chunks), dim)
				if err != nil {
					agg.addDegraded(dim, truncate(err.Error(), 80))
					return TaskTrace{
						Name: fmt.Sprintf("分片%d·%s", idx+1, agg.name(dim)), StartMs: dStart, EndMs: sched.rel(),
						Duration: time.Since(t0).Milliseconds(), Result: "AI 服务异常", Status: "error", Worker: w,
					}
				}
				agg.add(dim, res)
				return TaskTrace{
					Name: fmt.Sprintf("分片%d·%s", idx+1, agg.name(dim)), StartMs: dStart, EndMs: sched.rel(),
					Duration: res.ElapsedMs, Result: agg.traceResult(dim, res), Status: "ok", Worker: w,
				}
			})

			return TaskTrace{
				Name:     fmt.Sprintf("分片 %d/%d", idx+1, len(chunks)),
				StartMs:  startMs,
				EndMs:    sched.rel(),
				Duration: time.Since(t0).Milliseconds(),
				Result:   fmt.Sprintf("片内 %d 个维度并行完成", len(dimTraces)),
				Status:   "ok",
				Worker:   worker,
			}
		})

		stage := StageTimeline{
			Name:     fmt.Sprintf("分片并行审核（%d 片，片内多维度再并行）", len(chunks)),
			Key:      "chunks",
			StartMs:  chunkStart,
			EndMs:    sched.rel(),
			Duration: sched.rel() - chunkStart,
			Workers:  len(traces),
			Tasks:    traces,
			Result:   fmt.Sprintf("%d 个分片并行送审", len(chunks)),
		}
		if parallel {
			stage.Status = "ok"
		}
		sched.emit("chunks", "done", fmt.Sprintf("%d 个文本分片并行审核完成", len(chunks)), map[string]any{"chunks": len(chunks)})
		return StageTimeline{
			Name:     "语义审核聚合（fan-in）",
			Key:      "aggregate",
			StartMs:  sched.rel(),
			EndMs:    sched.rel(),
			Duration: 1,
			Workers:  1,
			Result:   "分片结果加权聚合",
			Status:   "ok",
		}, &stage, agg.results()
	}

	// ---- 维度并行 ----
	dimStart := sched.rel()
	items := make([]string, 0, len(dimensionTable))
	for _, d := range dimensionTable {
		items = append(items, d.Key)
	}
	content := chunks[0]

	traces, parallel := sched.fanOut(ctx, items, func(ctx context.Context, dim string, worker int) TaskTrace {
		t0 := time.Now()
		startMs := sched.rel()
		res, err := AIAuditChunk(ctx, req.Title, content, 0, 1, dim)
		if err != nil {
			agg.addDegraded(dim, truncate(err.Error(), 80))
			return TaskTrace{
				Name: agg.name(dim), StartMs: startMs, EndMs: sched.rel(),
				Duration: time.Since(t0).Milliseconds(), Result: "AI 服务异常，已降级", Status: "error", Worker: worker,
			}
		}
		agg.add(dim, res)
		return TaskTrace{
			Name: agg.name(dim), StartMs: startMs, EndMs: sched.rel(),
			Duration: res.ElapsedMs, Result: agg.traceResult(dim, res), Status: "ok", Worker: worker,
		}
	})

	status := "ok"
	if !parallel && len(traces) > 1 {
		status = "degraded"
	}
	stage := StageTimeline{
		Name:     fmt.Sprintf("多维度语义审核（%d 路并行）", len(items)),
		Key:      "dimensions",
		StartMs:  dimStart,
		EndMs:    sched.rel(),
		Duration: sched.rel() - dimStart,
		Workers:  len(traces),
		Tasks:    traces,
		Result:   fmt.Sprintf("%d 个审核维度并行完成", len(traces)),
		Status:   status,
	}
	sched.emit("dimensions", "done", fmt.Sprintf("%d 个语义维度并行审核完成", len(traces)), map[string]any{"dimensions": len(traces)})
	return stage, nil, agg.results()
}

// dimAggregator 聚合多个分片/维度返回的语义审核结果。
// 由主协程串行调用，内部仍加锁以防御未来的并发调用。
type dimAggregator struct {
	mu       sync.Mutex
	scores   map[string]int
	verdict  map[string]Verdict
	reasons  map[string][]string
	hits     map[string][]string
	degraded map[string]bool
	elapsed  map[string]int64
	names    map[string]string
}

func newDimAggregator(names map[string]string) *dimAggregator {
	return &dimAggregator{
		scores:   map[string]int{},
		verdict:  map[string]Verdict{},
		reasons:  map[string][]string{},
		hits:     map[string][]string{},
		degraded: map[string]bool{},
		elapsed:  map[string]int64{},
		names:    names,
	}
}

func (a *dimAggregator) name(key string) string {
	if n, ok := a.names[key]; ok {
		return n
	}
	return key
}

func (a *dimAggregator) add(dim string, res *ChunkAuditResult) {
	if res == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	// 分片结果按维度取"最大风险"，即最坏分片决定整篇文章在该维度上的风险。
	if res.Score > a.scores[dim] {
		a.scores[dim] = res.Score
	}
	if rank(Verdict(res.Status)) > rank(a.verdict[dim]) {
		a.verdict[dim] = Verdict(res.Status)
	}
	if res.Score >= 60 {
		a.verdict[dim] = VerdictReject
	} else if res.Score >= 30 && a.verdict[dim] != VerdictReject {
		a.verdict[dim] = VerdictReview
	} else if a.verdict[dim] == "" {
		a.verdict[dim] = VerdictPass
	}
	if res.ElapsedMs > a.elapsed[dim] {
		a.elapsed[dim] = res.ElapsedMs
	}
	for _, r := range res.Reasons {
		if r != "" {
			a.reasons[dim] = appendUnique(a.reasons[dim], r)
		}
	}
	for _, h := range res.Hits {
		if h != "" {
			a.hits[dim] = appendUnique(a.hits[dim], h)
		}
	}
	if res.Degraded {
		a.degraded[dim] = true
	}
}

func (a *dimAggregator) addDegraded(dim, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.degraded[dim] = true
	a.reasons[dim] = appendUnique(a.reasons[dim], "AI 服务异常："+reason)
}

func (a *dimAggregator) traceResult(dim string, res *ChunkAuditResult) string {
	if res.Score == 0 {
		return "未发现违规"
	}
	if len(res.Reasons) > 0 {
		return truncate(strings.Join(res.Reasons, "；"), 60)
	}
	return fmt.Sprintf("风险分 %d", res.Score)
}

// results 输出稳定的维度结果列表（按维度表顺序）。
func (a *dimAggregator) results() []DimensionResult {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]DimensionResult, 0, len(dimensionTable))
	for _, d := range dimensionTable {
		score := a.scores[d.Key]
		if score > 100 {
			score = 100
		}
		verdict := a.verdict[d.Key]
		if verdict == "" {
			verdict = VerdictPass
		}
		out = append(out, DimensionResult{
			Key:       d.Key,
			Name:      d.Name,
			Score:     d.Score,
			RiskScore: score,
			Verdict:   string(verdict),
			Reasons:   a.reasons[d.Key],
			Hits:      a.hits[d.Key],
			Degraded:  a.degraded[d.Key],
			KO:        d.KO,
			ElapsedMs: a.elapsed[d.Key],
		})
	}
	return out
}

// aggregate 加权聚合规则引擎与各语义维度的结论，产出最终判定。
//
// 判定策略（可解释、可回溯）：
//  1. 规则引擎命中致命规则，或任一 KO 维度判定违规 → 直接拒绝；
//  2. 风险分 ≥ 60 或存在"待复核"维度 → 转人工复核；
//  3. 语义维度全部降级（AI 服务不可用）且规则无命中 → 转人工复核，绝不静默放行；
//  4. 其余情况通过。
func aggregate(result *AuditResult) {
	risk := result.RuleScore
	var reasons []string
	reviewNeeded := false

	if result.RuleScore > 0 {
		reasons = append(reasons, fmt.Sprintf("本地规则命中 %d 条（+%d 分）", len(result.Hits), result.RuleScore))
	}

	for i := range result.Dimensions {
		d := &result.Dimensions[i]
		risk += d.RiskScore
		if d.RiskScore > 0 {
			reasons = append(reasons, fmt.Sprintf("%s：风险分 %d", d.Name, d.RiskScore))
		}
		if d.Verdict == string(VerdictReview) {
			reviewNeeded = true
		}
	}

	if risk > 100 {
		risk = 100
	}
	result.RiskScore = risk

	koHit := false
	var koNames []string
	for _, d := range result.Dimensions {
		if d.KO && d.Verdict == string(VerdictReject) {
			koHit = true
			koNames = append(koNames, d.Name)
		}
	}

	switch {
	case result.Critical:
		result.Verdict = VerdictReject
		reasons = append([]string{"命中致命违规规则"}, reasons...)
	case koHit:
		result.Verdict = VerdictReject
		reasons = append([]string{fmt.Sprintf("触发一票否决维度：%s", strings.Join(koNames, "、"))}, reasons...)
	case risk >= 60:
		result.Verdict = VerdictReject
		reasons = append([]string{"综合风险分超过拒绝阈值 60"}, reasons...)
	case reviewNeeded || risk >= 30:
		result.Verdict = VerdictReview
		reasons = append([]string{"存在疑似违规内容，需人工复核"}, reasons...)
	case result.degradedAll():
		result.Verdict = VerdictReview
		result.Degraded = true
		reasons = append([]string{"AI 语义审核服务不可用，已转人工复核（不放行）"}, reasons...)
	default:
		result.Verdict = VerdictPass
		if len(reasons) == 0 {
			reasons = append(reasons, "未发现违规内容，规则与语义审核均通过")
		}
	}

	if result.degradedAny() && result.Verdict != VerdictPass {
		result.Degraded = true
	}
	result.Reason = strings.Join(reasons, "；")
}

func (r *AuditResult) degradedAll() bool {
	if len(r.Dimensions) == 0 {
		return false
	}
	for _, d := range r.Dimensions {
		if !d.Degraded {
			return false
		}
	}
	return true
}

func (r *AuditResult) degradedAny() bool {
	for _, d := range r.Dimensions {
		if d.Degraded {
			return true
		}
	}
	return false
}

// leafDuration 汇总所有叶子任务的耗时之和，即"把整条流水线串行执行一遍需要多久"。
//
// 约定：顶层阶段时间线只有两层——阶段（StageTimeline）与其叶子任务（Tasks）。
// 分片并行模式下的"分片"轨迹是展示用的中间层，其耗时已包含在阶段耗时里，
// 因此这里以阶段耗时为准，避免把分片耗时与其内部维度耗时重复累加。
func leafDuration(stages []StageTimeline) (int64, int) {
	var total int64
	count := 0
	for _, stage := range stages {
		if len(stage.Tasks) == 0 {
			total += stage.Duration
			count++
			continue
		}
		for _, task := range stage.Tasks {
			total += task.Duration
			count++
		}
	}
	return total, count
}

// criticalPath 顶层阶段耗时之和，代表"阶段串行、阶段内并行"的关键路径长度。
func criticalPath(stages []StageTimeline) int64 {
	var total int64
	for _, stage := range stages {
		total += stage.Duration
	}
	return total
}

// persistAudit 把审核结论与并行性能指标写入数据库，并同步文章状态。
func persistAudit(req AuditRequest, result *AuditResult) error {
	hitsJSON, _ := json.Marshal(result.Hits)
	stagesJSON, _ := json.Marshal(result.Stages)

	record := &model.AuditRecord{
		ArticleID:      req.ArticleID,
		UserID:         req.UserID,
		Title:          truncate(req.Title, 190),
		Verdict:        string(result.Verdict),
		RiskScore:      result.RiskScore,
		Reason:         truncate(result.Reason, 500),
		Engine:         result.Engine,
		Mode:           result.Mode,
		Workers:        result.Workers,
		Chunks:         result.Chunks,
		ElapsedMs:      result.ElapsedMs,
		SequentialMs:   result.SequentialMs,
		Speedup:        result.Speedup,
		MaxConcurrency: result.PeakConcurrency,
		Stages:         string(stagesJSON),
		Hits:           string(hitsJSON),
	}
	if err := repository.DB.Create(record).Error; err != nil {
		return err
	}

	if req.ArticleID == 0 {
		return nil
	}
	now := time.Now()
	updates := map[string]any{
		"audit_status":     string(result.Verdict),
		"audit_score":      result.RiskScore,
		"audit_reason":     truncate(result.Reason, 500),
		"audit_latency_ms": result.ElapsedMs,
		"audited_at":       now,
	}
	// 审核结论直接驱动文章状态机：通过即发布，其余保持不可见。
	switch result.Verdict {
	case VerdictPass:
		updates["status"] = model.StatusPublished
		updates["published_at"] = now
	case VerdictReview:
		updates["status"] = model.StatusReview
	default:
		updates["status"] = model.StatusRejected
	}
	return repository.DB.Model(&model.Article{}).Where("id = ?", req.ArticleID).Updates(updates).Error
}

func appendUnique(list []string, v string) []string {
	for _, item := range list {
		if item == v {
			return list
		}
	}
	return append(list, v)
}

func rank(v Verdict) int {
	switch v {
	case VerdictReject:
		return 3
	case VerdictReview:
		return 2
	case VerdictPass:
		return 1
	}
	return 0
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
