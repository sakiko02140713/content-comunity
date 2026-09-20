package service

import (
	"errors"
	"os"
	"strings"
	"time"

	"content-community/internal/model"
	"content-community/internal/repository"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// jwtSecret 优先读取环境变量：生产环境务必通过 JWT_SECRET 覆盖。
var jwtSecret = func() []byte {
	if v := os.Getenv("JWT_SECRET"); v != "" {
		return []byte(v)
	}
	return []byte("content-community-dev-secret")
}()

const tokenTTL = 7 * 24 * time.Hour

// Register 注册社区用户。
//
// 用户名/密码先做长度校验再落库，避免弱口令与超长字段；密码用 bcrypt 加盐哈希存储。
func Register(username, password, nickname string) (*model.User, error) {
	username = strings.TrimSpace(username)
	if n := len([]rune(username)); n < 3 || n > 20 {
		return nil, errors.New("用户名长度需在 3-20 个字符之间")
	}
	if len(password) < 6 {
		return nil, errors.New("密码长度不能少于 6 位")
	}

	var existUser model.User
	if err := repository.DB.Where("username = ?", username).First(&existUser).Error; err == nil {
		return nil, errors.New("用户名已存在")
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(nickname) == "" {
		nickname = username
	}
	user := &model.User{
		Username: username,
		Password: string(hashedPassword),
		Nickname: nickname,
		Role:     model.RoleUser,
	}
	if err := repository.DB.Create(user).Error; err != nil {
		return nil, err
	}
	return user, nil
}

// Login 校验凭据并签发 JWT，同时在 Redis 中登记该 Token 以支持主动注销。
//
// Token 采用"JWT 自包含 + Redis 白名单"的双重校验：
// JWT 负责无状态验签，Redis 负责吊销，兼顾性能与可控性。
func Login(username, password string) (string, *model.User, error) {
	var user model.User
	if err := repository.DB.Where("username = ?", username).First(&user).Error; err != nil {
		return "", nil, errors.New("用户名或密码错误")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)); err != nil {
		return "", nil, errors.New("用户名或密码错误")
	}
	if user.Role == "" {
		user.Role = model.RoleUser
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":  user.ID,
		"username": user.Username,
		"role":     string(user.Role),
		"exp":      time.Now().Add(tokenTTL).Unix(),
	})
	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		return "", nil, err
	}
	repository.Redis.Set(repository.Ctx, "token:"+tokenString, user.ID, tokenTTL)
	return tokenString, &user, nil
}

// Logout 主动注销 Token。
func Logout(token string) {
	repository.Redis.Del(repository.Ctx, "token:"+token)
}

// CurrentUser 读取用户资料。
func CurrentUser(id uint) (*model.User, error) {
	var user model.User
	if err := repository.DB.First(&user, id).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

// IsAdmin 判断用户是否具备人工复核权限。
func IsAdmin(userID uint) bool {
	user, err := CurrentUser(userID)
	if err != nil {
		return false
	}
	return user.Role == model.RoleAdmin
}
