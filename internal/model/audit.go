package model

import "time"

// AuditRecord 记录一次完整的并行审核过程，用于审核追溯与并行性能分析。
//
// 其中 ElapsedMs / SequentialMs / Speedup / MaxConcurrency 由并行引擎实测写入，
// 是"并行计算"效果最直接的证据：串行耗时是各阶段耗时之和，加速比 = 串行耗时 / 实际耗时。
type AuditRecord struct {
	ID        uint   `gorm:"primarykey" json:"id"`
	ArticleID uint   `gorm:"index" json:"article_id"`
	UserID    uint   `gorm:"index" json:"user_id"`
	Title     string `gorm:"size:200" json:"title"`

	Verdict   string `gorm:"size:16;index" json:"verdict"` // pass / review / reject
	RiskScore int    `json:"risk_score"`
	Reason    string `gorm:"size:512" json:"reason"`

	Engine  string `gorm:"size:32" json:"engine"` // parallel-engine / sequential
	Mode    string `gorm:"size:32" json:"mode"`   // local / semantic / chunked
	Workers int    `json:"workers"`               // 并行工作协程数
	Chunks  int    `json:"chunks"`                // 文本分片数

	ElapsedMs      int64   `gorm:"default:0" json:"elapsed_ms"`    // 实际墙钟耗时
	SequentialMs   int64   `gorm:"default:0" json:"sequential_ms"` // 串行等价耗时
	Speedup        float64 `gorm:"default:0" json:"speedup"`       // 加速比
	MaxConcurrency int     `gorm:"default:0" json:"max_concurrency"`

	Stages string `gorm:"type:text" json:"stages"` // 各阶段时间线(JSON)
	Hits   string `gorm:"type:text" json:"hits"`   // 命中的规则/维度(JSON)

	CacheHit  bool      `gorm:"default:false" json:"cache_hit"`
	CreatedAt time.Time `json:"created_at"`
}

func (a *AuditRecord) TableName() string {
	return "audit_records"
}
