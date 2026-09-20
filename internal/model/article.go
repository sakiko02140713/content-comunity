package model

import (
	"time"

	"gorm.io/gorm"
)

// ArticleStatus 描述文章在"创作 → AI 审核 → 发布"链路中的状态。
//
//	draft    草稿，可自由编辑
//	review   AI 判定为疑似违规，等待人工复核
//	rejected AI 判定违规，拒绝发布
//	published 审核通过并发布，进入社区热榜
type ArticleStatus string

const (
	StatusDraft     ArticleStatus = "draft"
	StatusReview    ArticleStatus = "review"
	StatusRejected  ArticleStatus = "rejected"
	StatusPublished ArticleStatus = "published"
)

type Article struct {
	ID       uint   `gorm:"primarykey" json:"id"`
	Title    string `gorm:"size:200;not null;index" json:"title"`
	Content  string `gorm:"type:longtext;not null" json:"content"`
	Summary  string `gorm:"type:text" json:"summary"` // AI 摘要
	Tags     string `gorm:"size:255" json:"tags"`     // 逗号分隔标签
	AuthorID uint   `gorm:"index;not null" json:"author_id"`
	Author   *User  `gorm:"foreignKey:AuthorID" json:"author,omitempty"`

	// 论坛属性：版块归属、置顶、加精、回复数
	BoardID    uint   `gorm:"index;default:0" json:"board_id"`
	Board      *Board `gorm:"foreignKey:BoardID" json:"board,omitempty"`
	Pinned     bool   `gorm:"default:false;index" json:"pinned"`
	Featured   bool   `gorm:"default:false" json:"featured"`
	ReplyCount int64  `gorm:"default:0" json:"reply_count"`

	Status ArticleStatus `gorm:"size:16;default:draft;index" json:"status"`

	ViewCount int64 `gorm:"default:0" json:"view_count"`
	LikeCount int64 `gorm:"default:0" json:"like_count"`

	// 审核相关字段：保存最近一次审核结论，便于列表直接展示与人工复核。
	AuditStatus    string     `gorm:"size:16;index" json:"audit_status"`
	AuditScore     int        `gorm:"default:0" json:"audit_score"`
	AuditReason    string     `gorm:"size:512" json:"audit_reason"`
	AuditLatencyMs int64      `gorm:"default:0" json:"audit_latency_ms"`
	AuditedAt      *time.Time `json:"audited_at"`

	// 当前登录用户是否已点赞 / 收藏（查询时填充，不落库）
	Liked       bool       `gorm:"-" json:"liked"`
	LastReplyAt *time.Time `json:"last_reply_at"`

	PublishedAt *time.Time     `json:"published_at"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
}

func (a *Article) TableName() string {
	return "articles"
}

// AfterFind 兜底填充作者与版块信息，避免缓存或裸查询返回的文章缺少这些字段。
func (a *Article) AfterFind(tx *gorm.DB) error {
	if a.AuthorID > 0 && a.Author == nil {
		var author User
		if err := tx.Where("id = ?", a.AuthorID).First(&author).Error; err == nil {
			a.Author = &author
		}
	}
	if a.BoardID > 0 && a.Board == nil {
		var board Board
		if err := tx.Where("id = ?", a.BoardID).First(&board).Error; err == nil {
			a.Board = &board
		}
	}
	return nil
}
