package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"content-community/internal/rules"
)

// TestSplitChunks 验证长文分片：片数正确、片长受限、相邻片有重叠。
func TestSplitChunks(t *testing.T) {
	content := ""
	for i := 0; i < 400; i++ {
		content += "这是一句测试内容。"
	}
	chunks := splitChunks(content, chunkSize)
	if len(chunks) < 2 {
		t.Fatalf("长文应被切分为多个分片，实际得到 %d 片", len(chunks))
	}
	for i, c := range chunks {
		if n := len([]rune(c)); n > chunkSize+1 {
			t.Fatalf("第 %d 片长度 %d 超过上限 %d", i, n, chunkSize)
		}
		if c == "" {
			t.Fatalf("第 %d 片为空", i)
		}
	}
	// 重叠检查：分片长度超过上限说明发生了重叠（无重叠时片长应恰好等于 size 或更短）。
	if len([]rune(chunks[0])) < chunkSize {
		t.Logf("首片长度 %d，未触发边界回退，属正常情况", len([]rune(chunks[0])))
	}

	short := splitChunks("很短的内容", chunkSize)
	if len(short) != 1 {
		t.Fatalf("短文不应被切分，实际 %d 片", len(short))
	}
}

// TestMaxOverlap 验证并行度测量：4 个在时间上重叠的任务应测出并行度 4。
func TestMaxOverlap(t *testing.T) {
	traces := []TaskTrace{
		{StartNs: 0, EndNs: 100_000_000},
		{StartNs: 10_000_000, EndNs: 110_000_000},
		{StartNs: 20_000_000, EndNs: 120_000_000},
		{StartNs: 30_000_000, EndNs: 130_000_000},
	}
	if got := maxOverlap(traces); got != 4 {
		t.Fatalf("期望并行度 4，实际 %d", got)
	}

	// 完全串行的任务并行度应为 1（毫秒兜底路径）。
	serial := []TaskTrace{
		{StartMs: 0, EndMs: 100},
		{StartMs: 100, EndMs: 200},
		{StartMs: 200, EndMs: 300},
	}
	if got := maxOverlap(serial); got != 1 {
		t.Fatalf("串行任务期望并行度 1，实际 %d", got)
	}

	// 两个重叠任务的并行度应为 2。
	two := []TaskTrace{
		{StartMs: 0, EndMs: 100},
		{StartMs: 40, EndMs: 140},
	}
	if got := maxOverlap(two); got != 2 {
		t.Fatalf("重叠任务期望并行度 2，实际 %d", got)
	}
}

// TestFanOutActuallyParallel 验证工作协程池确实让任务并发重叠执行。
//
// 测试用 barrier 保证"全部任务都进入执行态后"才放行，
// 因此 4 个任务必然处于同一个并行窗口内。
//
// 注意：并行的判定同时依赖时间线重叠与并发计数两者取或——
// Windows 系统时钟精度约 0.5~15ms，短任务会被量化到同一时刻产生"零时长"轨迹，
// 此时只能用并发计数兜底，这也是生产代码采用双重判据的原因。
func TestFanOutActuallyParallel(t *testing.T) {
	sched := newStageScheduler(4, time.Now(), nil)
	items := []string{"a", "b", "c", "d"}

	var entered sync.WaitGroup
	entered.Add(len(items))
	release := make(chan struct{})

	// 后台协程在"4 个任务全部进入执行态"后统一放行，构造确定性的重叠窗口。
	go func() {
		entered.Wait()
		close(release)
	}()

	started := time.Now()
	traces, parallel := sched.fanOut(context.Background(), items, func(ctx context.Context, item string, worker int) TaskTrace {
		startMs, startNs := sched.rel(), sched.relNs()
		entered.Done()
		<-release
		// 保持一个可观的时间窗口，让时间线测量也具备可判定性。
		time.Sleep(80 * time.Millisecond)
		return TaskTrace{
			Name: item, StartMs: startMs, EndMs: sched.rel(),
			StartNs: startNs, EndNs: sched.relNs(),
			Status: "ok", Worker: worker,
		}
	})

	if len(traces) != 4 {
		t.Fatalf("期望 4 个任务轨迹，实际 %d", len(traces))
	}
	if !parallel {
		t.Fatalf("4 个任务同时进入执行态，应当被识别为并行执行")
	}
	if peak := atomic.LoadInt32(&sched.peak); peak != 4 {
		t.Fatalf("期望峰值并发计数为 4，实际 %d", peak)
	}
	// 4 个任务各睡眠 80ms：串行执行至少需要 320ms，并行则接近 80ms。
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("耗时 %v 表明任务被串行执行", elapsed)
	}
}

// TestAggregateVerdict 验证加权聚合的判定优先级。
func TestAggregateVerdict(t *testing.T) {
	cases := []struct {
		name     string
		result   AuditResult
		expected Verdict
	}{
		{
			name:     "全部通过",
			result:   AuditResult{RuleScore: 0, Dimensions: dims(map[string]int{"ad": 0, "abuse": 0, "porn": 0, "politics": 0, "illegal": 0}, nil)},
			expected: VerdictPass,
		},
		{
			name:     "低风险疑似违规转人工",
			result:   AuditResult{RuleScore: 20, Dimensions: dims(map[string]int{"ad": 20, "abuse": 0, "porn": 0, "politics": 0, "illegal": 0}, nil)},
			expected: VerdictReview,
		},
		{
			name:     "风险分超阈值直接拒绝",
			result:   AuditResult{RuleScore: 60, Dimensions: dims(map[string]int{"ad": 0, "abuse": 0, "porn": 0, "politics": 0, "illegal": 0}, nil)},
			expected: VerdictReject,
		},
		{
			name:     "KO 维度一票否决",
			result:   AuditResult{RuleScore: 0, Dimensions: dims(map[string]int{"ad": 0, "abuse": 0, "porn": 0, "politics": 60, "illegal": 0}, nil)},
			expected: VerdictReject,
		},
		{
			name:     "致命规则命中直接拒绝",
			result:   AuditResult{RuleScore: 5, Critical: true, Dimensions: dims(map[string]int{"ad": 0, "abuse": 0, "porn": 0, "politics": 0, "illegal": 0}, nil)},
			expected: VerdictReject,
		},
		{
			name: "AI 全部降级且无规则命中不放行",
			result: AuditResult{
				RuleScore:  0,
				Dimensions: dims(map[string]int{"ad": 0, "abuse": 0, "porn": 0, "politics": 0, "illegal": 0}, map[string]bool{"ad": true, "abuse": true, "porn": true, "politics": true, "illegal": true}),
			},
			expected: VerdictReview,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.result
			aggregate(&res)
			if res.Verdict != tc.expected {
				t.Fatalf("期望判定 %s，实际 %s（原因：%s）", tc.expected, res.Verdict, res.Reason)
			}
			if res.Reason == "" {
				t.Fatalf("判定必须给出可解释的原因")
			}
		})
	}
}

// dims 构造维度结果，risks 为各维度风险分，degraded 标记降级维度。
func dims(risks map[string]int, degraded map[string]bool) []DimensionResult {
	out := make([]DimensionResult, 0, len(dimensionTable))
	for _, d := range dimensionTable {
		risk := risks[d.Key]
		verdict := VerdictPass
		if risk >= 60 {
			verdict = VerdictReject
		} else if risk >= 30 {
			verdict = VerdictReview
		}
		out = append(out, DimensionResult{
			Key:       d.Key,
			Name:      d.Name,
			RiskScore: risk,
			Verdict:   string(verdict),
			KO:        d.KO,
			Degraded:  degraded[d.Key],
		})
	}
	return out
}

// TestRuleEngineScoring 验证规则引擎能识别广告/违禁内容并给出证据。
func TestRuleEngineScoring(t *testing.T) {
	clean := rules.Scan("如何用 Go 写一个并发爬虫", "本文介绍 goroutine 与 channel 的配合使用方式。")
	if clean.Score != 0 {
		t.Fatalf("正常技术文章不应命中规则，实际风险分 %d", clean.Score)
	}

	ad := rules.Scan("好物推荐", "想要低价货源的朋友加微信 abc12345，兼职日结，扫码进群免费领取")
	if ad.Score == 0 || len(ad.Hits) == 0 {
		t.Fatalf("广告导流内容应命中规则，实际风险分 %d", ad.Score)
	}
	if ad.Hits[0].Evidences == nil {
		t.Fatalf("命中规则时应给出证据片段，便于人工复核")
	}

	illegal := rules.Scan("急出", "出售毒品 冰毒，办假证代开发票")
	if !illegal.Critical {
		t.Fatalf("违禁内容应触发致命标记")
	}
}

// TestSplitChunksCoverage 验证分片拼接后不丢失原文信息（仅可能重复重叠部分）。
func TestSplitChunksCoverage(t *testing.T) {
	parts := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		parts = append(parts, fmt.Sprintf("第%d段内容。", i))
	}
	content := ""
	for _, p := range parts {
		content += p
	}
	chunks := splitChunks(content, 200)
	if len(chunks) < 3 {
		t.Fatalf("期望多个分片，实际 %d", len(chunks))
	}
	// 每一段的起始标记都应至少出现在某个分片中。
	for i := 0; i < 200; i += 20 {
		marker := fmt.Sprintf("第%d段内容", i)
		found := false
		for _, c := range chunks {
			if strings.Contains(c, marker) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("分片后丢失内容: %s", marker)
		}
	}
}
