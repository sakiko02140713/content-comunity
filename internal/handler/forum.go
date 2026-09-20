package handler

import (
	"net/http"
	"strconv"

	"content-community/internal/service"

	"github.com/gin-gonic/gin"
)

// ListBoards 版块列表（含帖子数/回复数/今日新帖统计）。
func ListBoards(c *gin.Context) {
	stats, err := service.ListBoardStats()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取版块失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": stats})
}

// GetBoard 单个版块信息。
func GetBoard(c *gin.Context) {
	board, err := service.BoardBySlug(c.Param("slug"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "版块不存在"})
		return
	}
	c.JSON(http.StatusOK, board)
}

// ListReplies 帖子回复列表（主楼 + 楼中楼）。
func ListReplies(c *gin.Context) {
	articleID, ok := paramID(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的帖子 ID"})
		return
	}
	page, err := service.ListReplies(c.Request.Context(), articleID,
		queryInt(c, "page", 1, 0), queryInt(c, "size", 20, 50), userID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取回复失败"})
		return
	}
	c.JSON(http.StatusOK, page)
}

// CreateReply 发表回复（回复同样经过 AI 并行审核）。
func CreateReply(c *gin.Context) {
	articleID, ok := paramID(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的帖子 ID"})
		return
	}
	var req struct {
		Content  string `json:"content" binding:"required"`
		ParentID uint   `json:"parent_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "回复内容不能为空"})
		return
	}

	result, err := service.CreateReply(c.Request.Context(), articleID, req.ParentID, userID(c), req.Content)
	if err != nil {
		writeArticleError(c, err)
		return
	}

	status := http.StatusCreated
	if result.Blocked {
		// 被 AI 拦截的回复不入库，用 200 + blocked 标记返回，前端据此提示用户。
		status = http.StatusOK
	}
	c.JSON(status, gin.H{
		"message": result.Message,
		"blocked": result.Blocked,
		"reply":   result.Reply,
		"audit": gin.H{
			"verdict":          result.Audit.Verdict,
			"risk_score":       result.Audit.RiskScore,
			"reason":           result.Audit.Reason,
			"elapsed_ms":       result.Audit.ElapsedMs,
			"sequential_ms":    result.Audit.SequentialMs,
			"speedup":          result.Audit.Speedup,
			"peak_concurrency": result.Audit.PeakConcurrency,
			"dimensions":       result.Audit.Dimensions,
		},
	})
}

// DeleteReply 删除回复。
func DeleteReply(c *gin.Context) {
	id, ok := paramID(c, "replyId")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的回复 ID"})
		return
	}
	if err := service.DeleteReply(c.Request.Context(), id, userID(c), isAdmin(c)); err != nil {
		writeArticleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "回复已删除"})
}

// ToggleLike 点赞/取消点赞（帖子或回复）。
func ToggleLike(c *gin.Context) {
	var req struct {
		TargetType string `json:"target_type" binding:"required"`
		TargetID   uint   `json:"target_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数不完整"})
		return
	}
	liked, count, err := service.ToggleLike(c.Request.Context(), userID(c), req.TargetType, req.TargetID)
	if err != nil {
		writeArticleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"liked": liked, "like_count": count})
}

// UpdateArticleFlags 版块管理操作：置顶 / 加精（审核员）。
func UpdateArticleFlags(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的帖子 ID"})
		return
	}
	var req struct {
		Pinned   *bool `json:"pinned"`
		Featured *bool `json:"featured"`
		BoardID  *uint `json:"board_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数格式错误"})
		return
	}

	updates := map[string]any{}
	if req.Pinned != nil {
		updates["pinned"] = *req.Pinned
	}
	if req.Featured != nil {
		updates["featured"] = *req.Featured
	}
	if req.BoardID != nil {
		updates["board_id"] = *req.BoardID
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有需要更新的字段"})
		return
	}
	article, err := service.UpdateArticleFlags(c.Request.Context(), id, updates)
	if err != nil {
		writeArticleError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "已更新", "article": article})
}

// ForumOverview 论坛首页概览：版块统计 + 最新帖子 + 热帖 + 站点数字。
func ForumOverview(c *gin.Context) {
	overview, err := service.BuildForumOverview(c.Request.Context(), userID(c), queryInt(c, "board_size", 5, 20))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取论坛概览失败"})
		return
	}
	c.JSON(http.StatusOK, overview)
}

// paramUint 解析查询参数中的无符号整数。
func paramUint(c *gin.Context, name string) uint {
	v, _ := strconv.ParseUint(c.Query(name), 10, 32)
	return uint(v)
}
