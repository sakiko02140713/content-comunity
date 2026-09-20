package service

import (
	"errors"
	"time"

	"content-community/internal/model"
	"content-community/internal/repository"
)

// ErrNotFound 通用"资源不存在"错误。
var ErrNotFound = errors.New("资源不存在")

// boardDefinitions 固定内置版块定义。
//
// 采用代码内置而非后台可配置：版块数量与定位在社区初期相对稳定，
// 内置可以保证任何一次部署（含全新数据库）都得到一致的版块结构。
var boardDefinitions = []model.Board{
	{Slug: "tech", Name: "技术交流", Icon: "💻", SortOrder: 1,
		Description: "编程语言、架构设计、并发与性能优化的经验分享"},
	{Slug: "create", Name: "创作分享", Icon: "✍️", SortOrder: 2,
		Description: "写作心得、内容运营与个人创作记录"},
	{Slug: "ai", Name: "AI 与内容安全", Icon: "🤖", SortOrder: 3,
		Description: "大模型应用、内容审核与社区治理的讨论"},
	{Slug: "ask", Name: "提问求助", Icon: "❓", SortOrder: 4,
		Description: "遇到问题来这里发帖，社区同学一起帮你解决"},
	{Slug: "notice", Name: "社区公告", Icon: "📢", SortOrder: 5,
		Description: "社区规则、版本更新与官方通知"},
	{Slug: "chat", Name: "灌水闲聊", Icon: "☕", SortOrder: 6,
		Description: "轻松话题，随便聊聊"},
}

// DefaultBoardSlug 发帖未指定版块时的默认归属。
const DefaultBoardSlug = "chat"

// SeedBoards 保证内置版块存在（按 slug 幂等写入），并返回全部版块。
func SeedBoards() ([]model.Board, error) {
	for i := range boardDefinitions {
		def := boardDefinitions[i]
		var existing model.Board
		err := repository.DB.Where("slug = ?", def.Slug).First(&existing).Error
		if err == nil {
			// 已存在则同步名称/描述，保证文案更新能生效。
			repository.DB.Model(&model.Board{}).Where("id = ?", existing.ID).Updates(map[string]any{
				"name":        def.Name,
				"description": def.Description,
				"icon":        def.Icon,
				"sort_order":  def.SortOrder,
			})
			continue
		}
		if err := repository.DB.Create(&def).Error; err != nil {
			return nil, err
		}
	}
	return ListBoards()
}

// ListBoards 返回全部版块（含统计）。
func ListBoards() ([]model.Board, error) {
	var boards []model.Board
	if err := repository.DB.Order("sort_order ASC, id ASC").Find(&boards).Error; err != nil {
		return nil, err
	}
	return boards, nil
}

// ListBoardStats 返回带统计的版块列表，用于论坛首页的版块分组展示。
//
// 每个版块的帖子数/回复数/今日新帖彼此独立，因此按版块并发统计后汇总，
// 避免逐版块串行查询造成的累计延迟。
func ListBoardStats() ([]model.BoardStat, error) {
	boards, err := ListBoards()
	if err != nil {
		return nil, err
	}
	if len(boards) == 0 {
		return []model.BoardStat{}, nil
	}

	type counts struct {
		threads int64
		replies int64
		today   int64
	}
	results := make([]counts, len(boards))
	dayStart := time.Now().Truncate(24 * time.Hour)

	done := make(chan int, len(boards))
	for i, b := range boards {
		go func(idx int, boardID uint) {
			defer func() { done <- idx }()
			var c counts
			repository.DB.Model(&model.Article{}).
				Where("board_id = ? AND status = ?", boardID, model.StatusPublished).
				Count(&c.threads)
			repository.DB.Model(&model.Reply{}).
				Where("article_id IN (SELECT id FROM articles WHERE board_id = ? AND status = ? AND deleted_at IS NULL)",
					boardID, model.StatusPublished).
				Count(&c.replies)
			repository.DB.Model(&model.Article{}).
				Where("board_id = ? AND status = ? AND created_at >= ?", boardID, model.StatusPublished, dayStart).
				Count(&c.today)
			results[idx] = c
		}(i, b.ID)
	}
	for range boards {
		<-done
	}

	stats := make([]model.BoardStat, 0, len(boards))
	for i, b := range boards {
		stats = append(stats, model.BoardStat{
			Board:       b,
			ThreadCount: results[i].threads,
			ReplyCount:  results[i].replies,
			TodayCount:  results[i].today,
		})
	}
	return stats, nil
}

// ResolveBoardID 把 slug 解析为版块 ID；slug 为空或未知时回退到默认版块。
func ResolveBoardID(slug string) uint {
	if slug == "" {
		slug = DefaultBoardSlug
	}
	var board model.Board
	if err := repository.DB.Where("slug = ?", slug).First(&board).Error; err != nil {
		// 未知版块也回退到默认版块，避免发帖因分类问题失败。
		if err := repository.DB.Where("slug = ?", DefaultBoardSlug).First(&board).Error; err != nil {
			return 0
		}
	}
	return board.ID
}

// BoardBySlug 按 slug 查询版块。
func BoardBySlug(slug string) (*model.Board, error) {
	var board model.Board
	if err := repository.DB.Where("slug = ?", slug).First(&board).Error; err != nil {
		return nil, ErrNotFound
	}
	return &board, nil
}
