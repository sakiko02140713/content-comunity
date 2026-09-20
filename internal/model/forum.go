package model

import (
	"time"

	"gorm.io/gorm"
)

// Board 论坛版块（分类）。
//
// 按当前需求采用"固定内置版块"：版块由代码定义并在启动时写入数据库，
// 文章通过 BoardID 归属版块，列表页据此分组展示。
type Board struct {
	ID          uint      `gorm:"primarykey" json:"id"`
	Slug        string    `gorm:"size:32;uniqueIndex;not null" json:"slug"`
	Name        string    `gorm:"size:32;not null" json:"name"`
	Description string    `gorm:"size:160" json:"description"`
	Icon        string    `gorm:"size:8" json:"icon"`
	SortOrder   int       `gorm:"default:0" json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
}

func (b *Board) TableName() string {
	return "boards"
}

// BoardStat 版块统计（帖子数 / 回复数 / 今日新帖），用于版块列表右侧的数字展示。
type BoardStat struct {
	Board
	ThreadCount int64 `json:"thread_count"`
	ReplyCount  int64 `json:"reply_count"`
	TodayCount  int64 `json:"today_count"`
}

// Reply 帖子回复（楼层）。
//
// 采用两层结构：ParentID 为 0 表示直接回复主题（主楼），
// 非 0 表示引用某条回复（楼中楼）。这样既保留对话上下文，
// 又避免无限层级带来的展示与查询复杂度。
type Reply struct {
	ID        uint   `gorm:"primarykey" json:"id"`
	ArticleID uint   `gorm:"index;not null" json:"article_id"`
	ParentID  uint   `gorm:"index;default:0" json:"parent_id"`
	AuthorID  uint   `gorm:"index;not null" json:"author_id"`
	Author    *User  `gorm:"foreignKey:AuthorID" json:"author,omitempty"`
	Content   string `gorm:"type:text;not null" json:"content"`

	Floor     int   `gorm:"default:0" json:"floor"` // 楼层号，仅主楼编号
	LikeCount int64 `gorm:"default:0" json:"like_count"`

	// 当前登录用户是否已点赞（查询时填充，不落库）
	Liked bool `gorm:"-" json:"liked"`

	// 回复同样经过 AI 审核；判定违规的回复不展示（内容置空并给出提示）。
	AuditStatus string `gorm:"size:16;index" json:"audit_status"`
	AuditReason string `gorm:"size:512" json:"audit_reason"`
	Visible     bool   `gorm:"default:true;index" json:"visible"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

func (r *Reply) TableName() string {
	return "replies"
}

// Reaction 点赞/表态记录。
//
// 用 (user_id, target_type, target_id) 唯一索引保证同一个人对同一目标只能点一次赞，
// 计数同时冗余在 articles.like_count / replies.like_count 上，避免列表页做聚合查询。
type Reaction struct {
	ID         uint      `gorm:"primarykey" json:"id"`
	UserID     uint      `gorm:"uniqueIndex:uk_reaction,priority:1;not null" json:"user_id"`
	TargetType string    `gorm:"size:16;uniqueIndex:uk_reaction,priority:2;not null" json:"target_type"` // article / reply
	TargetID   uint      `gorm:"uniqueIndex:uk_reaction,priority:3;not null" json:"target_id"`
	CreatedAt  time.Time `json:"created_at"`
}

func (r *Reaction) TableName() string {
	return "reactions"
}
