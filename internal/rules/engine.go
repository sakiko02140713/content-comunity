// Package rules 实现内容审核的本地规则引擎。
//
// 设计动机：大模型审核准确但耗时（数百毫秒到数秒），而敏感词/正则规则检测是纯 CPU 计算，
// 单次耗时通常在毫秒级，且完全不依赖外部服务。在并行审核流水线中，
// 规则引擎与大模型语义审核同时启动，"快速通道"可以先行给出风险信号，
// 既提升整体吞吐，也在 AI 服务不可用时提供兜底审核能力。
package rules

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Category 违规类别。
type Category string

const (
	CategoryAd       Category = "ad"       // 广告导流
	CategoryAbuse    Category = "abuse"    // 辱骂攻击
	CategoryPorn     Category = "porn"     // 色情低俗
	CategoryPolitics Category = "politics" // 政治敏感
	CategoryIllegal  Category = "illegal"  // 违法违禁
)

// Rule 单条审核规则。
type Rule struct {
	ID       string
	Name     string
	Category Category
	// Weight 命中该规则的权重分值，累加进风险分。
	Weight int
	// Severity critical 表示命中即直接拒绝。
	Severity   string
	Keywords   []string
	Patterns   []string
	patterns   []*regexp.Regexp
	Descriptor string
}

// Hit 一条命中记录。
type Hit struct {
	RuleID    string   `json:"rule_id"`
	RuleName  string   `json:"rule_name"`
	Category  Category `json:"category"`
	Severity  string   `json:"severity"`
	Weight    int      `json:"weight"`
	Keywords  []string `json:"keywords"`
	Evidences []string `json:"evidences"`
}

// Result 规则引擎的审核结果。
type Result struct {
	Score      int   `json:"score"`
	Hits       []Hit `json:"hits"`
	Critical   bool  `json:"critical"`
	ScannedLen int   `json:"scanned_len"`
	RuleCount  int   `json:"rule_count"`
}

// ruleTable 规则表。生产环境可替换为数据库/配置中心下发，这里以内置词库保证可复现。
var ruleTable = []Rule{
	{
		ID: "AD-001", Name: "违规广告导流", Category: CategoryAd, Weight: 35, Severity: "high",
		Keywords: []string{"加微信", "加vx", "加V信", "私聊我购买", "代购", "刷单", "兼职日结", "加qq", "扫码进群", "免费领取", "点击链接购买", "微商货源"},
		Patterns: []string{`(?i)(wx|vx|qq)\s*[:：]?\s*[0-9a-z]{5,}`, `(?i)https?://[^\s]{0,40}(buy|shop|promo|discount)`},
	},
	{
		ID: "ABU-001", Name: "辱骂与人身攻击", Category: CategoryAbuse, Weight: 40, Severity: "high",
		Keywords: []string{"傻逼", "傻B", "脑残", "废物", "去死", "滚出去", "垃圾东西", "狗东西", "you are stupid", "idiot"},
	},
	{
		ID: "POR-001", Name: "色情低俗内容", Category: CategoryPorn, Weight: 60, Severity: "critical",
		Keywords: []string{"约炮", "一夜情", "裸聊", "成人影片", "色情网站", "激情视频"},
	},
	{
		ID: "POL-001", Name: "政治敏感内容", Category: CategoryPolitics, Weight: 60, Severity: "critical",
		Keywords: []string{"颠覆国家", "分裂国家", "煽动颠覆", "反党反政府"},
	},
	{
		ID: "ILL-001", Name: "违法违禁信息", Category: CategoryIllegal, Weight: 80, Severity: "critical",
		Keywords: []string{"枪支弹药", "冰毒", "出售毒品", "办假证", "代开发票", "银行卡四件套", "赌博网站", "博彩平台"},
		Patterns: []string{`[0-9]{2,4}\s*元\s*(包夜|上门服务)`},
	},
	{
		ID: "AD-002", Name: "诱导分享与垃圾信息", Category: CategoryAd, Weight: 20, Severity: "medium",
		Keywords: []string{"转发三个群", "不转不是中国人", "点击领取红包", "限时秒杀链接", "免费送手机"},
		Patterns: []string{`1[3-9]\d{9}`}, // 手机号裸奔发布，常见于广告
	},
}

var (
	compiledOnce bool
	allPatterns  []*regexp.Regexp
)

// prepare 预编译正则表达式，避免每次审核重复编译。
// 由 init 触发，编译失败的正则被忽略（规则仍然以关键词方式生效）。
func prepare() {
	if compiledOnce {
		return
	}
	compiledOnce = true
	for i := range ruleTable {
		for _, p := range ruleTable[i].Patterns {
			re, err := regexp.Compile(p)
			if err != nil {
				continue
			}
			ruleTable[i].patterns = append(ruleTable[i].patterns, re)
			allPatterns = append(allPatterns, re)
		}
	}
}

func init() { prepare() }

// RuleCount 返回当前生效的规则条数与正则条数，用于前端展示审核引擎规模。
func RuleCount() (rules int, patterns int) {
	prepare()
	return len(ruleTable), len(allPatterns)
}

// Scan 对标题与正文执行规则扫描。
//
// 该函数不访问任何外部资源，是纯 CPU 计算，因此在并行引擎中可直接放在独立协程里执行。
func Scan(title, content string) Result {
	prepare()
	text := title + "\n" + content

	result := Result{RuleCount: len(ruleTable), ScannedLen: len([]rune(text))}

	for i := range ruleTable {
		rule := &ruleTable[i]
		hit := Hit{
			RuleID:   rule.ID,
			RuleName: rule.Name,
			Category: rule.Category,
			Severity: rule.Severity,
			Weight:   rule.Weight,
		}

		for _, kw := range rule.Keywords {
			if loc := strings.Index(text, kw); loc >= 0 {
				hit.Keywords = append(hit.Keywords, kw)
				hit.Evidences = append(hit.Evidences, clipEvidence(text, loc, len(kw)))
			}
		}

		for _, re := range rule.patterns {
			if loc := re.FindStringIndex(text); loc != nil {
				hit.Keywords = append(hit.Keywords, re.String())
				hit.Evidences = append(hit.Evidences, clipEvidence(text, loc[0], loc[1]-loc[0]))
			}
		}

		if len(hit.Keywords) == 0 {
			continue
		}
		if rule.Severity == "critical" {
			result.Critical = true
		}
		result.Score += rule.Weight
		result.Hits = append(result.Hits, hit)
	}

	if result.Score > 100 {
		result.Score = 100
	}
	sort.Slice(result.Hits, func(i, j int) bool { return result.Hits[i].Weight > result.Hits[j].Weight })
	return result
}

// clipEvidence 截取命中位置附近的片段作为证据，便于人工复核定位。
func clipEvidence(text string, loc, length int) string {
	r := []rune(text)
	// loc 是字节下标，转换为近似字符下标后取上下文窗口。
	start := loc - 12
	if start < 0 {
		start = 0
	}
	end := loc + length + 12
	if end > len(r) {
		end = len(r)
	}
	if start >= len(r) {
		start = len(r) - 1
	}
	if start < 0 {
		start = 0
	}
	segment := strings.Map(func(c rune) rune {
		if c == '\n' || c == '\r' || c == '\t' {
			return ' '
		}
		return c
	}, string(r[start:end]))
	segment = strings.TrimSpace(segment)
	if len([]rune(segment)) > 40 {
		segment = string([]rune(segment)[:40]) + "…"
	}
	return segment
}

// Summary 生成人类可读的规则命中摘要。
func Summary(hits []Hit) string {
	if len(hits) == 0 {
		return ""
	}
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		names = append(names, h.RuleName)
	}
	return fmt.Sprintf("命中规则：%s", strings.Join(names, "、"))
}
