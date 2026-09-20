-- =============================================================================
-- 并行计算与AI应用 · 面向UGC社区的智能内容审核与发布系统
-- 数据库初始化脚本（MySQL 8.0）
--
-- 说明：后端启动时 GORM AutoMigrate 也会自动建表，本脚本用于：
--   1) 容器首次启动时预建库表与索引；
--   2) 作为数据库设计的可读文档；
--   3) 便于在无后端环境下查看/调整表结构。
-- =============================================================================

CREATE DATABASE IF NOT EXISTS `community`
    DEFAULT CHARACTER SET utf8mb4
    DEFAULT COLLATE utf8mb4_unicode_ci;

USE `community`;

-- -----------------------------------------------------------------------------
-- 用户表
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `users` (
    `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `username`   VARCHAR(64)  NOT NULL COMMENT '登录名',
    `password`   VARCHAR(128) NOT NULL COMMENT 'bcrypt 哈希后的密码',
    `nickname`   VARCHAR(64)  DEFAULT NULL COMMENT '昵称',
    `role`       VARCHAR(16)  NOT NULL DEFAULT 'user' COMMENT 'user=普通用户, admin=审核员',
    `created_at` DATETIME(3)  DEFAULT NULL,
    `updated_at` DATETIME(3)  DEFAULT NULL,
    `deleted_at` DATETIME(3)  DEFAULT NULL COMMENT '软删除标记',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_users_username` (`username`),
    KEY `idx_users_deleted_at` (`deleted_at`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COMMENT = '社区用户';

-- -----------------------------------------------------------------------------
-- 文章表
--
-- status 描述"创作 → AI审核 → 发布"链路中的位置：
--   draft     草稿，可自由编辑
--   review    AI 判定疑似违规，等待人工复核（对外不可见）
--   rejected  AI 判定违规，拒绝发布
--   published 审核通过并发布，进入社区热榜
--
-- audit_* 字段缓存最近一次审核结论，列表页无需回查 audit_records 即可展示。
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `articles` (
    `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `title`            VARCHAR(200) NOT NULL,
    `content`          LONGTEXT     NOT NULL,
    `summary`          TEXT         DEFAULT NULL COMMENT 'AI 生成的摘要',
    `tags`             VARCHAR(255) DEFAULT NULL COMMENT '逗号分隔标签',
    `author_id`        BIGINT UNSIGNED NOT NULL,
    `status`           VARCHAR(16)  NOT NULL DEFAULT 'draft',
    `view_count`       BIGINT       NOT NULL DEFAULT 0 COMMENT '浏览量（由 Redis 缓冲批量回写）',
    `like_count`       BIGINT       NOT NULL DEFAULT 0,
    `audit_status`     VARCHAR(16)  DEFAULT NULL COMMENT '最近一次审核结论 pass/review/reject',
    `audit_score`      INT          NOT NULL DEFAULT 0 COMMENT '风险分 0-100',
    `audit_reason`     VARCHAR(512) DEFAULT NULL COMMENT '审核依据（可解释性）',
    `audit_latency_ms` BIGINT       NOT NULL DEFAULT 0 COMMENT '审核实际耗时',
    `audited_at`       DATETIME(3)  DEFAULT NULL,
    `published_at`     DATETIME(3)  DEFAULT NULL,
    `created_at`       DATETIME(3)  DEFAULT NULL,
    `updated_at`       DATETIME(3)  DEFAULT NULL,
    `deleted_at`       DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_articles_author` (`author_id`),
    KEY `idx_articles_status` (`status`),
    KEY `idx_articles_audit_status` (`audit_status`),
    KEY `idx_articles_title` (`title`),
    KEY `idx_articles_deleted_at` (`deleted_at`),
    KEY `idx_articles_hot` (`status`, `view_count` DESC) COMMENT '热榜兜底排序'
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COMMENT = '社区文章';

-- -----------------------------------------------------------------------------
-- 审核记录表：保存每次并行审核的结论与性能指标
--
-- 性能字段是"并行计算"的直接证据：
--   elapsed_ms    实际墙钟耗时（并行）
--   sequential_ms 串行等价耗时（各并行任务耗时之和）
--   speedup       加速比 = sequential_ms / elapsed_ms
--   max_concurrency 实测峰值并行度
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS `audit_records` (
    `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `article_id`      BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `user_id`         BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `title`           VARCHAR(200) DEFAULT NULL,
    `verdict`         VARCHAR(16)  NOT NULL DEFAULT 'pass' COMMENT 'pass/review/reject',
    `risk_score`      INT          NOT NULL DEFAULT 0,
    `reason`          VARCHAR(512) DEFAULT NULL,
    `engine`          VARCHAR(32)  DEFAULT NULL COMMENT '审核引擎标识',
    `mode`            VARCHAR(32)  DEFAULT NULL COMMENT 'local/semantic/chunked',
    `workers`         INT          NOT NULL DEFAULT 0 COMMENT '并行工作协程数',
    `chunks`          INT          NOT NULL DEFAULT 0 COMMENT '文本分片数',
    `elapsed_ms`      BIGINT       NOT NULL DEFAULT 0,
    `sequential_ms`   BIGINT       NOT NULL DEFAULT 0,
    `speedup`         DOUBLE       NOT NULL DEFAULT 0,
    `max_concurrency` INT          NOT NULL DEFAULT 0,
    `stages`          TEXT         DEFAULT NULL COMMENT '各阶段时间线(JSON)',
    `hits`            TEXT         DEFAULT NULL COMMENT '规则命中明细(JSON)',
    `cache_hit`       TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`      DATETIME(3)  DEFAULT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_audit_article` (`article_id`),
    KEY `idx_audit_user` (`user_id`),
    KEY `idx_audit_verdict` (`verdict`),
    KEY `idx_audit_created` (`created_at`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COMMENT = '并行审核记录';

-- -----------------------------------------------------------------------------
-- 可选：演示数据（需要先有 id=1 的用户，因此默认不执行）
-- 直接通过前端注册账号并发布文章即可产生真实数据。
-- -----------------------------------------------------------------------------
