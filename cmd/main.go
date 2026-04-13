package main

import (
	"content-community/internal/handler"
	"content-community/internal/middleware"
	"content-community/internal/repository"
	"content-community/pkg/config"
	"log"

	"github.com/gin-gonic/gin"
)

func main() {
	// 加载配置
	cfg := config.Load()

	// 初始化数据库和 Redis
	if err := repository.InitDB(cfg); err != nil {
		log.Fatal("数据库连接失败:", err)
	}
	repository.InitRedis(cfg)

	// 创建 Gin 路由
	r := gin.Default()

	// 公开接口
	r.POST("/api/register", handler.Register)
	r.POST("/api/login", handler.Login)
	r.GET("/api/articles/:id", handler.GetArticle)
	r.GET("/api/articles/hot", handler.GetHotArticles)

	// 需要认证的接口
	auth := r.Group("/api")
	auth.Use(middleware.AuthMiddleware())
	
		auth.POST("/articles", handler.CreateArticle)
		auth.PUT("/articles/:id", handler.UpdateArticle)
		auth.POST("/logout", handler.Logout)
	

	log.Println("服务启动在 :8080")
	r.Run(":8080")
}
