# 并行计算与AI应用 —— 面向 UGC 社区的智能内容审核与发布系统

一个面向社区用户的智能内容审核与发布系统：针对传统社区平台**内容发布流程繁琐、人工审核效率低、不良信息识别困难**的问题，引入**并行计算**与**人工智能**技术辅助社区内容审核与管理，提高社区内容管理的效率与智能化水平。

系统的技术主线是**并行计算**：把"审核一篇内容"拆解成多个彼此独立的计算任务，用 Go 协程池与 Python 多进程/多线程同时执行，再用加权策略把结果汇聚成可解释的审核结论，并实测加速比。

---

## 一、系统功能

系统以论坛形态呈现，围绕 **用户管理 → 内容发布 → 智能审核 → AI 辅助** 四条主线设计：

| 模块 | 功能 |
| --- | --- |
| **用户管理** | 注册（用户名/密码校验 + bcrypt 加盐哈希）、登录（JWT 签发）、注销、身份认证与角色区分（普通用户 / 审核员）、用户主页 |
| **论坛形态** | 6 个内置固定版块、版块分组与统计、帖子流（最新回复/最新发布/最多浏览/精华四种排序）、关键词搜索、置顶与加精 |
| **内容发布** | 发帖（选版块）、编辑、删除、存草稿、我的帖子管理（按状态筛选）、标签 |
| **帖子互动** | 楼层式回复（主楼 + 楼中楼引用）、回复点赞、帖子点赞、作者/审核员删除回复 |
| **内容审核** | 发帖与回复均自动触发 AI 审核；规则引擎 + 多维度语义审核并行执行；结论分为"通过 / 待人工复核 / 拒绝"，并反馈给用户；违规回复直接拦截不入库；审核员后台人工复核 |
| **AI 辅助** | 大语言模型辅助生成文章；长文 Map-Reduce 并行总结；热榜并行总结与趋势分析 |
| **热点展示** | Redis ZSet 热榜排名、版块统计、标签热度聚合（并发执行） |
| **并行可视化** | 审核过程 SSE 实时推送、并行执行甘特图、加速比 / 峰值并行度 / 串行等价耗时 |

### 页面结构

| 路由 | 页面 | 说明 |
| --- | --- | --- |
| `#/` | 论坛首页 | 站点横幅与数字、版块列表（含帖数/回复数/今日新帖）、最新主题、侧边栏热门讨论 |
| `#/b/:slug` | 版块页 | 该版块下的帖子流 |
| `#/latest` `#/hot` `#/essence` | 排序页 | 最新发布 / 最多浏览 / 精华 |
| `#/t/:id` | 帖子详情 | 正文 + AI 审核标记 + 点赞/回复/AI总结/编辑/置顶加精 + 楼层式回复 + 回复框 |
| `#/compose` | 发帖 / 编辑 | 选版块、写正文、AI 帮我写、提交后实时展示并行审核过程 |
| `#/u/:id` | 用户主页 | 该用户的帖子与回复、发帖/回复/获赞统计 |
| `#/me` | 我的帖子 | 按状态筛选管理自己的帖子、批量并行审核草稿、审核员复核队列 |
| `#/audit-lab` | 并行审核实验室 | 任意文本的并行审核实时可视化，可调并行度对比 |
| `#/login` | 登录 / 注册 | — |


---

## 二、并行计算设计（核心）

### 2.1 多阶段并行审核流水线

传统做法是把审核点串行执行：先查敏感词 → 再调模型判广告 → 再判辱骂 → 再判涉政……总耗时是各部分之和。
本系统把**互不依赖的审核任务同时启动**，总耗时趋近于"最慢的那条分支"：

```
                         ┌── 阶段1  规则引擎（本地 CPU，毫秒级"快速通道"）
                         │
  用户提交内容 ──fan-out─┼── 阶段2  多维度语义审核（5 路并行）
                         │            ├── 广告导流    （LLM）
                         │            ├── 辱骂攻击    （LLM）
                         │            ├── 色情低俗    （LLM，一票否决）
                         │            ├── 政治敏感    （LLM，一票否决）
                         │            └── 违法违禁    （LLM，一票否决）
                         │
                         └── 阶段3  长文分片并发审核（分片并行 × 片内维度再并行）
                                            │
                                      fan-in 加权聚合 → 风险分 → 判定
```

**两级并行叠加**：长文场景下"分片并行"与"维度并行"相乘，例如 4 个分片 × 5 个维度 = 20 个 LLM 调用同时在途（受并行度上限约束）。

### 2.2 关键实现

| 机制 | 位置 | 说明 |
| --- | --- | --- |
| 工作协程池 (`fan-out` / `fan-in`) | `internal/service/audit_engine.go` | 信号量控制并行度，带缓冲结果通道收集，边完成边处理；同时测量峰值并行度 |
| 任务时间线采集 | 同上 `TaskTrace` | 记录每个任务的起止时刻（纳秒精度），前端据此画甘特图 |
| 长文分片 | 同上 `splitChunks` | 按句子边界切分并保留 40 字重叠，避免违规内容横跨接缝被漏判 |
| 可解释加权聚合 | 同上 `aggregate` | 规则分 + 各维度风险分，含"一票否决"与"AI 全降级不放行"策略 |
| 批量并行审核 | 同上 `BatchAudit` | 文章之间再并行一层，形成"请求间并行 × 请求内并行" |
| 并行热点聚合 | `internal/service/aggregation.go` | 热榜 / 统计 / 标签三个子任务并发查询后 fan-in |
| Map-Reduce 并行总结 | 同上 `SummarizeHotParallel` | Map 阶段并行生成各篇摘要，Reduce 阶段归并 |
| 并行写回 | 同上 `FlushViewCounts` | 浏览量在 Redis 缓冲，定时并发批量回写数据库（写合并） |
| Python 侧并行 | `ai-service/llm_client.py` | 线程池隐藏 LLM 网络延迟 + 多进程工作池 + 全局并发闸门 |
| Python 分片并行 | `ai-service/audit.py` | 分片并行 + 片内维度并行，LLM 不可用时降级为词库规则 |

### 2.3 并行效果实测

`internal/service/live_audit_test.go` 会对**真实的 AI 服务**跑一次完整审核并打印各项指标。实测输出：

```
[事件]     0ms pipeline   并行审核启动：策略=dimension，正文 64 字，切分 1 片，并行度 8
[事件]     0ms rules      规则引擎未命中敏感规则
[事件]   796ms dimensions 5 个语义维度并行审核完成
[事件]   796ms pipeline   并行审核完成：未发现违规内容（风险分 0），
                          实际耗时 796 ms，串行等价 3101 ms，加速比 3.90x，峰值并行度 5

  阶段 规则引擎（本地 CPU 快速通道）   起止=[   0,   0]ms 耗时=  0ms 任务=1
  阶段 多维度语义审核（5 路并行）      起止=[   0, 796]ms 耗时=796ms 任务=5
      └ 广告导流   起止=[   0, 919]ms 耗时=914ms worker=2
      └ 辱骂攻击   起止=[   0, 851]ms 耗时=847ms worker=1
      └ 色情低俗   起止=[   0, 846]ms 耗时=842ms worker=4
      └ 政治敏感   起止=[   0, 791]ms 耗时=788ms worker=3
      └ 违法违禁   起止=[   0, 607]ms 耗时=604ms worker=5

规则阶段与语义阶段的启动时间差: 0ms（越小说明并行度越高）
```

5 个语义维度**启动时刻全部为 0ms**（真正同时开始），5 次 LLM 调用累计耗时 3995ms，实际只用 796ms 完成，**加速比 3.90x**。

> 指标口径：
> - `sequential_ms`（串行等价耗时）= 所有叶子任务耗时之和，即"不用并行需要多久"；
> - `critical_path_ms`（关键路径耗时）= 各顶层阶段耗时之和，即"阶段串行、阶段内并行"的长度；
> - `speedup = sequential_ms / elapsed_ms`；
> - `peak_concurrency` = 实测峰值并行度（时间线重叠数 / 并发计数）。

### 2.4 审核结论如何产生

```
规则引擎命中致命规则            → 拒绝
任一 KO 维度（涉黄/涉政/违法）违规 → 拒绝（一票否决）
风险分 ≥ 60                     → 拒绝
风险分 ≥ 30 或存在"待复核"维度   → 转人工复核
AI 服务全部不可用且规则无命中     → 转人工复核（绝不静默放行）
其余                            → 通过，文章自动发布
```

规则引擎是纯 CPU 计算、毫秒级返回，在流水线中充当**快速通道**；即使大模型不可用，系统仍能依靠规则库完成基础审核（响应中带 `degraded` 标记），保证可用性。

---

## 三、系统架构

前后端分离 + 独立 AI 服务：

```
┌────────────────────────┐
│  前端（Vue 3 单页应用）  │  页面展示、用户交互、并行审核实时可视化
└───────────┬────────────┘
            │ HTTP / SSE
┌───────────▼────────────────────────────────┐
│  后端 Go + Gin                              │
│  ├─ handler    接口层（注册登录/文章/审核/AI）│
│  ├─ middleware JWT 鉴权 + Redis 白名单       │
│  ├─ service    业务 + 并行审核编排引擎        │
│  ├─ rules      本地规则引擎（敏感词/正则）     │
│  └─ repository GORM(MySQL) + Redis           │
└───────┬──────────────────────┬──────────────┘
        │                      │ HTTP（并行调用）
┌───────▼──────┐   ┌───────────▼──────────────────────┐
│ MySQL  8.0   │   │ Python 并行 AI 服务（零第三方依赖）│
│ 业务数据存储  │   │ 多进程工作池 × 线程池 × 分片并行   │
└──────────────┘   └──────────────────────────────────┘
┌──────────────┐
│ Redis 7      │  缓存、Token 白名单、热榜 ZSet、浏览量缓冲
└──────────────┘
```

**技术选型**

| 层次 | 技术 |
| --- | --- |
| 后端 | Go 1.25、Gin、GORM、JWT（golang-jwt/v5）、bcrypt |
| 前端 | Vue 3（CDN 引入）、Axios、原生 SSE 流式解析 |
| 存储 | MySQL 8.0（业务数据）、Redis 7（缓存 / 热榜 / 限流缓冲） |
| AI 服务 | Python 3.11+，**仅使用标准库**（http.server / urllib / multiprocessing / concurrent.futures） |
| 大模型 | 兼容 OpenAI 协议（默认 DeepSeek `deepseek-chat`，通过 `response_format` 约束 JSON 输出） |

---

## 四、目录结构

```
content-community/
├── cmd/main.go                     # 服务入口：路由注册、CORS、后台浏览量回写
├── internal/
│   ├── handler/                    # 接口层
│   │   ├── ai.go                   # AI 生成/总结 + 并行审核 + SSE 流式审核
│   │   ├── article.go              # 文章 CRUD、发布审核、人工复核、审核记录
│   │   ├── user.go                 # 注册、登录、注销、个人中心
│   │   └── common.go               # 上下文取值与参数解析helper
│   ├── middleware/auth.go          # JWT 验签 + Redis 白名单 + 审核员校验
│   ├── model/                      # 数据模型（User / Article / AuditRecord）
│   ├── repository/db.go            # MySQL 连接池 + Redis + AutoMigrate
│   ├── rules/engine.go             # 本地规则引擎（敏感词 + 正则 + 证据提取）
│   └── service/
│       ├── audit_engine.go         # ★ 并行审核引擎（fan-out/fan-in、时间线、加权聚合）
│       ├── audit_engine_test.go    # 并行原语与判定策略单元测试
│       ├── live_audit_test.go      # 对真实 AI 服务的联调与性能验证
│       ├── aggregation.go          # 并行热点聚合、Map-Reduce 总结、写回
│       ├── article.go              # 文章业务与发布链路
│       ├── ai_service.go           # 调用 Python AI 服务的客户端
│       ├── user.go                 # 用户与鉴权业务
│       └── seed.go                 # 审核员账号初始化
├── ai-service/                     # Python 并行 AI 服务
│   ├── main.py                     # 标准库 HTTP 服务 + 路由
│   ├── audit.py                    # 分片并行 × 维度并行审核 + 词库降级
│   ├── generate.py                 # 文章生成 + Map-Reduce 并行总结
│   ├── llm_client.py               # LLM 客户端、线程池、分片、并发闸门
│   └── config.py                   # 并行参数与审核维度配置
├── frontend/                       # Vue 3 单页应用
│   ├── index.html                  # 页面结构 + 审核结果可视化组件模板
│   ├── app.js                      # 业务逻辑、SSE 流式解析、甘特图计算
│   └── style.css
├── deploy/schema.sql               # 数据库设计（含注释与索引）
├── docker-compose.yml              # 一键启动 MySQL + Redis + AI + 后端
├── Dockerfile / ai-service/Dockerfile
├── Makefile                        # 常用命令封装
└── .env.example / ai-service/.env.example
```

---

## 五、快速开始

### 方式一：Docker 一键启动（推荐）

```bash
cp .env.example .env
cp ai-service/.env.example ai-service/.env
# 编辑 ai-service/.env，填入 DEEPSEEK_API_KEY（不填则审核降级为规则模式）

docker compose up -d --build
```

访问：

- 前端页面 <http://localhost:8080>
- AI 服务健康检查 <http://localhost:8000/health>

### 方式二：本地分别启动

```bash
# 1) 启动依赖（MySQL / Redis）
docker compose up -d mysql redis

# 2) 启动 Python AI 并行服务
cd ai-service && python main.py          # 监听 :8000

# 3) 启动 Go 后端（按需修改 .env 中的 DB_HOST/REDIS_HOST 为 localhost）
go run ./cmd/main.go                     # 监听 :8080
```

启动后打开 <http://localhost:8080>。Go 服务会直接托管 `frontend/` 静态页面（`/` 与 `/static`），无需额外的前端构建步骤。

> 不建议用浏览器直接打开 `frontend/index.html`：`file://` 协议下浏览器的跨域策略会拦截 API 请求。
> 如需独立调试前端，可在 `frontend/` 目录执行 `python -m http.server 5500` 后访问 <http://localhost:5500>。

首次启动会自动创建审核员账号：

| 账号 | 密码 | 角色 |
| --- | --- | --- |
| `admin` | `admin123456` | 审核员（可人工复核） |

也可自行注册普通用户。

### 常用命令

```bash
make test      # 运行并行引擎单元测试
make lint      # go vet 静态检查
make up        # docker compose 一键启动
make logs-ai   # 查看 AI 服务日志
```

---

## 六、主要接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/register` `/api/login` `/api/logout` | 注册 / 登录 / 注销 |
| GET | `/api/me` | 当前用户资料与内容统计 |
| GET | `/api/users/:id/profile` | 用户公开主页（发帖/回复/获赞 + 最近动态） |
| GET | `/api/boards` | 版块列表（含帖子数/回复数/今日新帖统计） |
| GET | `/api/boards/:slug` | 单个版块 |
| GET | `/api/forum/overview` | 论坛首页概览（**4 个子任务并行聚合**） |
| GET | `/api/articles` | 帖子流（`board`/`sort`/`keyword`/`tag`/分页） |
| GET | `/api/articles/hot` | 热门内容（并行聚合 + 标签热度） |
| GET | `/api/articles/:id` | 帖子详情（含当前用户点赞状态） |
| POST/PUT/DELETE | `/api/articles[/:id]` | 发帖（选版块）/ 编辑 / 删除 |
| GET | `/api/my/articles` | 我的帖子管理 |
| POST | `/api/articles/:id/publish` | **触发 AI 审核并发布**（审核→发布闭环） |
| GET/POST | `/api/articles/:id/replies` | 回复列表（两层结构）/ **发表回复（并行审核）** |
| DELETE | `/api/replies/:replyId` | 删除回复 |
| POST | `/api/likes/toggle` | 帖子 / 回复点赞切换 |
| GET | `/api/audit/records` | 审核记录 |
| POST | `/api/admin/articles/:id/review` | 人工复核（审核员） |
| PUT | `/api/admin/articles/:id/flags` | 置顶 / 加精 / 调整版块（审核员） |
| POST | `/api/ai/audit` | 并行审核（同步返回完整结果与性能指标） |
| POST | `/api/ai/audit/stream` | **并行审核 SSE 实时推送**（前端甘特图数据源） |
| POST | `/api/ai/audit/batch` | 批量并行审核 |
| GET | `/api/ai/audit/info` | 审核引擎能力（维度、权重、并行度） |
| GET | `/api/ai/stats` | 审核与并行的历史收益统计 |
| POST | `/api/ai/generate` | AI 辅助生成文章 |
| POST | `/api/ai/summary` | 热榜并行总结与趋势分析 |
| POST | `/api/ai/articles/:id/summary` | 单篇长文 Map-Reduce 并行总结 |
| GET | `/api/ai/health` | AI 服务健康与并行运行时状态 |

### 快速体验并行审核

```bash
# 1) 登录拿 Token
curl -s -X POST http://localhost:8080/api/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123456"}'

# 2) 并行审核一段广告导流文本（stream 为 SSE，这里用同步接口便于查看）
curl -s -X POST http://localhost:8080/api/ai/audit \
  -H 'Content-Type: application/json' -H "Authorization: Bearer <TOKEN>" \
  -d '{"title":"低价货源","content":"加微信 abc12345 详聊，兼职日结，扫码进群免费领取","mode":"semantic","concurrency":8}'
```

返回结果中包含 `verdict`、`risk_score`、`reason`、`hits`、`dimensions`、`stages`（各阶段与任务的时间线）以及 `elapsed_ms` / `sequential_ms` / `speedup` / `peak_concurrency` 等并行指标。

前端「**⚡ 并行审核实验室**」页面可直接体验：填入文本 → 点击"并行审核"→ 实时看到各并行分支的执行过程与甘特图，并可拖动滑块调整并行度对比效果。

---

## 七、测试

```bash
go test ./... -count=1            # 全部单元测试
go vet ./...                      # 静态检查
```

单元测试覆盖：

- `TestSplitChunks` / `TestSplitChunksCoverage`：长文分片长度约束、句子边界切分与内容不丢失
- `TestMaxOverlap`：并行度测量（重叠 4 路 / 串行 1 路 / 两路重叠）
- `TestFanOutActuallyParallel`：工作协程池确实并发执行（峰值并发与耗时双重断言）
- `TestAggregateVerdict`：加权判定策略（通过 / 转人工 / 超阈值拒绝 / KO 一票否决 / 致命规则 / AI 全降级不放行）
- `TestRuleEngineScoring`：规则引擎命中与证据提取
- `TestLiveAuditPipeline`：**对真实 AI 服务的端到端联调**（需 `AI_SERVICE_URL`，未设置时自动跳过）

```bash
# 联调测试（需先启动 AI 服务）
AI_SERVICE_URL=http://127.0.0.1:8000 go test ./internal/service/ -run TestLiveAuditPipeline -v
```

### 完整业务链路验证脚本

`deploy/e2e_check.py` 会对着已启动的服务跑一遍真实业务流程，共 84 项检查
（注册登录 → 发帖 → 并行审核发布 → 帖子流/热榜 → AI 生成与总结 → 批量审核 → 人工复核
→ 鉴权越权防护 → 编辑重审 → 收益统计 → **版块/回复/楼中楼/点赞/置顶加精/用户主页**）：

```bash
# 需先 docker compose up -d
python deploy/e2e_check.py
```

脚本使用**临时账号**（带时间戳后缀），运行后会向数据库写入测试数据；
如需清理，执行 `docker compose down -v && docker compose up -d` 即可重置。

在真实大模型服务下的实测结果（单项审核）：

```
判定=pass 风险分=0
实际耗时=1280ms 串行等价=5307ms 加速比=4.15x 峰值并行度=5

帖子审核：实际 1206ms 加速比 4.30x
回复审核：实际 1136ms 串行等价 4986ms 加速比 4.39x
违规回复：被拦截（一票否决维度：违法违禁 + 规则命中"加微信/兼职日结"）
批量并行审核 4 篇：实际 3809ms 串行等价 37454ms 加速比 9.83x
论坛首页并行聚合 4 个子任务：加速比 2.5～3.7x
累积统计：平均加速比 3.33x / 最高 4.8x
```

---

## 八、降级与容错设计

| 场景 | 表现 |
| --- | --- |
| 未配置大模型密钥 | 审核降级为本地词库/正则规则，接口照常返回并标记 `degraded` |
| 大模型超时/限流 | 自动指数退避重试；仍失败则该维度降级，其余维度结论仍然有效 |
| AI 服务整体不可用 | Go 侧捕获错误并把所有维度标记为降级 → 结论转**人工复核**（绝不静默放行） |
| Redis 不可用 | 启动时告警不中断；缓存与热榜退化为数据库直连与浏览量排序 |
| 单个并行任务失败 | 通过结果通道标记 `error`，不影响同批次其他任务，避免拖垮整批 |
| 上游请求超时 | 流水线统一 `context` 截止，在途分支立即取消，防止协程泄漏 |
| 单请求异常 | Python 服务逐请求 try/except 兜底，返回 500 而不影响其他在途请求 |

---

## 九、可扩展方向

- 规则库改为配置中心/数据库下发，支持热更新与运营自助维护
- 引入消息队列把审核异步化，支持超长文本与视频/图片多模态审核
- 大模型结果缓存（相同内容指纹命中直接复用），进一步降低调用成本
- 审核记录接入监控看板，观测加速比、误杀率与人工复核量随时间的变化
- 人工复核结果回流为训练/提示词优化数据，形成"AI 审核 + 人工反馈"的闭环
