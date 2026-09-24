package main

import (
	"log"
	"os"
	"strings"

	"content-community/internal/handler"
	"content-community/internal/middleware"
	"content-community/internal/repository"
	"content-community/internal/service"
	"content-community/pkg/config"

	"github.com/gin-gonic/gin"
)

func main() {
	cfg := config.Load()

	if err := repository.InitDB(cfg); err != nil {
		log.Fatal("数据库连接失败: ", err)
	}
	if err := repository.InitRedis(cfg); err != nil {
		log.Println("[warn] Redis 连接失败，缓存与热榜将降级为数据库直连: ", err)
	} else {
		log.Println("Redis 连接成功")
	}

	// 签发与验签共用同一密钥。
	middleware.JWTSecret = service.JWTSecret()

	// 后台定时把 Redis 中缓冲的浏览量批量回写数据库（写合并）。
	service.StartViewCountFlusher(repository.Ctx)

	if os.Getenv("SEED_ADMIN") != "false" {
		username, password, created, err := service.SeedAdmin()
		if err != nil {
			log.Println("[warn] 初始化审核员账号失败: ", err)
		} else if created {
			log.Printf("已创建审核员账号：%s / %s（请在生产环境中修改）", username, password)
		}
	}

	// 论坛版块为内置固定分类，启动时按 slug 幂等写入。
	if boards, err := service.SeedBoards(); err != nil {
		log.Println("[warn] 初始化论坛版块失败: ", err)
	} else {
		log.Printf("论坛版块就绪，共 %d 个", len(boards))
	}

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	r.Use(corsMiddleware())

	registerRoutes(r)

	port := cfg.ServerPort
	log.Printf("内容社区服务已启动: http://localhost:%s", port)
	log.Printf("前端资源版本号: %s（前端文件变更后该值会自动变化，浏览器将重新拉取）", handler.AssetVersion())
	if err := r.Run(":" + port); err != nil {
		log.Fatal("服务启动失败: ", err)
	}
}

// corsMiddleware 允许前端页面跨域调试访问 API，并禁止浏览器缓存接口响应。
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Authorization")
		c.Header("Access-Control-Max-Age", "86400")
		// 接口响应不做缓存：点赞数、回复数等会随操作变化，
		// 若被浏览器/代理缓存会出现"点了赞但数字不变"的错觉。
		c.Header("Cache-Control", "no-store")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

func registerRoutes(r *gin.Engine) {
	api := r.Group("/api")

	// ---------------- 公开接口 ----------------
	api.POST("/register", handler.Register)
	api.POST("/login", handler.Login)

	// 文章读取接口对未登录用户开放，但需要识别"若已登录是谁"：
	// 作者要能查看自己的草稿/被拒文章，审核员要能在后台查看待复核内容。
	public := api.Group("")
	public.Use(middleware.OptionalAuth())
	{
		public.GET("/articles", handler.ListArticles)
		public.GET("/articles/hot", handler.GetHotArticles)
		public.GET("/articles/:id", handler.GetArticle)
		public.GET("/articles/:id/replies", handler.ListReplies)
		public.GET("/boards", handler.ListBoards)
		public.GET("/boards/:slug", handler.GetBoard)
		public.GET("/forum/overview", handler.ForumOverview)
		public.GET("/users/:id/profile", handler.UserProfile)
	}

	api.GET("/ai/health", handler.AIHealth)
	api.GET("/ai/audit/info", handler.AIAuditInfo)

	// ---------------- 需登录接口 ----------------
	auth := api.Group("")
	auth.Use(middleware.AuthMiddleware())
	{
		auth.POST("/logout", handler.Logout)
		auth.GET("/me", handler.Me)

		// 文章管理：创建 / 编辑 / 删除 / 我的文章 / 发布
		auth.POST("/articles", handler.CreateArticle)
		auth.PUT("/articles/:id", handler.UpdateArticle)
		auth.DELETE("/articles/:id", handler.DeleteArticle)
		auth.GET("/my/articles", handler.MyArticles)
		auth.POST("/articles/:id/publish", handler.PublishArticle)
		auth.GET("/audit/records", handler.ListAuditRecords)

		// 论坛互动：回复与点赞
		auth.POST("/articles/:id/replies", handler.CreateReply)
		auth.DELETE("/replies/:replyId", handler.DeleteReply)
		auth.POST("/likes/toggle", handler.ToggleLike)

		// AI 辅助功能
		// 只提供"润色"，不提供"凭空生成"：润色强制要求正文非空。
		auth.POST("/ai/polish", handler.AIPolish)
		auth.POST("/ai/summary", handler.AISummary)
		auth.POST("/ai/articles/:id/summary", handler.AISummarizeArticle)

		// 并行内容审核
		auth.POST("/ai/audit", handler.AIAudit)
		auth.POST("/ai/audit/stream", handler.AIAuditStream)
		auth.POST("/ai/audit/batch", handler.AIBatchAudit)
		auth.GET("/ai/stats", handler.AIStats)

		// 人工复核（审核员）
		admin := auth.Group("/admin")
		admin.Use(middleware.AdminOnly())
		admin.POST("/articles/:id/review", handler.ReviewArticle)
		admin.PUT("/articles/:id/flags", handler.UpdateArticleFlags)
	}

	// 静态前端：直接把 frontend 目录挂载到根路径，便于一体化运行。
	// 首页会把资源版本号注入到 CSS/JS 引用上，并声明 no-cache；
	// 带版本号的静态资源则长期强缓存，从而保证"前端一改，刷新即生效"。
	r.GET("/", handler.ServeIndex)
	r.GET("/static/:file", handler.ServeStatic)

	r.NoRoute(func(c *gin.Context) {
		// 前端为 hash 路由，未知路径统一回落到首页，避免直接访问子路径时 404。
		if c.Request.Method == "GET" && !strings.HasPrefix(c.Request.URL.Path, "/api") {
			handler.ServeIndex(c)
			return
		}
		c.JSON(404, gin.H{"error": "接口不存在"})
	})
}
