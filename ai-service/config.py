"""AI 服务运行期配置。

全部可通过环境变量覆盖，未配置时使用保守的默认值。
"""

from __future__ import annotations

import os

# ---------------------------------------------------------------------------
# 大模型接入
# ---------------------------------------------------------------------------
# 兼容 OpenAI 协议的服务地址与模型名（默认对接 DeepSeek）。
LLM_API_KEY = os.getenv("DEEPSEEK_API_KEY") or os.getenv("LLM_API_KEY") or ""
LLM_BASE_URL = (os.getenv("LLM_BASE_URL") or "https://api.deepseek.com").rstrip("/")
LLM_MODEL = os.getenv("LLM_MODEL") or "deepseek-chat"
LLM_TIMEOUT = float(os.getenv("LLM_TIMEOUT", "45"))
LLM_MAX_RETRIES = int(os.getenv("LLM_MAX_RETRIES", "2"))

# ---------------------------------------------------------------------------
# 并行计算参数
# ---------------------------------------------------------------------------
# 服务端并发度：单个请求内部允许同时进行的 LLM 调用数。
# 调大可提升长文审核吞吐，但会成倍消耗大模型配额。
CONCURRENCY = max(1, int(os.getenv("AI_CONCURRENCY", "6")))

# 审核工作进程数：用于多进程并行审核池（>1 时启用多进程模式）。
try:
    import multiprocessing as _mp

    _CPU = _mp.cpu_count() or 2
except Exception:  # pragma: no cover - 极端环境兜底
    _CPU = 2
WORKER_PROCESSES = max(1, min(8, int(os.getenv("AI_WORKERS", str(max(1, _CPU // 2))))))

# 同时处理的 HTTP 请求数上限（每个请求内部还有 CONCURRENCY 级别的并发）。
MAX_INFLIGHT_REQUESTS = max(2, int(os.getenv("AI_MAX_INFLIGHT", "16")))

# ---------------------------------------------------------------------------
# 文本分片
# ---------------------------------------------------------------------------
# 单次送审的最大字符数：太长会超出上下文且审核粒度变粗。
CHUNK_SIZE = int(os.getenv("AI_CHUNK_SIZE", "600"))
# 相邻分片的重叠字符数，避免句子被硬切断导致语义丢失。
CHUNK_OVERLAP = int(os.getenv("AI_CHUNK_OVERLAP", "60"))
# 超过该长度才启用分片并行。
CHUNK_THRESHOLD = int(os.getenv("AI_CHUNK_THRESHOLD", "800"))

# ---------------------------------------------------------------------------
# 审核维度
# ---------------------------------------------------------------------------
# key 与 Go 侧 dimensionTable 保持一致，name 用于回传给前端展示。
AUDIT_DIMENSIONS = [
    {"key": "ad", "name": "广告导流", "weight": 30, "ko": False,
     "criteria": "营销推广、站外导流、联系方式引流、刷单兼职、诱导分享"},
    {"key": "abuse", "name": "辱骂攻击", "weight": 35, "ko": False,
     "criteria": "人身攻击、辱骂、地域或群体歧视、网络暴力"},
    {"key": "porn", "name": "色情低俗", "weight": 60, "ko": True,
     "criteria": "色情描写、低俗擦边、成人内容交易"},
    {"key": "politics", "name": "政治敏感", "weight": 60, "ko": True,
     "criteria": "危害国家安全、煽动颠覆、分裂国家的言论"},
    {"key": "illegal", "name": "违法违禁", "weight": 60, "ko": True,
     "criteria": "违禁品交易、毒品、赌博、诈骗、伪造证件"},
]

DIMENSION_BY_KEY = {d["key"]: d for d in AUDIT_DIMENSIONS}

# 规则兜底词库：当大模型不可用时提供基础审核能力（与 Go 侧规则引擎互为补充）。
SENSITIVE_HINTS = {
    "ad": ["加微信", "加vx", "私聊购买", "扫码进群", "免费领取", "兼职日结", "刷单"],
    "abuse": ["傻逼", "脑残", "废物", "去死", "垃圾东西"],
    "porn": ["约炮", "裸聊", "成人影片", "色情网站"],
    "politics": ["颠覆国家", "分裂国家", "煽动颠覆"],
    "illegal": ["枪支弹药", "冰毒", "办假证", "代开发票", "赌博网站", "博彩平台"],
}

# 严重程度阈值：风险分达到该值即判定为拒绝。
REJECT_SCORE = 60
# 疑似阈值：达到该值转人工复核。
REVIEW_SCORE = 30
