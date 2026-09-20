package repository

import (
	"context"
	"fmt"
	"time"

	"content-community/internal/model"
	"content-community/pkg/config"

	"github.com/go-redis/redis/v8"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	DB    *gorm.DB
	Redis *redis.Client
	Ctx   = context.Background()
)

// InitDB 建立数据库连接并自动迁移表结构。
func InitDB(cfg *config.Config) error {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		cfg.DBUser, cfg.DBPassword, cfg.DBHost, cfg.DBPort, cfg.DBName)
	var err error
	DB, err = gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return err
	}

	sqlDB, err := DB.DB()
	if err != nil {
		return err
	}
	// 连接池配置：并行审核会在短时间内并发访问数据库，需要足够的连接数与复用率。
	sqlDB.SetMaxOpenConns(64)
	sqlDB.SetMaxIdleConns(16)
	sqlDB.SetConnMaxLifetime(time.Hour)

	return DB.AutoMigrate(&model.User{}, &model.Article{}, &model.AuditRecord{},
		&model.Board{}, &model.Reply{}, &model.Reaction{})
}

// InitRedis 建立 Redis 连接。连接失败不阻断启动（缓存降级为直连数据库），
// 但会返回错误供调用方打印告警。
func InitRedis(cfg *config.Config) error {
	Redis = redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%s", cfg.RedisHost, cfg.RedisPort),
		Password: cfg.RedisPass,
		DB:       0,
		PoolSize: 64,
	})
	ctx, cancel := context.WithTimeout(Ctx, 3*time.Second)
	defer cancel()
	return Redis.Ping(ctx).Err()
}
