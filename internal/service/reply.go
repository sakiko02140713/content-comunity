package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"content-community/internal/model"
	"content-community/internal/repository"
)

// ---------------------------------------------------------------------------
// 回复（楼层）
// ---------------------------------------------------------------------------

// maxReplyLength 单条回复的长度上限，防止刷屏式长文回复。
const maxReplyLength = 2000

// CreateReplyResult 回复创建结果，附带该条回复的审核结论。
//
// 设计取舍：回复的 AI 审核走"轻量流水线"（mode=reply）——
// 规则引擎快速通道 + 语义维度并行，但分片更小、并行度更低，
// 在保证拦截效果的同时把单次回复的等待控制在秒级。
type CreateReplyResult struct {
	Reply   *model.Reply `json:"reply"`
	Audit   *AuditResult `json:"audit"`
	Blocked bool         `json:"blocked"`
	Message string       `json:"message"`
}

// CreateReply 发表回复，并通过 AI 审核决定是否可见。
//
// 审核策略与发帖一致：违规直接拦截（不展示），疑似放行但标记待复核。
// 回复场景更强调"不打断交流"，因此疑似内容仍然展示，只附加提示。
func CreateReply(ctx context.Context, articleID, parentID, userID uint, content string) (*CreateReplyResult, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, errors.New("回复内容不能为空")
	}
	if len([]rune(content)) > maxReplyLength {
		return nil, fmt.Errorf("回复内容不能超过 %d 字", maxReplyLength)
	}

	var article model.Article
	if err := repository.DB.Select("id, title, status").First(&article, articleID).Error; err != nil {
		return nil, ErrNotFound
	}
	if article.Status != model.StatusPublished {
		return nil, errors.New("该帖子尚未发布，暂时无法回复")
	}

	// 二级结构校验：父回复必须属于同一帖子。
	if parentID > 0 {
		var parent model.Reply
		if err := repository.DB.First(&parent, parentID).Error; err != nil {
			return nil, errors.New("引用的回复不存在")
		}
		if parent.ArticleID != articleID {
			return nil, errors.New("引用的回复不属于该帖子")
		}
	}

	audit, err := AuditContent(ctx, AuditRequest{
		ArticleID:   articleID,
		UserID:      userID,
		Title:       "回复：" + article.Title,
		Content:     content,
		Mode:        "reply",
		Concurrency: 5,
		Persist:     false, // 回复审核不写入文章审核记录表
	}, nil)
	if err != nil {
		return nil, err
	}

	reply := &model.Reply{
		ArticleID:   articleID,
		ParentID:    parentID,
		AuthorID:    userID,
		Content:     content,
		AuditStatus: string(audit.Verdict),
		AuditReason: truncateForColumn(audit.Reason, 500),
	}

	result := &CreateReplyResult{Audit: audit}
	switch audit.Verdict {
	case VerdictReject:
		// 违规回复不入库，直接反馈给用户，避免污染帖子内容。
		result.Blocked = true
		result.Message = "回复未通过 AI 审核，已拦截：" + audit.Reason
		return result, nil
	default:
		reply.Visible = true
		if audit.Verdict == VerdictReview {
			result.Message = "回复已发布，AI 判定疑似违规，已标记待人工复核"
		} else {
			result.Message = "回复成功"
		}
	}

	// 楼层号在主楼场景下自增；楼中楼不再编号。
	if parentID == 0 {
		var maxFloor int
		repository.DB.Model(&model.Reply{}).
			Where("article_id = ? AND parent_id = 0", articleID).
			Select("COALESCE(MAX(floor), 0)").Scan(&maxFloor)
		reply.Floor = maxFloor + 1
	}

	if err := repository.DB.Create(reply).Error; err != nil {
		return nil, err
	}

	now := time.Now()
	repository.DB.Model(&model.Article{}).Where("id = ?", articleID).Updates(map[string]any{
		"reply_count":   repository.DB.Raw("reply_count + 1"),
		"last_reply_at": now,
	})
	if err := repository.DB.Preload("Author").First(reply, reply.ID).Error; err != nil {
		return nil, err
	}
	result.Reply = reply

	// 回复数变化会影响详情缓存中的计数，这里主动失效。
	invalidArticleCache(ctx, articleID)
	return result, nil
}

// ReplyNode 回复树节点：主楼 + 其下的楼中楼。
type ReplyNode struct {
	Reply   model.Reply  `json:"reply"`
	Replies []ReplyBrief `json:"children"`
}

// ReplyBrief 楼中楼的精简结构。
type ReplyBrief struct {
	ID        uint        `json:"id"`
	ParentID  uint        `json:"parent_id"`
	Content   string      `json:"content"`
	Floor     int         `json:"floor"`
	LikeCount int64       `json:"like_count"`
	Liked     bool        `json:"liked"`
	Author    *model.User `json:"author,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
}

// ReplyPage 回复分页结果。
type ReplyPage struct {
	Total int64       `json:"total"`
	Page  int         `json:"page"`
	Size  int         `json:"size"`
	Items []ReplyNode `json:"items"`
}

// ListReplies 查询帖子的回复（两层结构：主楼 + 楼中楼）。
//
// viewerID 用于标记"当前用户是否点过赞"，未登录传 0。
func ListReplies(ctx context.Context, articleID uint, page, size int, viewerID uint) (*ReplyPage, error) {
	if page <= 0 {
		page = 1
	}
	if size <= 0 || size > 50 {
		size = 20
	}

	var total int64
	repository.DB.Model(&model.Reply{}).
		Where("article_id = ? AND parent_id = 0", articleID).Count(&total)

	pageResult := &ReplyPage{Total: total, Page: page, Size: size, Items: []ReplyNode{}}
	if total == 0 {
		return pageResult, nil
	}

	var roots []model.Reply
	if err := repository.DB.Preload("Author").
		Where("article_id = ? AND parent_id = 0", articleID).
		Order("floor ASC, id ASC").
		Offset((page - 1) * size).Limit(size).
		Find(&roots).Error; err != nil {
		return nil, err
	}

	rootIDs := make([]uint, 0, len(roots))
	for _, r := range roots {
		rootIDs = append(rootIDs, r.ID)
	}

	var children []model.Reply
	if len(rootIDs) > 0 {
		repository.DB.Preload("Author").
			Where("article_id = ? AND parent_id IN ?", articleID, rootIDs).
			Order("id ASC").
			Find(&children)
	}

	childMap := map[uint][]ReplyBrief{}
	for _, c := range children {
		childMap[c.ParentID] = append(childMap[c.ParentID], ReplyBrief{
			ID:        c.ID,
			ParentID:  c.ParentID,
			Content:   visibleContent(c),
			Floor:     c.Floor,
			LikeCount: c.LikeCount,
			Author:    c.Author,
			CreatedAt: c.CreatedAt,
		})
	}

	for _, r := range roots {
		kids := childMap[r.ID]
		if kids == nil {
			kids = []ReplyBrief{}
		}
		pageResult.Items = append(pageResult.Items, ReplyNode{
			Reply:   maskReply(r),
			Replies: kids,
		})
	}

	// 批量填充当前用户的点赞状态，避免逐条查询。
	if viewerID > 0 {
		ids := make([]uint, 0, len(roots)+len(children))
		for _, r := range roots {
			ids = append(ids, r.ID)
		}
		for _, c := range children {
			ids = append(ids, c.ID)
		}
		liked := likedTargets(ctx, viewerID, "reply", ids)
		for i := range pageResult.Items {
			pageResult.Items[i].Reply.Liked = liked[pageResult.Items[i].Reply.ID]
			for j := range pageResult.Items[i].Replies {
				pageResult.Items[i].Replies[j].Liked = liked[pageResult.Items[i].Replies[j].ID]
			}
		}
		_ = childMap
	}
	return pageResult, nil
}

// DeleteReply 删除回复（作者本人或管理员）。
func DeleteReply(ctx context.Context, replyID, userID uint, isAdmin bool) error {
	var reply model.Reply
	if err := repository.DB.First(&reply, replyID).Error; err != nil {
		return ErrNotFound
	}
	if !isAdmin && reply.AuthorID != userID {
		return ErrForbidden
	}
	if err := repository.DB.Delete(&model.Reply{}, replyID).Error; err != nil {
		return err
	}
	repository.DB.Model(&model.Article{}).Where("id = ?", reply.ArticleID).
		UpdateColumn("reply_count", repository.DB.Raw("GREATEST(reply_count - 1, 0)"))
	invalidArticleCache(ctx, reply.ArticleID)
	return nil
}

// maskReply 对不可见的回复做内容屏蔽（保留楼层占位，提示已被处理）。
func maskReply(r model.Reply) model.Reply {
	if !r.Visible {
		r.Content = "该回复因违反社区规范已被隐藏。"
	}
	return r
}

func visibleContent(r model.Reply) string {
	if !r.Visible {
		return "该回复因违反社区规范已被隐藏。"
	}
	return r.Content
}

// ---------------------------------------------------------------------------
// 点赞（帖子 / 回复）
// ---------------------------------------------------------------------------

// ToggleLike 切换点赞状态，返回最新状态与总数。
func ToggleLike(ctx context.Context, userID uint, targetType string, targetID uint) (bool, int64, error) {
	if userID == 0 {
		return false, 0, errors.New("请先登录后再点赞")
	}
	if targetType != "article" && targetType != "reply" {
		return false, 0, errors.New("不支持的点赞目标")
	}

	// 目标存在性校验，避免产生悬空点赞记录。
	switch targetType {
	case "article":
		var article model.Article
		if err := repository.DB.Select("id, like_count").First(&article, targetID).Error; err != nil {
			return false, 0, ErrNotFound
		}
	case "reply":
		var reply model.Reply
		if err := repository.DB.Select("id, like_count").First(&reply, targetID).Error; err != nil {
			return false, 0, ErrNotFound
		}
	}

	var existing model.Reaction
	err := repository.DB.Where("user_id = ? AND target_type = ? AND target_id = ?",
		userID, targetType, targetID).First(&existing).Error

	liked := false
	delta := int64(-1)
	table := "articles"
	if targetType == "reply" {
		table = "replies"
	}

	if err == nil {
		// 已点赞 → 取消
		if delErr := repository.DB.Delete(&model.Reaction{}, existing.ID).Error; delErr != nil {
			return false, 0, delErr
		}
	} else {
		liked = true
		delta = 1
		reaction := &model.Reaction{UserID: userID, TargetType: targetType, TargetID: targetID}
		if createErr := repository.DB.Create(reaction).Error; createErr != nil {
			// 并发重复点赞会命中唯一索引，此时按"已点赞"处理。
			if !strings.Contains(createErr.Error(), "Duplicate") {
				return false, 0, createErr
			}
			liked = true
		}
	}

	if delta < 0 {
		repository.DB.Exec(fmt.Sprintf("UPDATE %s SET like_count = GREATEST(like_count - 1, 0) WHERE id = ?", table), targetID)
	} else {
		repository.DB.Exec(fmt.Sprintf("UPDATE %s SET like_count = like_count + 1 WHERE id = ?", table), targetID)
	}

	var count int64
	repository.DB.Table(table).Select("like_count").Where("id = ?", targetID).Scan(&count)
	if targetType == "article" {
		invalidArticleCache(ctx, targetID)
	}
	return liked, count, nil
}

// likedTargets 批量查询当前用户对一组目标的点赞状态。
func likedTargets(ctx context.Context, userID uint, targetType string, ids []uint) map[uint]bool {
	out := map[uint]bool{}
	if userID == 0 || len(ids) == 0 {
		return out
	}
	var reactions []model.Reaction
	repository.DB.Where("user_id = ? AND target_type = ? AND target_id IN ?", userID, targetType, ids).
		Find(&reactions)
	for _, r := range reactions {
		out[r.TargetID] = true
	}
	return out
}

// MarkArticleLikes 为文章列表批量填充点赞状态。
func MarkArticleLikes(ctx context.Context, userID uint, articles []model.Article) {
	if userID == 0 || len(articles) == 0 {
		return
	}
	ids := make([]uint, 0, len(articles))
	for _, a := range articles {
		ids = append(ids, a.ID)
	}
	liked := likedTargets(ctx, userID, "article", ids)
	for i := range articles {
		articles[i].Liked = liked[articles[i].ID]
	}
}

func truncateForColumn(s string, n int) string {
	return truncate(s, n)
}
