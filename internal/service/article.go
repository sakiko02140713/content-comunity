package service

import (
	"content-community/internal/model"
	"content-community/internal/repository"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"gorm.io/gorm"
)

// 创建文章
func CreateArticle(title, content string, authorID uint) (*model.Article, error) {
	article := &model.Article{
		Title:    title,
		Content:  content,
		AuthorID: authorID,
	}
	if err := repository.DB.Create(article).Error; err != nil {
		return nil, err
	}
	return article, nil
}

// 获取文章详情（带缓存）
func GetArticleByID(id uint) (*model.Article, error) {
	cacheKey := fmt.Sprintf("article:%d", id)

	// 1. 先查 Redis 缓存
	cached, err := repository.Redis.Get(repository.Ctx, cacheKey).Result()
	if err == nil {
		var article model.Article
		if err := json.Unmarshal([]byte(cached), &article); err == nil {
			return &article, nil
		}
	}

	// 2. 缓存未命中，查 MySQL
	var article model.Article
	if err := repository.DB.Preload("Author").First(&article, id).Error; err != nil {
		return nil, err
	}

	// 3. 异步更新阅读量（用于排行榜）
	go incrementViewCount(id)

	// 4. 写入缓存
	data, _ := json.Marshal(article)
	repository.Redis.Set(repository.Ctx, cacheKey, data, time.Hour)

	return &article, nil
}

// 更新阅读量（Redis ZSet 排行榜）
func incrementViewCount(articleID uint) {
	// 更新数据库中的阅读量（用于持久化）
	repository.DB.Model(&model.Article{}).Where("id = ?", articleID).UpdateColumn("view_count", gorm.Expr("view_count + 1"))

	// 更新 Redis ZSet 排行榜
	repository.Redis.ZIncrBy(repository.Ctx, "article:ranking", 1, strconv.FormatUint(uint64(articleID), 10))
}

// 获取热度排行榜 Top N
func GetHotArticles(limit int64) ([]model.Article, error) {
	// 从 Redis ZSet 获取排名前 N 的文章ID
	ids, err := repository.Redis.ZRevRange(repository.Ctx, "article:ranking", 0, limit-1).Result()
	if err != nil {
		return nil, err
	}

	if len(ids) == 0 {
		return []model.Article{}, nil
	}

	// 批量查询 MySQL 获取文章详情
	var articles []model.Article
	repository.DB.Preload("Author").Where("id IN ?", ids).Find(&articles)

	// 按排名顺序排序
	idOrder := make(map[string]int)
	for i, id := range ids {
		idOrder[id] = i
	}
	sort.Slice(articles, func(i, j int) bool {
		return idOrder[strconv.FormatUint(uint64(articles[i].ID), 10)] <
			idOrder[strconv.FormatUint(uint64(articles[j].ID), 10)]
	})

	return articles, nil
}

// 更新文章（同时删除缓存）
func UpdateArticle(id uint, title, content string) (*model.Article, error) {
	var article model.Article
	if err := repository.DB.First(&article, id).Error; err != nil {
		return nil, err
	}

	article.Title = title
	article.Content = content
	if err := repository.DB.Save(&article).Error; err != nil {
		return nil, err
	}

	// 删除缓存，保证一致性
	repository.Redis.Del(repository.Ctx, fmt.Sprintf("article:%d", id))

	return &article, nil
}
