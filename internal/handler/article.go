package handler

import (
	"errors"
	"net/http"

	"content-community/internal/model"
	"content-community/internal/service"

	"github.com/gin-gonic/gin"
)

type articleReq struct {
	Title   string `json:"title" binding:"required"`
	Content string `json:"content" binding:"required"`
	Tags    string `json:"tags"`
	Board   string `json:"board"` // 版块 slug
}

// ListArticles 文章列表（帖子流）：支持版块、关键词、标签、排序与分页。
//
// 可见性规则：默认只返回已发布内容；
// 查询未发布状态（草稿/待复核/已拒绝）时必须登录——
// 普通用户只能看自己的，审核员可以看全部（用于人工复核）。
func ListArticles(c *gin.Context) {
	status := c.DefaultQuery("status", "published")
	uid := userID(c)
	admin := isAdmin(c)

	query := service.ArticleQuery{
		Keyword:  c.Query("keyword"),
		Status:   status,
		Tag:      c.Query("tag"),
		Sort:     c.DefaultQuery("sort", "last"),
		Page:     queryInt(c, "page", 1, 0),
		Size:     queryInt(c, "size", 20, 50),
		BoardID:  paramUint(c, "board_id"),
		ViewerID: uid,
	}
	// 也支持按版块 slug 过滤（前端路由更友好）
	if slug := c.Query("board"); slug != "" && query.BoardID == 0 {
		if board, err := service.BoardBySlug(slug); err == nil {
			query.BoardID = board.ID
		}
	}
	if authorID := uint(queryInt(c, "author_id", 0, 0)); authorID > 0 {
		query.AuthorID = authorID
	}

	if status != "published" && status != "" {
		if uid == 0 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "查看未发布内容需要先登录"})
			return
		}
		if !admin {
			// 强制收敛到"只看自己"，避免越权读取他人草稿。
			query.AuthorID = uid
		}
	}

	page, err := service.ListArticles(query)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询文章列表失败"})
		return
	}
	c.JSON(http.StatusOK, page)
}

// GetArticle 文章详情。草稿/待复核内容仅作者本人与审核员可见。
func GetArticle(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的文章 ID"})
		return
	}
	article, err := service.GetArticleByID(c.Request.Context(), id, userID(c))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "文章不存在"})
		return
	}
	if article.Status != model.StatusPublished {
		if article.AuthorID != userID(c) && !isAdmin(c) {
			c.JSON(http.StatusForbidden, gin.H{"error": "该文章尚未通过审核发布"})
			return
		}
	}
	c.JSON(http.StatusOK, article)
}

// GetHotArticles 热门内容展示。
//
// 使用并行聚合：热榜榜单、状态统计、标签热度三个子任务并发执行后 fan-in，
// 响应中附带并行耗时与加速比，便于前端展示"并行计算"的实际收益。
func GetHotArticles(c *gin.Context) {
	limit := queryInt(c, "limit", 10, 50)
	result, err := service.AggregateHotContent(c.Request.Context(), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "热点聚合失败"})
		return
	}
	c.JSON(http.StatusOK, result)
}

// MyArticles 我的文章管理：按状态过滤自己的全部内容。
func MyArticles(c *gin.Context) {
	page, err := service.ListArticles(service.ArticleQuery{
		AuthorID:    userID(c),
		Status:      c.DefaultQuery("status", "all"),
		Keyword:     c.Query("keyword"),
		Page:        queryInt(c, "page", 1, 0),
		Size:        queryInt(c, "size", 10, 50),
		WithContent: false,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询失败"})
		return
	}
	c.JSON(http.StatusOK, page)
}

// CreateArticle 新建文章草稿（发布需另行调用发布接口触发 AI 审核）。
func CreateArticle(c *gin.Context) {
	var req articleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "标题与正文不能为空"})
		return
	}
	article, err := service.CreateArticleDraft(req.Title, req.Content, req.Tags, req.Board, userID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}
	c.JSON(http.StatusCreated, article)
}

// UpdateArticle 编辑文章，修改后需要重新审核。
func UpdateArticle(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的文章 ID"})
		return
	}
	var req articleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "标题与正文不能为空"})
		return
	}
	article, err := service.UpdateArticle(c.Request.Context(), id, userID(c), req.Title, req.Content, req.Tags, req.Board, isAdmin(c))
	if err != nil {
		writeArticleError(c, err)
		return
	}
	c.JSON(http.StatusOK, article)
}

// DeleteArticle 删除文章。
func DeleteArticle(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的文章 ID"})
		return
	}
	if err := service.DeleteArticle(c.Request.Context(), id, userID(c), isAdmin(c)); err != nil {
		writeArticleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "文章已删除"})
}

// PublishArticle 触发"AI 审核 → 发布"闭环。
func PublishArticle(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的文章 ID"})
		return
	}
	mode := c.DefaultQuery("mode", "semantic")
	if body := struct {
		Mode string `json:"mode"`
	}{}; c.ShouldBindJSON(&body) == nil && body.Mode != "" {
		mode = body.Mode
	}

	article, result, err := service.PublishArticle(c.Request.Context(), id, userID(c), isAdmin(c), mode, nil)
	if err != nil {
		writeArticleError(c, err)
		return
	}

	status := http.StatusOK
	message := "审核通过，文章已发布"
	switch result.Verdict {
	case service.VerdictReview:
		message = "内容疑似违规，已转人工复核，暂不对外发布"
	case service.VerdictReject:
		message = "内容审核未通过，已拒绝发布"
	}
	c.JSON(status, gin.H{
		"message": message,
		"article": article,
		"audit":   result,
	})
}

// ListAuditRecords 审核记录，用于审核结果页展示历史与并行性能指标。
func ListAuditRecords(c *gin.Context) {
	articleID := uint(queryInt(c, "article_id", 0, 0))
	records, err := service.ListAuditRecords(c.Request.Context(), articleID, queryInt(c, "limit", 10, 50))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询审核记录失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": records})
}

// ReviewArticle 人工复核：审核员对待复核文章做出最终裁决。
func ReviewArticle(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的文章 ID"})
		return
	}
	var req struct {
		Decision string `json:"decision" binding:"required"` // approve / reject
		Reason   string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请给出复核结论"})
		return
	}
	article, err := service.ManualReview(c.Request.Context(), id, req.Decision, req.Reason)
	if err != nil {
		writeArticleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "复核完成", "article": article})
}

func writeArticleError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	}
}
