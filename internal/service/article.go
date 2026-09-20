package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"content-community/internal/model"
	"content-community/internal/repository"
	"content-community/internal/rules"
)

// ErrForbidden 越权访问（非作者本人）。
var ErrForbidden = errors.New("无权操作该文章")

// ArticleQuery 文章列表查询条件。
type ArticleQuery struct {
	Keyword  string
	AuthorID uint
	BoardID  uint
	Status   string
	Tag      string
	Page     int
	Size     int
	// Sort: last 最新回复 / new 最新发布 / hot 最多浏览 / ess 精华
	Sort string
	// ViewerID 当前登录用户，用于批量填充点赞状态（0 表示未登录）
	ViewerID uint
	// WithContent 列表场景默认不返回正文，减少传输体积。
	WithContent bool
}

// ArticlePage 分页结果。
type ArticlePage struct {
	Total int64           `json:"total"`
	Page  int             `json:"page"`
	Size  int             `json:"size"`
	Items []model.Article `json:"items"`
}

// CreateArticleDraft 创建文章草稿。发布必须经过 AI 审核，因此新建时状态固定为 draft。
//
// boardSlug 为空或未知时归入默认版块，保证发帖不会因为分类问题失败。
func CreateArticleDraft(title, content, tags, boardSlug string, authorID uint) (*model.Article, error) {
	article := &model.Article{
		Title:    strings.TrimSpace(title),
		Content:  content,
		Tags:     NormalizeTags(tags),
		AuthorID: authorID,
		BoardID:  ResolveBoardID(boardSlug),
		Status:   model.StatusDraft,
	}
	if err := repository.DB.Create(article).Error; err != nil {
		return nil, err
	}
	if err := repository.DB.Preload("Author").Preload("Board").First(article, article.ID).Error; err != nil {
		return article, nil
	}
	return article, nil
}

// GetArticleByID 读取文章详情。
//
// 读路径采用"缓存旁路 + 异步浏览量缓冲"：
// 命中 Redis 直接返回；未命中回源数据库并异步回填缓存；
// 浏览量先写入 Redis Hash 缓冲，由后台协程批量合并回数据库。
//
// viewerID 用于填充"当前用户是否已点赞"。该字段与用户相关，
// 因此不写入缓存，而是在每次请求返回前单独查询并覆盖。
func GetArticleByID(ctx context.Context, id uint, viewerID uint) (*model.Article, error) {
	cacheKey := fmt.Sprintf("article:%d", id)
	var article model.Article

	cached, err := repository.Redis.Get(ctx, cacheKey).Result()
	hit := false
	if err == nil && cached != "" {
		if jsonErr := json.Unmarshal([]byte(cached), &article); jsonErr == nil {
			hit = true
		}
	}

	if !hit {
		if err := repository.DB.Preload("Author").Preload("Board").First(&article, id).Error; err != nil {
			return nil, err
		}
		go func(a model.Article) {
			if data, err := json.Marshal(a); err == nil {
				repository.Redis.Set(context.Background(), cacheKey, data, time.Hour)
			}
		}(article)
	}

	// 点赞状态按请求上下文单独计算，避免把某一个用户的状态缓存给所有人。
	if viewerID > 0 {
		liked := likedTargets(ctx, viewerID, "article", []uint{article.ID})
		article.Liked = liked[article.ID]
	} else {
		article.Liked = false
	}

	go bufferViewCount(id)
	return &article, nil
}

// bufferViewCount 把一次阅读写入 Redis 浏览量缓冲与热榜 ZSet。
// 两个写操作放在独立协程中执行，不阻塞用户请求。
func bufferViewCount(articleID uint) {
	ctx := context.Background()
	idStr := strconv.FormatUint(uint64(articleID), 10)
	repository.Redis.HIncrBy(ctx, viewBufferKey, idStr, 1)
	repository.Redis.ZIncrBy(ctx, rankingKey, 1, idStr)
}

// ListArticles 分页查询文章列表。
func ListArticles(q ArticleQuery) (*ArticlePage, error) {
	if q.Page <= 0 {
		q.Page = 1
	}
	if q.Size <= 0 || q.Size > 50 {
		q.Size = 10
	}

	tx := repository.DB.Model(&model.Article{})
	if q.Keyword != "" {
		kw := "%" + q.Keyword + "%"
		tx = tx.Where("title LIKE ? OR content LIKE ?", kw, kw)
	}
	if q.AuthorID > 0 {
		tx = tx.Where("author_id = ?", q.AuthorID)
	}
	if q.BoardID > 0 {
		tx = tx.Where("board_id = ?", q.BoardID)
	}
	if q.Tag != "" {
		tx = tx.Where("tags LIKE ?", "%"+q.Tag+"%")
	}
	switch q.Status {
	case "published":
		tx = tx.Where("status = ?", model.StatusPublished)
	case "draft":
		tx = tx.Where("status = ?", model.StatusDraft)
	case "review":
		tx = tx.Where("status = ?", model.StatusReview)
	case "rejected":
		tx = tx.Where("status = ?", model.StatusRejected)
	case "all":
		// 不附加状态条件
	default:
		// 默认只看已发布内容
		tx = tx.Where("status = ?", model.StatusPublished)
	}
	// 精华筛选（论坛常见入口）
	if q.Sort == "ess" {
		tx = tx.Where("featured = ?", true)
	}

	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, err
	}

	page := &ArticlePage{Total: total, Page: q.Page, Size: q.Size, Items: []model.Article{}}
	if total == 0 {
		return page, nil
	}

	var articles []model.Article
	query := tx.Preload("Author").Preload("Board")
	// 排序：置顶帖永远在最前，其余按所选维度排列。
	switch q.Sort {
	case "new":
		query = query.Order("pinned DESC, id DESC")
	case "hot":
		query = query.Order("pinned DESC, view_count DESC, id DESC")
	case "ess":
		query = query.Order("pinned DESC, like_count DESC, id DESC")
	default: // last：按最后回复时间排序（论坛默认）
		query = query.Order("pinned DESC, COALESCE(last_reply_at, created_at) DESC, id DESC")
	}
	query = query.Offset((q.Page - 1) * q.Size).Limit(q.Size)
	if !q.WithContent {
		// 列表场景不拉取长正文：通过 Select 白名单列让数据库少传输大字段，显著降低 IO。
		query = query.Select("id, title, summary, tags, author_id, board_id, status, pinned, featured, " +
			"reply_count, view_count, like_count, audit_status, audit_score, audit_reason, audit_latency_ms, " +
			"audited_at, last_reply_at, published_at, created_at, updated_at")
	}
	if err := query.Find(&articles).Error; err != nil {
		return nil, err
	}
	// 批量填充"我是否点过赞"，避免前端逐条请求。
	MarkArticleLikes(context.Background(), q.ViewerID, articles)
	page.Items = articles
	return page, nil
}

// UpdateArticle 更新文章。仅作者本人可改；修改后需要重新审核才能再次发布。
func UpdateArticle(ctx context.Context, id, userID uint, title, content, tags, boardSlug string, isAdmin bool) (*model.Article, error) {
	var article model.Article
	if err := repository.DB.First(&article, id).Error; err != nil {
		return nil, err
	}
	if !isAdmin && article.AuthorID != userID {
		return nil, ErrForbidden
	}

	article.Title = strings.TrimSpace(title)
	article.Content = content
	if tags != "" {
		article.Tags = NormalizeTags(tags)
	}
	// 允许作者调整版块（发错版块是论坛的常见操作）。
	if boardSlug != "" {
		article.BoardID = ResolveBoardID(boardSlug)
	}
	// 内容变更后原审核结论失效，回到草稿状态等待重新审核。
	article.Status = model.StatusDraft
	article.AuditStatus = ""
	article.AuditReason = ""
	article.AuditScore = 0
	if err := repository.DB.Save(&article).Error; err != nil {
		return nil, err
	}
	invalidArticleCache(ctx, id)
	return &article, nil
}

// DeleteArticle 删除文章（软删除），仅作者或管理员可操作。
func DeleteArticle(ctx context.Context, id, userID uint, isAdmin bool) error {
	var article model.Article
	if err := repository.DB.First(&article, id).Error; err != nil {
		return err
	}
	if !isAdmin && article.AuthorID != userID {
		return ErrForbidden
	}
	if err := repository.DB.Delete(&model.Article{}, id).Error; err != nil {
		return err
	}
	invalidArticleCache(ctx, id)
	repository.Redis.ZRem(ctx, rankingKey, strconv.FormatUint(uint64(id), 10))
	return nil
}

// PublishArticle 触发"AI 审核 → 发布"闭环。
//
// 审核通过则文章直接进入 published 并写入热榜；疑似违规转人工复核；
// 判定违规则拒绝发布。整个过程一次请求完成，无需人工在后台逐条审核。
func PublishArticle(ctx context.Context, id, userID uint, isAdmin bool, mode string, listener AuditEventListener) (*model.Article, *AuditResult, error) {
	var article model.Article
	if err := repository.DB.First(&article, id).Error; err != nil {
		return nil, nil, err
	}
	if !isAdmin && article.AuthorID != userID {
		return nil, nil, ErrForbidden
	}
	if strings.TrimSpace(article.Title) == "" || strings.TrimSpace(article.Content) == "" {
		return nil, nil, errors.New("标题与正文不能为空")
	}

	result, err := AuditContent(ctx, AuditRequest{
		ArticleID:   article.ID,
		UserID:      userID,
		Title:       article.Title,
		Content:     article.Content,
		Tags:        article.Tags,
		Mode:        mode,
		Concurrency: defaultConcurrency,
		Persist:     true,
	}, listener)
	if err != nil {
		return nil, nil, err
	}

	if err := repository.DB.Preload("Author").Preload("Board").First(&article, id).Error; err != nil {
		return nil, result, err
	}
	invalidArticleCache(ctx, id)

	// 审核通过的已发布文章按其初始热度进入热榜，保证新内容有曝光机会。
	if result.Verdict == VerdictPass {
		repository.Redis.ZIncrBy(ctx, rankingKey, 1, strconv.FormatUint(uint64(article.ID), 10))
		// 论坛列表默认按"最后回复时间"排序，新帖需要初始化该字段才会出现在列表顶部。
		repository.DB.Model(&model.Article{}).Where("id = ? AND last_reply_at IS NULL", id).
			UpdateColumn("last_reply_at", time.Now())
		if publishErr := repository.DB.Preload("Author").Preload("Board").First(&article, id).Error; publishErr == nil {
			invalidArticleCache(ctx, id)
		}
	}
	return &article, result, nil
}

// ReauditArticle 重新审核（用户修改后可再次提交，或运营按新规则复查）。
func ReauditArticle(ctx context.Context, id, userID uint, isAdmin bool, mode string, listener AuditEventListener) (*model.Article, *AuditResult, error) {
	return PublishArticle(ctx, id, userID, isAdmin, mode, listener)
}

// BatchAudit 批量并行审核。
//
// 文章之间相互独立，因此用工作协程池并发处理：
// 单篇审核内部已经是并行的（规则/维度/分片），批处理在其上再叠加一层任务并行，
// 整体表现为"请求间并行 × 请求内并行"的两级并行结构。
func BatchAudit(ctx context.Context, articles []model.Article, userID uint, concurrency int) ([]map[string]any, int64, int64, float64) {
	if concurrency <= 0 || concurrency > 8 {
		concurrency = 4
	}
	start := time.Now()
	sem := make(chan struct{}, concurrency)
	type item struct {
		result map[string]any
		seq    int64
	}
	out := make(chan item, len(articles))
	var seqTotal int64

	for _, a := range articles {
		a := a
		go func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out <- item{result: map[string]any{"article_id": a.ID, "error": "已取消"}}
				return
			}
			defer func() { <-sem }()

			res, err := AuditContent(ctx, AuditRequest{
				ArticleID:   a.ID,
				UserID:      userID,
				Title:       a.Title,
				Content:     a.Content,
				Tags:        a.Tags,
				Mode:        "semantic",
				Concurrency: defaultConcurrency,
				Persist:     true,
			}, nil)
			if err != nil {
				out <- item{result: map[string]any{"article_id": a.ID, "title": a.Title, "error": err.Error()}}
				return
			}
			out <- item{
				seq: res.SequentialMs,
				result: map[string]any{
					"article_id":    a.ID,
					"title":         a.Title,
					"verdict":       string(res.Verdict),
					"risk_score":    res.RiskScore,
					"reason":        res.Reason,
					"elapsed_ms":    res.ElapsedMs,
					"sequential_ms": res.SequentialMs,
					"speedup":       res.Speedup,
					"status":        string(a.Status),
				},
			}
		}()
	}

	results := make([]map[string]any, 0, len(articles))
	for i := 0; i < len(articles); i++ {
		it := <-out
		seqTotal += it.seq
		if it.result != nil {
			results = append(results, it.result)
		}
	}

	elapsed := time.Since(start).Milliseconds()
	if elapsed <= 0 {
		elapsed = 1
	}
	return results, elapsed, seqTotal, round2(float64(seqTotal) / float64(elapsed))
}

// ListAuditRecords 查询审核记录（支持按文章过滤），用于审核详情页展示历史。
func ListAuditRecords(ctx context.Context, articleID uint, limit int) ([]model.AuditRecord, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	var records []model.AuditRecord
	tx := repository.DB.Model(&model.AuditRecord{}).Order("id DESC").Limit(limit)
	if articleID > 0 {
		tx = tx.Where("article_id = ?", articleID)
	}
	if err := tx.Find(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}

// AuditInsight 并行审核的性能统计，用于运营看板展示"并行计算带来的收益"。
type AuditInsight struct {
	TotalAudits     int64   `json:"total_audits"`
	AvgElapsedMs    float64 `json:"avg_elapsed_ms"`
	AvgSequentialMs float64 `json:"avg_sequential_ms"`
	AvgSpeedup      float64 `json:"avg_speedup"`
	MaxSpeedup      float64 `json:"max_speedup"`
	PeakConcurrency int     `json:"peak_concurrency"`
	PassRate        float64 `json:"pass_rate"`
	RejectRate      float64 `json:"reject_rate"`
	RuleCount       int     `json:"rule_count"`
	RulePatterns    int     `json:"rule_patterns"`
	Dimensions      int     `json:"dimensions"`
}

// AuditStats 汇总审核性能指标。
func AuditStats() (*AuditInsight, error) {
	type agg struct {
		Total    int64
		Elapsed  float64
		Seq      float64
		MaxSpeed float64
		Peak     int
		Passed   int64
		Rejected int64
	}
	var row agg
	err := repository.DB.Model(&model.AuditRecord{}).
		Select("COUNT(*) as total, COALESCE(AVG(elapsed_ms),0) as elapsed, COALESCE(AVG(sequential_ms),0) as seq, COALESCE(MAX(speedup),0) as max_speed, COALESCE(MAX(max_concurrency),0) as peak").
		Scan(&row).Error
	if err != nil {
		return nil, err
	}
	repository.DB.Model(&model.AuditRecord{}).Where("verdict = ?", string(VerdictPass)).Count(&row.Passed)
	repository.DB.Model(&model.AuditRecord{}).Where("verdict = ?", string(VerdictReject)).Count(&row.Rejected)

	ruleCount, patternCount := ruleCounts()
	insight := &AuditInsight{
		TotalAudits:     row.Total,
		AvgElapsedMs:    round2(row.Elapsed),
		AvgSequentialMs: round2(row.Seq),
		MaxSpeedup:      round2(row.MaxSpeed),
		PeakConcurrency: row.Peak,
		RuleCount:       ruleCount,
		RulePatterns:    patternCount,
		Dimensions:      len(dimensionTable),
	}
	if row.Elapsed > 0 {
		insight.AvgSpeedup = round2(row.Seq / row.Elapsed)
	}
	if row.Total > 0 {
		insight.PassRate = round2(float64(row.Passed) / float64(row.Total) * 100)
		insight.RejectRate = round2(float64(row.Rejected) / float64(row.Total) * 100)
	}
	return insight, nil
}

func invalidArticleCache(ctx context.Context, id uint) {
	repository.Redis.Del(ctx, fmt.Sprintf("article:%d", id))
}

// ManualReview 人工复核：AI 判定为 review 的文章由审核员做最终裁决。
//
// 这是"AI 辅助人工"的落点——AI 只负责把可疑内容筛出来并给出依据，
// 最终是否发布仍由人决定，避免模型误杀或漏放。
func ManualReview(ctx context.Context, id uint, decision, reason string) (*model.Article, error) {
	var article model.Article
	if err := repository.DB.First(&article, id).Error; err != nil {
		return nil, err
	}

	now := time.Now()
	updates := map[string]any{
		"audit_reason": fmt.Sprintf("人工复核：%s（%s）", decision, reason),
		"audited_at":   now,
	}
	switch decision {
	case "approve":
		updates["status"] = model.StatusPublished
		updates["audit_status"] = string(VerdictPass)
		updates["published_at"] = now
	case "reject":
		updates["status"] = model.StatusRejected
		updates["audit_status"] = string(VerdictReject)
	default:
		return nil, errors.New("复核结论只能是 approve 或 reject")
	}

	if err := repository.DB.Model(&model.Article{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return nil, err
	}
	invalidArticleCache(ctx, id)
	if decision == "approve" {
		repository.Redis.ZIncrBy(ctx, rankingKey, 1, strconv.FormatUint(uint64(id), 10))
	}
	if err := repository.DB.Preload("Author").First(&article, id).Error; err != nil {
		return nil, err
	}
	return &article, nil
}

// ruleCounts 暴露规则引擎规模，供运营看板展示"审核引擎覆盖了多少规则"。
func ruleCounts() (int, int) {
	return rules.RuleCount()
}

// RuleStats 返回规则条数与正则条数。
func RuleStats() (int, int) {
	return ruleCounts()
}

// MaxParallelism 并行度上限。
func MaxParallelism() int { return maxConcurrency }

// DefaultParallelism 默认并行度。
func DefaultParallelism() int { return defaultConcurrency }

// FindArticleBrief 轻量读取文章元信息（不触发浏览量统计与缓存写入），
// 供审核请求补充标签等上下文。
func FindArticleBrief(id uint, out *model.Article) error {
	return repository.DB.Select("id, title, tags, author_id, status").First(out, id).Error
}

// SaveArticleSummary 持久化 AI 生成的摘要，列表页可直接复用。
func SaveArticleSummary(ctx context.Context, id uint, summary string) {
	repository.DB.Model(&model.Article{}).Where("id = ?", id).
		UpdateColumn("summary", summary)
	invalidArticleCache(ctx, id)
}
