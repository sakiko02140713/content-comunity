package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"content-community/internal/model"
	"content-community/internal/service"

	"github.com/gin-gonic/gin"
)

type generateReq struct {
	Title   string `json:"title" binding:"required"`
	Content string `json:"content"`
	Outline string `json:"outline"`
	Style   string `json:"style"`
}

// AIGenerate AI 辅助创作：根据主题/大纲生成文章正文。
func AIGenerate(c *gin.Context) {
	var req generateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先填写文章主题"})
		return
	}
	outline := req.Outline
	if outline == "" {
		outline = req.Content
	}
	if strings.TrimSpace(outline) == "" {
		outline = "请围绕该主题生成一篇结构完整、观点清晰的社区文章"
	}

	content, err := service.AIGenerateArticle(req.Title, outline, req.Style)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"title":             req.Title,
		"generated_content": content,
	})
}

// AISummary 热榜并行总结：先在 Go 侧并行生成各篇摘要，再归并成社区级总结。
func AISummary(c *gin.Context) {
	var req struct {
		Question string `json:"question"`
		Limit    int    `json:"limit"`
	}
	_ = c.ShouldBindJSON(&req)

	limit := req.Limit
	if limit <= 0 {
		limit = queryInt(c, "limit", 6, 12)
	}

	agg, err := service.AggregateHotContent(c.Request.Context(), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取热榜数据失败"})
		return
	}
	if len(agg.Hot) == 0 {
		c.JSON(http.StatusOK, gin.H{"question": req.Question, "answer": "当前社区尚无已发布文章，暂时无法生成总结", "article_count": 0})
		return
	}

	result, err := service.SummarizeHotParallel(c.Request.Context(), agg.Hot, req.Question, 4)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	result["question"] = req.Question
	c.JSON(http.StatusOK, result)
}

// AISummarizeArticle 单篇文章总结（Python 服务内部 map-reduce 并行）。
func AISummarizeArticle(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的文章 ID"})
		return
	}
	article, err := service.GetArticleByID(c.Request.Context(), id, userID(c))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "文章不存在"})
		return
	}
	result, err := service.AISummarizeArticle(c.Request.Context(), article.Title, article.Content)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	// 摘要落库，列表页可直接展示，无需重复调用大模型。
	if result.Answer != "" {
		service.SaveArticleSummary(c.Request.Context(), article.ID, result.Answer)
	}
	c.JSON(http.StatusOK, gin.H{
		"article_id": article.ID,
		"title":      article.Title,
		"summary":    result.Answer,
		"chunks":     result.ChunkCount,
		"elapsed_ms": result.ElapsedMs,
		"mode":       result.Mode,
		"strategy":   "map-reduce-parallel",
	})
}

type auditReq struct {
	Title       string `json:"title"`
	Content     string `json:"content" binding:"required"`
	Mode        string `json:"mode"`
	Strategy    string `json:"strategy"`
	Concurrency int    `json:"concurrency"`
}

// AIAuditInfo 返回审核引擎的能力描述：维度、权重、并行度上限等。
func AIAuditInfo(c *gin.Context) {
	dims := service.Dimensions()
	ruleCount, patternCount := service.RuleStats()
	count := int64(0)
	if stats, err := service.AuditStats(); err == nil {
		count = stats.TotalAudits
	}
	c.JSON(http.StatusOK, gin.H{
		"engine":           "Go goroutine worker-pool + rules + LLM",
		"dimensions":       dims,
		"rule_count":       ruleCount,
		"rule_patterns":    patternCount,
		"max_parallel":     service.MaxParallelism(),
		"default_parallel": service.DefaultParallelism(),
		"total_audits":     count,
		"modes": []gin.H{
			{"key": "local", "name": "本地规则审核", "desc": "仅使用敏感词与正则规则，毫秒级返回"},
			{"key": "semantic", "name": "规则 + 多维度语义并行", "desc": "5 个审核维度并行送审大模型"},
			{"key": "chunked", "name": "长文分片并发审核", "desc": "长文切分后分片并行，片内维度再并行"},
		},
	})
}

// AIAudit 一次完整的并行审核（同步返回全部结果与性能指标）。
func AIAudit(c *gin.Context) {
	req, ok := bindAuditReq(c)
	if !ok {
		return
	}
	result, err := service.AuditContent(c.Request.Context(), req, nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

// AIAuditStream 并行审核的实时过程推送（Server-Sent Events）。
//
// 审核流水线中每个并行分支完成时都会回调 listener，
// 这里把事件即时 flush 给浏览器，前端据此绘制各分支的并行执行时间线（甘特图）。
func AIAuditStream(c *gin.Context) {
	req, ok := bindAuditReq(c)
	if !ok {
		return
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	c.Writer.Flush()

	send := func(event string, payload any) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, data)
		c.Writer.Flush()
	}

	ctx := c.Request.Context()
	listener := func(ev service.AuditEvent) {
		send("progress", ev)
	}

	// 心跳协程：长时间等待大模型返回时保持连接活跃，避免被网关掐断。
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case t := <-ticker.C:
				fmt.Fprintf(c.Writer, ": heartbeat %d\n\n", t.UnixMilli())
				c.Writer.Flush()
			}
		}
	}()

	result, err := service.AuditContent(ctx, req, listener)
	close(done)

	if err != nil {
		send("error", gin.H{"error": err.Error()})
		return
	}
	send("result", result)
}

// AIBatchAudit 批量并行审核：对草稿/待复核文章做一次性并行复审。
func AIBatchAudit(c *gin.Context) {
	var req struct {
		Status      string `json:"status"`
		Limit       int    `json:"limit"`
		Concurrency int    `json:"concurrency"`
	}
	_ = c.ShouldBindJSON(&req)
	if req.Limit <= 0 || req.Limit > 20 {
		req.Limit = 10
	}
	status := req.Status
	if status == "" {
		status = "draft"
	}

	page, err := service.ListArticles(service.ArticleQuery{
		Status:      status,
		Page:        1,
		Size:        req.Limit,
		WithContent: true,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取待审核文章失败"})
		return
	}
	if len(page.Items) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"items":   []any{},
			"message": fmt.Sprintf("没有状态为 %s 的文章需要审核", status),
			"total":   0,
		})
		return
	}

	items, elapsed, seq, speedup := service.BatchAudit(c.Request.Context(), page.Items, userID(c), req.Concurrency)
	c.JSON(http.StatusOK, gin.H{
		"items":         items,
		"total":         len(items),
		"elapsed_ms":    elapsed,
		"sequential_ms": seq,
		"speedup":       speedup,
		"strategy":      "batch-parallel × inner-parallel",
		"concurrency":   req.Concurrency,
	})
}

// AIStats 并行计算与审核运营统计。
func AIStats(c *gin.Context) {
	insight, err := service.AuditStats()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "统计失败"})
		return
	}
	agg, aggErr := service.AggregateHotContent(c.Request.Context(), 5)
	payload := gin.H{"audit": insight}
	if aggErr == nil {
		payload["content"] = gin.H{
			"stats":         agg.Stats,
			"tags":          agg.Tags,
			"elapsed_ms":    agg.ElapsedMs,
			"sequential_ms": agg.SequentialMs,
			"speedup":       agg.Speedup,
			"tasks":         agg.Tasks,
		}
	}
	c.JSON(http.StatusOK, payload)
}

// AIHealth AI 服务健康检查（用于部署自检与运营看板）。
func AIHealth(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	health, err := service.AIHealth(ctx)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status": "unavailable",
			"error":  err.Error(),
			"hint":   "请确认 Python AI 服务已启动（默认 http://ai-service:8000）",
		})
		return
	}
	c.JSON(http.StatusOK, health)
}

func bindAuditReq(c *gin.Context) (service.AuditRequest, bool) {
	var body auditReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供待审核的标题与正文"})
		return service.AuditRequest{}, false
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = "未命名内容"
	}
	mode := body.Mode
	if mode == "" {
		mode = c.DefaultQuery("mode", "semantic")
	}
	concurrency := body.Concurrency
	if concurrency <= 0 {
		concurrency = service.DefaultParallelism()
	}

	req := service.AuditRequest{
		Title:       title,
		Content:     body.Content,
		Mode:        mode,
		Strategy:    body.Strategy,
		Concurrency: concurrency,
		UserID:      userID(c),
		Persist:     false,
	}

	// 若携带文章 ID，则同步文章状态并写入审核记录。
	if articleID := uint(queryInt(c, "article_id", 0, 0)); articleID > 0 {
		req.ArticleID = articleID
		req.Persist = true
		var article model.Article
		if err := service.FindArticleBrief(articleID, &article); err == nil {
			req.Tags = article.Tags
		}
	}
	return req, true
}
