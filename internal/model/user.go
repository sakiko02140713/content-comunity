package model

import (
	"time"

	"gorm.io/gorm"
)

// Role 社区用户角色。普通用户负责创作，审核员负责人工复核 AI 审核结果。
type Role string

const (
	RoleUser  Role = "user"
	RoleAdmin Role = "admin"
)

type User struct {
	ID        uint           `gorm:"primarykey" json:"id"`
	Username  string         `gorm:"size:64;uniqueIndex;not null" json:"username"`
	Password  string         `gorm:"size:128;not null" json:"-"`
	Nickname  string         `gorm:"size:64" json:"nickname"`
	Role      Role           `gorm:"size:16;default:user" json:"role"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

func (u *User) TableName() string {
	return "users"
}
