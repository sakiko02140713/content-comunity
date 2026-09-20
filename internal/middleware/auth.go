package middleware

import (
	"net/http"
	"strings"

	"content-community/internal/repository"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// JWTSecret 由 main 在启动时注入，避免在多个包中重复硬编码密钥。
var JWTSecret = []byte("content-community-dev-secret")

// AuthMiddleware 校验请求携带的 JWT，并把用户身份写入上下文。
//
// 采用"JWT 验签 + Redis 白名单"双重校验：
//   - JWT 验签保证 Token 未被篡改（无状态，性能好）；
//   - Redis 白名单保证 Token 可以被主动吊销（注销、封禁立即生效）。
func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "未提供认证信息，请先登录"})
			c.Abort()
			return
		}
		tokenString := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		if tokenString == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "认证信息格式错误"})
			c.Abort()
			return
		}

		if !resolveIdentity(c, tokenString) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "登录状态已失效，请重新登录"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// OptionalAuth 用于"公开但带身份"的接口（如文章详情、文章列表）。
//
// 这类接口未登录也能访问，但若携带了有效 Token，就需要识别出用户身份——
// 否则作者查看自己的草稿/被拒文章时会被当作陌生访客而拒绝。
// Token 无效时静默按匿名处理，不阻断请求。
func OptionalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.Next()
			return
		}
		tokenString := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		if tokenString != "" {
			resolveIdentity(c, tokenString)
		}
		c.Next()
	}
}

// resolveIdentity 校验 Token 并写入 user_id / username / role，成功返回 true。
func resolveIdentity(c *gin.Context, tokenString string) bool {
	// 先查白名单：注销过的 Token 直接拒绝，避免继续解析。
	if _, err := repository.Redis.Get(repository.Ctx, "token:"+tokenString).Result(); err != nil {
		return false
	}

	token, err := jwt.Parse(tokenString, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return JWTSecret, nil
	})
	if err != nil || !token.Valid {
		return false
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return false
	}
	if rawID, ok := claims["user_id"].(float64); ok {
		c.Set("user_id", uint(rawID))
	}
	if name, ok := claims["username"].(string); ok {
		c.Set("username", name)
	}
	role, _ := claims["role"].(string)
	if role == "" {
		role = "user"
	}
	c.Set("role", role)
	return true
}

// AdminOnly 仅允许具备复核权限的用户访问（人工复核后台）。
func AdminOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		role, _ := c.Get("role")
		if r, _ := role.(string); r != "admin" {
			c.JSON(http.StatusForbidden, gin.H{"error": "该操作需要审核员权限"})
			c.Abort()
			return
		}
		c.Next()
	}
}
