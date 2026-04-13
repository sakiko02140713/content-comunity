package model

import (
	"time"

	"gorm.io/gorm"
)

type Article struct {
	ID        uint           `gorm:"primarykey" json:"id"`
	Title     string         `gorm:"size:200;not null" json:"title"`
	Content   string         `gorm:"type:text;not null" json:"content"`
	AuthorID  uint           `gorm:"index;not null" json:"author_id"`
	Author    *User          `gorm:"foreignKey:AuthorID" json:"author,omitempty"`
	ViewCount int64          `gorm:"default:0" json:"view_count"` // 阅读量，用于排行榜
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

func (a *Article) TableName() string {
	return "articles"
}

// Hook: 查询文章后自动加载作者信息
func (a *Article) AfterFind(tx *gorm.DB) error {
	if a.AuthorID > 0 && a.Author == nil {
		var author User
		if err := tx.Where("id = ?", a.AuthorID).First(&author).Error; err == nil {
			a.Author = &author
		}
	}
	return nil
}
