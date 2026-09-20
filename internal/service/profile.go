package service

import (
	"content-community/internal/model"
	"content-community/internal/repository"
)

// UserProfile 用户公开资料（论坛个人主页展示）。
type UserProfile struct {
	User          *model.User     `json:"user"`
	ThreadCount   int64           `json:"thread_count"`
	ReplyCount    int64           `json:"reply_count"`
	LikeReceived  int64           `json:"like_received"`
	RecentPosts   []model.Article `json:"recent_posts"`
	RecentReplies []ReplyBrief    `json:"recent_replies"`
}

// BuildUserProfile 汇总某个用户的论坛资料。
func BuildUserProfile(userID uint, viewerID uint) (*UserProfile, error) {
	var user model.User
	if err := repository.DB.First(&user, userID).Error; err != nil {
		return nil, ErrNotFound
	}

	profile := &UserProfile{User: &user}

	repository.DB.Model(&model.Article{}).
		Where("author_id = ? AND status = ?", userID, model.StatusPublished).
		Count(&profile.ThreadCount)
	repository.DB.Model(&model.Reply{}).Where("author_id = ?", userID).Count(&profile.ReplyCount)

	// 收到多少赞：帖子与回复的点赞数之和。
	repository.DB.Model(&model.Article{}).Where("author_id = ?", userID).
		Select("COALESCE(SUM(like_count), 0)").Scan(&profile.LikeReceived)

	page, err := ListArticles(ArticleQuery{
		AuthorID: userID, Status: "published", Sort: "new", Page: 1, Size: 10, ViewerID: viewerID,
	})
	if err == nil && page != nil {
		profile.RecentPosts = page.Items
	}
	if profile.RecentPosts == nil {
		profile.RecentPosts = []model.Article{}
	}

	var replies []model.Reply
	repository.DB.Preload("Author").
		Where("author_id = ?", userID).Order("id DESC").Limit(10).Find(&replies)
	profile.RecentReplies = []ReplyBrief{}
	for _, r := range replies {
		profile.RecentReplies = append(profile.RecentReplies, ReplyBrief{
			ID: r.ID, ParentID: r.ParentID, Content: visibleContent(r),
			Floor: r.Floor, LikeCount: r.LikeCount, Author: r.Author, CreatedAt: r.CreatedAt,
		})
	}
	return profile, nil
}
