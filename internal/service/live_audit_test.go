package service

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// TestLiveAuditPipeline 仅在设置 AI_SERVICE_URL 时运行：
// 对真实 Python AI 服务跑一次完整并行审核，验证 Go 侧编排（fan-out / fan-in /
// 加速比计算 / 判定聚合）与 Python 侧接口契约完全一致。
//
//	go test ./internal/service/ -run TestLiveAuditPipeline -v
func TestLiveAuditPipeline(t *testing.T) {
	if os.Getenv("AI_SERVICE_URL") == "" {
		t.Skip("未设置 AI_SERVICE_URL，跳过联调测试")
	}

	events := 0
	result, err := AuditContent(context.Background(), AuditRequest{
		Title:       "如何用 Go 做并行内容审核",
		Content:     "本文介绍 goroutine worker pool、channel fan-in 以及分片并行的实现方式，并给出性能度量方法。",
		Mode:        "semantic",
		Concurrency: 8,
		Persist:     false,
	}, func(ev AuditEvent) {
		events++
		fmt.Printf("  [事件] %5dms %-10s %s\n", ev.ElapsedMs, ev.Stage, ev.Message)
	})
	if err != nil {
		t.Fatalf("审核失败: %v", err)
	}

	fmt.Printf("\n判定=%s 风险分=%d 原因=%s\n", result.Verdict, result.RiskScore, result.Reason)
	fmt.Printf("阶段数=%d 任务数=%d 分片=%d\n", len(result.Stages), result.TaskCount, result.Chunks)
	fmt.Printf("实际耗时=%dms 串行等价=%dms 加速比=%.2fx 峰值并行度=%d\n",
		result.ElapsedMs, result.SequentialMs, result.Speedup, result.PeakConcurrency)

	for _, s := range result.Stages {
		fmt.Printf("  阶段 %-28s 起止=[%4d,%4d]ms 耗时=%4dms 任务=%d 状态=%s\n",
			s.Name, s.StartMs, s.EndMs, s.Duration, len(s.Tasks), s.Status)
		for _, task := range s.Tasks {
			fmt.Printf("      └ %-22s 起止=[%4d,%4d]ms 耗时=%4dms worker=%d %s\n",
				task.Name, task.StartMs, task.EndMs, task.Duration, task.Worker, task.Result)
		}
	}
	for _, d := range result.Dimensions {
		fmt.Printf("  维度 %-6s 风险分=%3d 判定=%-6s 耗时=%4dms 原因=%v\n",
			d.Name, d.RiskScore, d.Verdict, d.ElapsedMs, d.Reasons)
	}

	// ---------- 契约与并行性断言 ----------
	if len(result.Dimensions) != len(dimensionTable) {
		t.Fatalf("期望 %d 个审核维度，实际 %d", len(dimensionTable), len(result.Dimensions))
	}
	if result.ElapsedMs <= 0 {
		t.Fatalf("实际耗时必须为正数")
	}
	if result.SequentialMs <= 0 {
		t.Fatalf("串行等价耗时必须为正数")
	}
	if result.Speedup <= 0 {
		t.Fatalf("加速比必须为正数")
	}
	if events == 0 {
		t.Fatalf("应至少收到 1 个进度事件（SSE 实时推送依赖该回调）")
	}
	// 规则阶段与语义阶段应并发启动：两者的启动时刻应非常接近。
	var ruleStart, dimStart int64 = -1, -1
	for _, s := range result.Stages {
		switch s.Key {
		case "rules":
			ruleStart = s.StartMs
		case "dimensions", "chunks":
			dimStart = s.StartMs
		}
	}
	if ruleStart >= 0 && dimStart >= 0 {
		diff := ruleStart - dimStart
		if diff < 0 {
			diff = -diff
		}
		fmt.Printf("\n规则阶段与语义阶段的启动时间差: %dms（越小说明并行度越高）\n", diff)
		if diff > 200 {
			t.Fatalf("规则阶段与语义阶段应几乎同时启动，实际相差 %dms", diff)
		}
	}
	fmt.Println("\n✅ Go 并行编排与 Python AI 服务联调通过")
}
