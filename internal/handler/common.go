package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// userID 从上下文取出当前登录用户 ID。
func userID(c *gin.Context) uint {
	v, ok := c.Get("user_id")
	if !ok {
		return 0
	}
	id, _ := v.(uint)
	return id
}

// username 从上下文取出当前登录用户名。
func username(c *gin.Context) string {
	v, ok := c.Get("username")
	if !ok {
		return ""
	}
	name, _ := v.(string)
	return name
}

// isAdmin 从上下文取出当前登录用户是否具备复核权限。
func isAdmin(c *gin.Context) bool {
	v, ok := c.Get("role")
	if !ok {
		return false
	}
	role, _ := v.(string)
	return role == "admin"
}

// paramID 解析路径参数中的无符号整数。
func paramID(c *gin.Context, name string) (uint, bool) {
	id, err := strconv.ParseUint(c.Param(name), 10, 32)
	if err != nil || id == 0 {
		return 0, false
	}
	return uint(id), true
}

// queryInt 读取整型查询参数并应用默认值与上限。
func queryInt(c *gin.Context, name string, def, max int) int {
	raw := c.DefaultQuery(name, strconv.Itoa(def))
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if max > 0 && v > max {
		return max
	}
	if v < 0 {
		return def
	}
	return v
}
