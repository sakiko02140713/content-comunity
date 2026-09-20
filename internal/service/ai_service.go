package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// aiServiceURL Python AI 服务的地址，可通过环境变量覆盖（默认适配 docker-compose 网络）。
var aiServiceURL = func() string {
	if v := os.Getenv("AI_SERVICE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://ai-service:8000"
}()

// aiClient 复用连接池的共享 HTTP 客户端。
// 并行审核会同时发起多个请求，连接复用可以避免每个请求都重新握手。
var aiClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	},
}

type AIGenerateResponse struct {
	GeneratedContent string `json:"generated_content"`
	Model            string `json:"model"`
	ElapsedMs        int64  `json:"elapsed_ms"`
}

type AISummaryResponse struct {
	Answer    string `json:"answer"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Mode      string `json:"mode"`
}

type AIAuditContentResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// ChunkAuditResult 单次分片语义审核结果。
type ChunkAuditResult struct {
	Index     int      `json:"index"`
	Status    string   `json:"status"` // pass / review / reject
	Score     int      `json:"score"`
	Reasons   []string `json:"reasons"`
	Hits      []string `json:"hits"`
	Degraded  bool     `json:"degraded"`
	ElapsedMs int64    `json:"elapsed_ms"`
}

// ParallelSummaryResult Python 服务端 map-reduce 并行总结结果。
type ParallelSummaryResult struct {
	Answer     string   `json:"answer"`
	Chunks     []string `json:"chunk_summaries"`
	ChunkCount int      `json:"chunk_count"`
	ElapsedMs  int64    `json:"elapsed_ms"`
	Mode       string   `json:"mode"`
}

// postJSON 向 AI 服务发起一次可取消的 JSON 请求。
// 使用 context 而非固定超时，是为了让上层并行流水线统一控制截止时间：
// 上游超时后所有在途分片请求会被立即取消，避免 goroutine 泄漏。
func postJSON(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, aiServiceURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := aiClient.Do(req)
	if err != nil {
		return fmt.Errorf("调用 AI 服务失败: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取 AI 服务响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("AI 服务返回 %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("解析 AI 服务响应失败: %w", err)
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// AIGenerateArticle 调用大语言模型辅助创作文章。
func AIGenerateArticle(title, outline, style string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var result AIGenerateResponse
	err := postJSON(ctx, "/api/ai/generate", map[string]any{
		"title":   title,
		"content": outline,
		"style":   style,
	}, &result)
	if err != nil {
		return "", err
	}
	return result.GeneratedContent, nil
}

// AISummaryHotlist 让大模型分析社区热榜趋势。
func AISummaryHotlist(articles []map[string]any, question string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var result AISummaryResponse
	err := postJSON(ctx, "/api/ai/summary", map[string]any{
		"articles": articles,
		"question": question,
	}, &result)
	if err != nil {
		return "", err
	}
	return result.Answer, nil
}

// AISummarizeArticle 对单篇长文做 map-reduce 并行总结：Python 服务内部把正文切片后并发总结，
// 再并发归并成最终摘要。
func AISummarizeArticle(ctx context.Context, title, content string) (*ParallelSummaryResult, error) {
	var result ParallelSummaryResult
	err := postJSON(ctx, "/api/ai/article-summary", map[string]any{
		"title":   title,
		"content": content,
	}, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// AIAuditChunk 对单个文本分片做多维度语义审核。
// dimension 参数用于把审核意图拆成独立请求，使 Go 侧可以把不同维度并行分发。
func AIAuditChunk(ctx context.Context, title, chunk string, index, total int, dimension string) (*ChunkAuditResult, error) {
	var result ChunkAuditResult
	err := postJSON(ctx, "/api/ai/audit-chunk", map[string]any{
		"title":     title,
		"chunk":     chunk,
		"index":     index,
		"total":     total,
		"dimension": dimension,
	}, &result)
	if err != nil {
		return nil, err
	}
	result.Index = index
	return &result, nil
}

// AIAuditContent 兼容接口：单次整体审核（旧版前端直接调用该能力）。
func AIAuditContent(title, content string) (status, reason string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result AIAuditContentResponse
	if err := postJSON(ctx, "/api/ai/audit", map[string]any{
		"title":   title,
		"content": content,
	}, &result); err != nil {
		return "reject", "审核服务不可用", err
	}
	return result.Status, result.Reason, nil
}

// AIHealth 探测 AI 服务是否可用，供运营监控页展示。
func AIHealth(ctx context.Context) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, aiServiceURL+"/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := aiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}
