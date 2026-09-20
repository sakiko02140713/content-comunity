package service

import (
	"os"

	"content-community/internal/model"
	"content-community/internal/repository"

	"golang.org/x/crypto/bcrypt"
)

// JWTSecret 暴露当前使用的签名密钥，供 middleware 初始化时注入，保证签发与验签使用同一密钥。
func JWTSecret() []byte { return jwtSecret }

// SeedAdmin 在首次启动时保障存在一个审核员账号，便于演示人工复核闭环。
// 密码取自环境变量 ADMIN_PASSWORD，未配置时使用默认值并在启动日志中提示。
func SeedAdmin() (username, password string, created bool, err error) {
	username = os.Getenv("ADMIN_USERNAME")
	if username == "" {
		username = "admin"
	}
	password = os.Getenv("ADMIN_PASSWORD")
	if password == "" {
		password = "admin123456"
	}

	var existing model.User
	if e := repository.DB.Where("username = ?", username).First(&existing).Error; e == nil {
		if existing.Role != model.RoleAdmin {
			repository.DB.Model(&model.User{}).Where("id = ?", existing.ID).Update("role", model.RoleAdmin)
		}
		return username, password, false, nil
	}

	hashed, e := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if e != nil {
		return "", "", false, e
	}
	admin := &model.User{
		Username: username,
		Password: string(hashed),
		Nickname: "社区审核员",
		Role:     model.RoleAdmin,
	}
	if e := repository.DB.Create(admin).Error; e != nil {
		return "", "", false, e
	}
	return username, password, true, nil
}
