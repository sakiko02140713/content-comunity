package service

import (
	"context"
	"strconv"
	"time"

	"content-community/internal/model"
	"content-community/internal/repository"
)

// ForumOverview 论坛首页概览：一次请求返回站点数字、版块统计与推荐帖子。
//
// 四个子任务彼此独立（站点统计 / 版块统计 / 最新帖子 / 热门帖子），
// 因此并发执行后 fan-in，避免首页串行发起多次查询。
type ForumOverview struct {
	Stats        ForumStats        `json:"stats"`
	Boards       []model.BoardStat `json:"boards"`
	Latest       []model.Article   `json:"latest"`
	Hot          []model.Article   `json:"hot"`
	ElapsedMs    int64             `json:"elapsed_ms"`
	SequentialMs int64             `json:"sequential_ms"`
	Speedup      float64           `json:"speedup"`
	Tasks        []TaskTrace       `json:"tasks"`
}

// ForumStats 站点级统计（论坛首页展示）。
type ForumStats struct {
	Threads    int64 `json:"threads"`
	Replies    int64 `json:"replies"`
	Members    int64 `json:"members"`
	TodayPosts int64 `json:"today_posts"`
	TodayUsers int64 `json:"today_users"`
}

// BuildForumOverview 并发构建论坛首页数据。
func BuildForumOverview(ctx context.Context, viewerID uint, boardSize int) (*ForumOverview, error) {
	if boardSize <= 0 || boardSize > 20 {
		boardSize = 5
	}
	start := time.Now()

	var (
		boards []model.BoardStat
		latest []model.Article
		hot    []model.Article
		stats  ForumStats
		tasks  []TaskTrace
		seq    int64
	)

	type taskResult struct {
		name     string
		startMs  int64
		duration int64
		result   string
		err      error
	}
	results := make(chan taskResult, 4)

	// 子任务 1：版块统计
	go func() {
		t0 := time.Now()
		startMs := time.Since(start).Milliseconds()
		b, err := ListBoardStats()
		boards = b
		results <- taskResult{"版块统计", startMs, time.Since(t0).Milliseconds(),
			strconv.Itoa(len(b)) + " 个版块", err}
	}()

	// 子任务 2：最新帖子（按发布时间）
	go func() {
		t0 := time.Now()
		startMs := time.Since(start).Milliseconds()
		page, err := ListArticles(ArticleQuery{
			Status: "published", Sort: "new", Page: 1, Size: 10, ViewerID: viewerID,
		})
		if err == nil && page != nil {
			latest = page.Items
		}
		results <- taskResult{"最新帖子", startMs, time.Since(t0).Milliseconds(),
			strconv.Itoa(len(latest)) + " 条", err}
	}()

	// 子任务 3：热门帖子（按浏览量）
	go func() {
		t0 := time.Now()
		startMs := time.Since(start).Milliseconds()
		hotList, err := loadHotArticles(ctx, 10)
		if err == nil {
			for _, h := range hotList {
				hot = append(hot, h.Article)
			}
			MarkArticleLikes(ctx, viewerID, hot)
		}
		results <- taskResult{"热门帖子", startMs, time.Since(t0).Milliseconds(),
			strconv.Itoa(len(hot)) + " 条", err}
	}()

	// 子任务 4：站点统计
	go func() {
		t0 := time.Now()
		startMs := time.Since(start).Milliseconds()
		s, err := loadForumStats()
		stats = s
		results <- taskResult{"站点统计", startMs, time.Since(t0).Milliseconds(), "统计完成", err}
	}()

	for i := 0; i < 4; i++ {
		r := <-results
		if r.err != nil {
			return nil, r.err
		}
		tasks = append(tasks, TaskTrace{
			Name: r.name, StartMs: r.startMs, EndMs: time.Since(start).Milliseconds(),
			Duration: r.duration, Result: r.result, Status: statusOf(r.err),
		})
		seq += r.duration
	}

	elapsed := time.Since(start).Milliseconds()
	if elapsed <= 0 {
		elapsed = 1
	}
	sequential := maxInt64(seq, elapsed)
	return &ForumOverview{
		Stats:        stats,
		Boards:       boards,
		Latest:       latest,
		Hot:          hot,
		ElapsedMs:    elapsed,
		SequentialMs: sequential,
		Speedup:      round2(float64(sequential) / float64(elapsed)),
		Tasks:        tasks,
	}, nil
}

func loadForumStats() (ForumStats, error) {
	var stats ForumStats
	dayStart := time.Now().Truncate(24 * time.Hour)

	err := repository.DB.Model(&model.Article{}).
		Where("status = ?", model.StatusPublished).Count(&stats.Threads).Error
	if err != nil {
		return stats, err
	}
	repository.DB.Model(&model.Reply{}).Count(&stats.Replies)
	repository.DB.Model(&model.User{}).Count(&stats.Members)
	repository.DB.Model(&model.Article{}).
		Where("status = ? AND created_at >= ?", model.StatusPublished, dayStart).Count(&stats.TodayPosts)
	repository.DB.Model(&model.User{}).Where("created_at >= ?", dayStart).Count(&stats.TodayUsers)
	return stats, nil
}

// UpdateArticleFlags 更新帖子的置顶/加精/版块归属（审核员操作）。
func UpdateArticleFlags(ctx context.Context, id uint, updates map[string]any) (*model.Article, error) {
	var article model.Article
	if err := repository.DB.First(&article, id).Error; err != nil {
		return nil, ErrNotFound
	}
	if err := repository.DB.Model(&model.Article{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return nil, err
	}
	invalidArticleCache(ctx, id)
	if err := repository.DB.Preload("Author").Preload("Board").First(&article, id).Error; err != nil {
		return nil, err
	}
	return &article, nil
}

func statusOf(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
