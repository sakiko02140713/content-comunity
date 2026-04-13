package handler

import (
    "content-community/internal/service"
    "github.com/gin-gonic/gin"
    "net/http"
)

func Register(c *gin.Context) {
    var req struct {
        Username string `json:"username" binding:"required"`
        Password string `json:"password" binding:"required"`
    }

    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
        return
    }

    user, err := service.Register(req.Username, req.Password)
    if err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
        return
    }

    c.JSON(http.StatusCreated, user)
}

func Login(c *gin.Context) {
    var req struct {
        Username string `json:"username" binding:"required"`
        Password string `json:"password" binding:"required"`
    }

    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
        return
    }

    token, err := service.Login(req.Username, req.Password)
    if err != nil {
        c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
        return
    }

    c.JSON(http.StatusOK, gin.H{"token": token})
}

func Logout(c *gin.Context) {
    // 从请求头获取 Token
    token := c.GetHeader("Authorization")
    if len(token) > 7 && token[:7] == "Bearer " {
        token = token[7:]
    }

    // 调用 service 层注销
    service.Logout(token)
    c.JSON(http.StatusOK, gin.H{"message": "注销成功"})
}