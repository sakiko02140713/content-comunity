package handler

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// frontendDir 前端静态资源目录（容器内与本地运行都位于项目根的 frontend 下）。
const frontendDir = "./frontend"

var (
	assetVersionOnce sync.Once
	assetVersion     string
)

// AssetVersion 返回前端资源版本号。
//
// 取 index.html / app.js / style.css 的"大小 + 修改时间"做哈希：
// 前端只要有任何改动，版本号就会变化，页面里引用的资源 URL 随之改变，
// 浏览器必然重新拉取——避免出现"改了样式但用户看到的还是旧版"的情况。
func AssetVersion() string {
	assetVersionOnce.Do(func() {
		h := sha1.New()
		for _, name := range []string{"index.html", "app.js", "style.css"} {
			info, err := os.Stat(filepath.Join(frontendDir, name))
			if err != nil {
				continue
			}
			fmt.Fprintf(h, "%s|%d|%d;", name, info.Size(), info.ModTime().UnixNano())
		}
		assetVersion = hex.EncodeToString(h.Sum(nil))[:10]
	})
	return assetVersion
}

// ServeIndex 返回前端首页，并把资源版本号注入到 CSS/JS 引用上。
//
// 同时声明 no-cache：HTML 必须每次协商，否则浏览器会一直沿用旧的资源引用，
// 即使静态资源已更新也拉不到新版。
func ServeIndex(c *gin.Context) {
	raw, err := os.ReadFile(filepath.Join(frontendDir, "index.html"))
	if err != nil {
		c.String(500, "前端页面缺失，请确认 frontend/index.html 存在")
		return
	}

	version := AssetVersion()
	html := string(raw)
	// 给静态资源引用追加版本号，配合下面的 immutable 缓存策略实现"改动即生效"。
	html = strings.ReplaceAll(html, `href="/static/style.css"`, `href="/static/style.css?v=`+version+`"`)
	html = strings.ReplaceAll(html, `src="/static/app.js"`, `src="/static/app.js?v=`+version+`"`)

	c.Header("Cache-Control", "no-cache, must-revalidate")
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(200, "%s", html)
}

// ServeStatic 提供前端静态资源，并根据是否携带版本号选择缓存策略。
//
//	带版本号 → 长期强缓存（URL 变化自然会绕过缓存）
//	不带版本号 → 仅协商缓存，保证内容更新后立刻生效
func ServeStatic(c *gin.Context) {
	if c.Query("v") != "" {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		c.Header("Cache-Control", "no-cache")
	}
	c.File(filepath.Join(frontendDir, c.Param("file")))
}
