package handler

import (
	"net/http"

	"content-community/internal/service"

	"github.com/gin-gonic/gin"
)

type registerReq struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
	Nickname string `json:"nickname"`
}

type loginReq struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// Register 用户注册。
func Register(c *gin.Context) {
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写完整的用户名与密码"})
		return
	}
	user, err := service.Register(req.Username, req.Password, req.Nickname)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"message": "注册成功，请登录",
		"user":    user,
	})
}

// Login 用户登录，签发 JWT。
func Login(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写完整的用户名与密码"})
		return
	}
	token, user, err := service.Login(req.Username, req.Password)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token": token,
		"user":  user,
	})
}

// Logout 注销当前 Token。
func Logout(c *gin.Context) {
	token := c.GetHeader("Authorization")
	if len(token) > 7 && token[:7] == "Bearer " {
		token = token[7:]
	}
	service.Logout(token)
	c.JSON(http.StatusOK, gin.H{"message": "已退出登录"})
}

// UserProfile 用户公开主页（论坛里点击头像进入）。
func UserProfile(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的用户 ID"})
		return
	}
	profile, err := service.BuildUserProfile(id, userID(c))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "用户不存在"})
		return
	}
	c.JSON(http.StatusOK, profile)
}

// Me 返回当前登录用户资料与内容统计，供个人中心展示。
func Me(c *gin.Context) {
	id := userID(c)
	user, err := service.CurrentUser(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "用户不存在"})
		return
	}
	page, err := service.ListArticles(service.ArticleQuery{AuthorID: id, Status: "all", Page: 1, Size: 1})
	total := int64(0)
	if err == nil {
		total = page.Total
	}
	published, _ := service.ListArticles(service.ArticleQuery{AuthorID: id, Status: "published", Page: 1, Size: 1})
	c.JSON(http.StatusOK, gin.H{
		"user": user,
		"stats": gin.H{
			"total":     total,
			"published": published.Total,
		},
	})
}
