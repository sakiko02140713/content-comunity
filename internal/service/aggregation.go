package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"content-community/internal/model"
	"content-community/internal/repository"
	"gorm.io/gorm"
)

// hotKeyPrefix 与热点聚合相关的 Redis Key。
const (
	rankingKey      = "article:ranking" // ZSet：文章热度排行
	viewBufferKey   = "article:views"   // Hash：浏览量缓冲，定时批量回写数据库
	hotSnapshotKey  = "hot:snapshot"    // String：热点聚合结果缓存
	viewFlushPeriod = 60 * time.Second  // 浏览量回写周期
	hotCacheTTL     = 30 * time.Second  // 热点快照缓存时间
)

// HotArticle 热点条目。在文章基础上附加了排名与热度分，
// 热度分 = 浏览量 × 0.7 + 点赞量 × 0.3，用于 Redis 不可用时兜底排序。
type HotArticle struct {
	model.Article
	Rank  int   `json:"rank"`
	Score int64 `json:"hot_score"`
}

// ArticleStats 社区内容概览统计。
type ArticleStats struct {
	Total     int64 `json:"total"`
	Published int64 `json:"published"`
	Draft     int64 `json:"draft"`
	Review    int64 `json:"review"`
	Rejected  int64 `json:"rejected"`
	Views     int64 `json:"views"`
}

// AggregationResult 热点聚合结果，附带并行执行的性能指标。
type AggregationResult struct {
	Hot          []HotArticle `json:"hot"`
	Stats        ArticleStats `json:"stats"`
	Tags         []TagCount   `json:"tags"`
	ElapsedMs    int64        `json:"elapsed_ms"`
	SequentialMs int64        `json:"sequential_ms"`
	Speedup      float64      `json:"speedup"`
	Tasks        []TaskTrace  `json:"tasks"`
	GeneratedAt  time.Time    `json:"generated_at"`
}

// TagCount 标签热度。
type TagCount struct {
	Tag   string `json:"tag"`
	Count int64  `json:"count"`
}

// AggregateHotContent 并行聚合热点内容。
//
// 三个数据子任务彼此独立（热榜榜单、状态统计、标签聚合），
// 因此放入独立协程并发执行，最后在主协程 fan-in 组装：
// 这是"并行计算"在业务查询侧的落地——把串行的多次数据库/缓存往返压缩为一次并行往返。
func AggregateHotContent(ctx context.Context, limit int) (*AggregationResult, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	start := time.Now()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		tasks []TaskTrace
		seq   int64
	)

	record := func(name string, t0 time.Time, startMs int64, result string, err error) {
		status := "ok"
		if err != nil {
			status = "error"
		}
		mu.Lock()
		tasks = append(tasks, TaskTrace{
			Name:     name,
			StartMs:  startMs,
			EndMs:    time.Since(start).Milliseconds(),
			Duration: time.Since(t0).Milliseconds(),
			Result:   result,
			Status:   status,
		})
		seq += time.Since(t0).Milliseconds()
		mu.Unlock()
	}

	var (
		hot     []HotArticle
		stats   ArticleStats
		tags    []TagCount
		hotErr  error
		statErr error
		tagErr  error
	)

	// 子任务 1：热榜榜单（优先读 Redis ZSet，回源数据库）
	wg.Add(1)
	go func() {
		defer wg.Done()
		t0 := time.Now()
		startMs := time.Since(start).Milliseconds()
		hot, hotErr = loadHotArticles(ctx, limit)
		record("热榜榜单查询（Redis ZSet + 数据库回源）", t0, startMs,
			fmt.Sprintf("返回 %d 条热点文章", len(hot)), hotErr)
	}()

	// 子任务 2：文章状态统计（一条聚合 SQL 拿到全部计数）
	wg.Add(1)
	go func() {
		defer wg.Done()
		t0 := time.Now()
		startMs := time.Since(start).Milliseconds()
		stats, statErr = loadArticleStats()
		record("内容状态统计（聚合查询）", t0, startMs,
			fmt.Sprintf("已发布 %d / 待复核 %d / 已拒绝 %d", stats.Published, stats.Review, stats.Rejected), statErr)
	}()

	// 子任务 3：标签热度聚合（内存聚合，CPU 密集）
	wg.Add(1)
	go func() {
		defer wg.Done()
		t0 := time.Now()
		startMs := time.Since(start).Milliseconds()
		tags, tagErr = loadTagCounts(ctx, 12)
		record("标签热度聚合", t0, startMs, fmt.Sprintf("统计 %d 个标签", len(tags)), tagErr)
	}()

	wg.Wait()

	for _, err := range []error{hotErr, statErr, tagErr} {
		if err != nil {
			return nil, err
		}
	}

	sort.Slice(tasks, func(i, j int) bool { return tasks[i].StartMs < tasks[j].StartMs })

	elapsed := time.Since(start).Milliseconds()
	if elapsed <= 0 {
		elapsed = 1
	}
	return &AggregationResult{
		Hot:          hot,
		Stats:        stats,
		Tags:         tags,
		ElapsedMs:    elapsed,
		SequentialMs: seq,
		Speedup:      round2(float64(seq) / float64(elapsed)),
		Tasks:        tasks,
		GeneratedAt:  time.Now(),
	}, nil
}

// loadHotArticles 读取热榜。热度数据来自 Redis ZSet；若 ZSet 为空（例如首次部署），
// 则以数据库浏览量的加权分兜底，保证首屏不为空。
func loadHotArticles(ctx context.Context, limit int) ([]HotArticle, error) {
	ids, err := repository.Redis.ZRevRange(ctx, rankingKey, 0, int64(limit-1)).Result()
	if err == nil && len(ids) > 0 {
		var articles []model.Article
		if err := repository.DB.Preload("Author").
			Where("id IN ? AND status = ?", ids, model.StatusPublished).
			Find(&articles).Error; err != nil {
			return nil, err
		}
		byID := make(map[uint]model.Article, len(articles))
		for _, a := range articles {
			byID[a.ID] = a
		}
		hot := make([]HotArticle, 0, len(ids))
		for _, idStr := range ids {
			id, convErr := strconv.ParseUint(idStr, 10, 32)
			if convErr != nil {
				continue
			}
			a, ok := byID[uint(id)]
			if !ok {
				continue
			}
			hot = append(hot, HotArticle{
				Article: a,
				Rank:    len(hot) + 1,
				Score:   a.ViewCount*7/10 + a.LikeCount*3/10,
			})
		}
		if len(hot) > 0 {
			return hot, nil
		}
	}

	// 兜底：按浏览量排序。
	var articles []model.Article
	if err := repository.DB.Preload("Author").
		Where("status = ?", model.StatusPublished).
		Order("view_count DESC, id DESC").
		Limit(limit).Find(&articles).Error; err != nil {
		return nil, err
	}
	hot := make([]HotArticle, 0, len(articles))
	for i, a := range articles {
		hot = append(hot, HotArticle{
			Article: a,
			Rank:    i + 1,
			Score:   a.ViewCount*7/10 + a.LikeCount*3/10,
		})
	}
	return hot, nil
}

func loadArticleStats() (ArticleStats, error) {
	var stats ArticleStats
	type row struct {
		Status string
		Count  int64
		Views  int64
	}
	var rows []row
	err := repository.DB.Model(&model.Article{}).
		Select("status, COUNT(*) as count, COALESCE(SUM(view_count),0) as views").
		Group("status").Scan(&rows).Error
	if err != nil {
		return stats, err
	}
	for _, r := range rows {
		stats.Total += r.Count
		stats.Views += r.Views
		switch model.ArticleStatus(r.Status) {
		case model.StatusPublished:
			stats.Published = r.Count
		case model.StatusDraft:
			stats.Draft = r.Count
		case model.StatusReview:
			stats.Review = r.Count
		case model.StatusRejected:
			stats.Rejected = r.Count
		}
	}
	return stats, nil
}

// loadTagCounts 从已发布文章中聚合标签热度。
// 标签以逗号分隔存储在 articles.tags 字段中，这里在应用层做一次并发友好的内存聚合。
func loadTagCounts(ctx context.Context, topN int) ([]TagCount, error) {
	var tagsList []string
	if err := repository.DB.Model(&model.Article{}).
		Where("status = ? AND tags <> ''", model.StatusPublished).
		Pluck("tags", &tagsList).Error; err != nil {
		return nil, err
	}

	counts := map[string]int64{}
	for _, raw := range tagsList {
		for _, tag := range splitTags(raw) {
			counts[tag]++
		}
	}
	out := make([]TagCount, 0, len(counts))
	for tag, count := range counts {
		out = append(out, TagCount{Tag: tag, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			return out[i].Tag < out[j].Tag
		}
		return out[i].Count > out[j].Count
	})
	if len(out) > topN {
		out = out[:topN]
	}
	return out, nil
}

// SummarizeHotParallel 对热榜文章并行生成摘要。
//
// map 阶段：每篇文章的摘要生成彼此独立，用工作协程池并发执行；
// reduce 阶段：把各篇摘要汇总后交给大模型产出社区级别的整体总结。
// 相比串行逐篇总结，耗时从 N×T 降到约 T + 归并开销。
func SummarizeHotParallel(ctx context.Context, articles []HotArticle, question string, concurrency int) (map[string]any, error) {
	if len(articles) == 0 {
		return map[string]any{"answer": "当前社区暂无已发布文章"}, nil
	}
	if concurrency <= 0 || concurrency > 8 {
		concurrency = 4
	}

	start := time.Now()
	type mapResult struct {
		title   string
		summary string
		elapsed int64
		err     error
	}

	sem := make(chan struct{}, concurrency)
	results := make(chan mapResult, len(articles))
	var wg sync.WaitGroup
	var seq int64

	for _, a := range articles {
		a := a
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results <- mapResult{title: a.Title, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()

			t0 := time.Now()
			res, err := AISummarizeArticle(ctx, a.Title, a.Content)
			elapsed := time.Since(t0).Milliseconds()
			atomicAddInt64(&seq, elapsed)

			out := mapResult{title: a.Title, elapsed: elapsed, err: err}
			if err == nil && res != nil {
				out.summary = res.Answer
			}
			results <- out
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	summaries := make([]map[string]any, 0, len(articles))
	byTitle := make(map[string]string, len(articles))
	for r := range results {
		if r.err != nil {
			continue
		}
		summaries = append(summaries, map[string]any{
			"title":   r.title,
			"summary": r.summary,
		})
		byTitle[r.title] = r.summary
	}

	// reduce：把并行产出的单篇摘要归并成社区级总结。
	articleData := make([]map[string]any, 0, len(articles))
	for _, a := range articles {
		articleData = append(articleData, map[string]any{
			"title":      a.Title,
			"view_count": a.ViewCount,
			"author":     authorName(a.Article),
			"summary":    byTitle[a.Title],
		})
	}

	if question == "" {
		question = "请总结当前社区热榜的内容趋势，并推荐最值得阅读的文章"
	}
	answer, err := AISummaryHotlist(articleData, question)
	if err != nil {
		return nil, err
	}

	elapsed := time.Since(start).Milliseconds()
	if elapsed <= 0 {
		elapsed = 1
	}
	return map[string]any{
		"answer":        answer,
		"summaries":     summaries,
		"article_count": len(articles),
		"elapsed_ms":    elapsed,
		"sequential_ms": seq,
		"speedup":       round2(float64(seq) / float64(elapsed)),
		"strategy":      "map-reduce-parallel",
		"concurrency":   concurrency,
	}, nil
}

func pickSummary(list []map[string]any, title string) string {
	for _, m := range list {
		if m["title"] == title {
			if s, ok := m["summary"].(string); ok {
				return s
			}
		}
	}
	return ""
}

func authorName(a model.Article) string {
	if a.Author != nil && a.Author.Username != "" {
		return a.Author.Username
	}
	return "匿名"
}

// FlushViewCounts 把 Redis 中缓冲的浏览量批量回写数据库，并同步热榜分值。
//
// 写合并策略：阅读产生的浏览量先累加在 Redis Hash，每 60 秒批量回写一次数据库，
// 把高频随机写压缩为低频批量写；热榜分值走 Redis ZSet，读侧始终是 O(log N) 的并行读。
func FlushViewCounts(ctx context.Context) (int, error) {
	buffered, err := repository.Redis.HGetAll(ctx, viewBufferKey).Result()
	if err != nil || len(buffered) == 0 {
		return 0, err
	}
	type pending struct {
		id    uint
		idStr string
		delta int64
	}
	items := make([]pending, 0, len(buffered))
	for idStr, delta := range buffered {
		raw, convErr := strconv.ParseUint(idStr, 10, 32)
		if convErr != nil {
			continue
		}
		deltaInt, _ := strconv.ParseInt(delta, 10, 64)
		if deltaInt <= 0 {
			continue
		}
		items = append(items, pending{id: uint(raw), idStr: idStr, delta: deltaInt})
	}
	if len(items) == 0 {
		return 0, nil
	}

	// 回写本身按文章并发执行，避免逐条串行等待数据库往返。
	var wg sync.WaitGroup
	var flushed int64
	sem := make(chan struct{}, 4)
	for _, it := range items {
		it := it
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if err := repository.DB.Model(&model.Article{}).Where("id = ?", it.id).
				UpdateColumn("view_count", gorm.Expr("view_count + ?", it.delta)).Error; err != nil {
				return
			}
			repository.Redis.HIncrBy(ctx, viewBufferKey, it.idStr, -it.delta)
			repository.Redis.ZIncrBy(ctx, rankingKey, float64(it.delta), it.idStr)
			atomic.AddInt64(&flushed, 1)
		}()
	}
	wg.Wait()
	return int(flushed), nil
}

// StartViewCountFlusher 启动后台定时回写协程。
func StartViewCountFlusher(ctx context.Context) {
	ticker := time.NewTicker(viewFlushPeriod)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = FlushViewCounts(ctx)
			}
		}
	}()
}

// splitTags 解析逗号分隔的标签串。
func splitTags(raw string) []string {
	var out []string
	current := make([]rune, 0, 16)
	flush := func() {
		tag := string(current)
		current = current[:0]
		if tag != "" {
			out = append(out, tag)
		}
	}
	for _, r := range raw {
		switch r {
		case ',', '，', ';', '；', '|', ' ':
			flush()
		default:
			current = append(current, r)
		}
	}
	flush()
	return out
}

// NormalizeTags 把用户输入的标签串规范化为"逗号分隔、去重、限长"的形式。
func NormalizeTags(raw string) string {
	tags := splitTags(raw)
	seen := map[string]bool{}
	uniq := make([]string, 0, len(tags))
	for _, t := range tags {
		if seen[t] || len([]rune(t)) > 20 {
			continue
		}
		seen[t] = true
		uniq = append(uniq, t)
		if len(uniq) >= 8 {
			break
		}
	}
	out := ""
	for i, t := range uniq {
		if i > 0 {
			out += ","
		}
		out += t
	}
	return out
}

func atomicAddInt64(addr *int64, delta int64) {
	atomic.AddInt64(addr, delta)
}
