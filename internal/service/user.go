package service

import (
	"content-community/internal/model"
	"content-community/internal/repository"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

var jwtSecret = []byte("your-secret-key-change-in-production")

// 注册
func Register(username, password string) (*model.User, error) {
	// 检查用户是否存在
	var existUser model.User
	if err := repository.DB.Where("username = ?", username).First(&existUser).Error; err == nil {
		return nil, errors.New("用户名已存在")
	}

	// 加密密码
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	user := &model.User{
		Username: username,
		Password: string(hashedPassword),
	}

	if err := repository.DB.Create(user).Error; err != nil {
		return nil, err
	}
	return user, nil
}

// 登录
func Login(username, password string) (string, error) {
	var user model.User
	if err := repository.DB.Where("username = ?", username).First(&user).Error; err != nil {
		return "", errors.New("用户名或密码错误")
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)); err != nil {
		return "", errors.New("用户名或密码错误")
	}

	// 生成 JWT
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":  user.ID,
		"username": user.Username,
		"exp":      time.Now().Add(time.Hour * 24 * 7).Unix(),
	})

	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		return "", err
	}

	// 将 Token 存入 Redis（用于强制注销）
	repository.Redis.Set(repository.Ctx, "token:"+tokenString, user.ID, time.Hour*24*7)

	return tokenString, nil
}
func Logout(token string) {
    // 从 Redis 中删除 Token，使其失效
    repository.Redis.Del(repository.Ctx, "token:"+token)
}
