"""并行内容审核：分片并行 × 维度并行，并带有规则兜底。

两级并行结构：

    长篇正文 ──split──▶ 分片1 ─┐
                          分片2 ─┼─ 每个分片内部再并行送审 5 个审核维度
                          分片3 ─┘
                                   │
                              聚合：分片取最大风险 → 维度加权 → 判定

当大模型不可用时自动降级为词库/正则规则审核，保证服务始终可用（degraded 标记）。
"""

from __future__ import annotations

import logging
import time
from typing import Any

import config
import llm_client

logger = logging.getLogger("ai-service.audit")

_SCORE_BY_STATUS = {"pass": 0, "review": 40, "reject": 75}

_DIMENSION_SYSTEM_PROMPT = (
    "你是一名严格但公正的社区内容审核员。"
    "你只判断给定维度是否违规，不评价文笔，不做道德说教。"
    "必须只输出 JSON，不要输出任何解释性文字。"
)


def _dimension_prompt(dimension: dict[str, Any], text: str, title: str) -> str:
    return (
        f"审核维度：{dimension['name']}\n"
        f"判定标准：{dimension['criteria']}\n\n"
        f"文章标题：{title or '（无）'}\n"
        f"待审核文本片段：\n{text}\n\n"
        "请判断该片段在【" + dimension["name"] + "】维度上是否违规，"
        "并按以下 JSON 格式返回：\n"
        '{"status": "pass|review|reject", "score": 0-100, "hits": ["命中要素"], "reason": "20字以内的判断依据"}\n'
        "说明：pass 表示无违规，review 表示疑似需要人工确认，reject 表示明确违规。"
        "若片段与该维度无关，返回 pass、score 0。"
    )


def _rule_fallback(dimension_key: str, text: str) -> dict[str, Any]:
    """词库兜底审核：大模型不可用时的降级路径。"""
    hints = config.SENSITIVE_HINTS.get(dimension_key, [])
    hits = [word for word in hints if word in text]
    if not hits:
        return {"status": "pass", "score": 0, "hits": [], "reason": "规则未命中"}

    dimension = config.DIMENSION_BY_KEY.get(dimension_key, {})
    # 命中 KO 维度直接升级为 reject，其余维度按 review 处理，交由 Go 侧加权聚合。
    status = "reject" if dimension.get("ko") else "review"
    return {
        "status": status,
        "score": 70 if status == "reject" else 40,
        "hits": hits,
        "reason": f"本地词库命中：{'、'.join(hits[:3])}",
    }


def audit_dimension(dimension_key: str,
                    text: str,
                    title: str = "",
                    index: int = 0,
                    total: int = 1) -> dict[str, Any]:
    """对单个分片执行单维度审核（一次 LLM 调用，失败时降级到词库）。"""
    dimension = config.DIMENSION_BY_KEY.get(dimension_key)
    if dimension is None:
        return {"dimension": dimension_key, "status": "pass", "score": 0,
                "hits": [], "reason": "未知审核维度", "degraded": True, "elapsed_ms": 0}

    started = time.time()
    degraded = False

    if llm_client.llm_available():
        try:
            raw = llm_client.chat(
                messages=[
                    {"role": "system", "content": _DIMENSION_SYSTEM_PROMPT},
                    {"role": "user", "content": _dimension_prompt(dimension, text, title)},
                ],
                temperature=0.1,
                max_tokens=300,
                json_mode=True,
            )
            parsed = llm_client.extract_json(raw)
            status = str(parsed.get("status", "pass")).lower()
            if status not in _SCORE_BY_STATUS:
                status = "review"
            try:
                score = int(parsed.get("score", _SCORE_BY_STATUS[status]))
            except (TypeError, ValueError):
                score = _SCORE_BY_STATUS[status]
            score = max(0, min(100, score))
            hits = parsed.get("hits") or []
            if not isinstance(hits, list):
                hits = [str(hits)]
            result = {
                "dimension": dimension_key,
                "name": dimension["name"],
                "status": status,
                "score": score,
                "hits": [str(h)[:40] for h in hits][:5],
                "reason": str(parsed.get("reason", ""))[:120],
                "degraded": False,
            }
        except Exception as exc:  # noqa: BLE001
            logger.warning("维度 %s 审核降级到规则: %s", dimension_key, exc)
            result = _rule_fallback(dimension_key, text)
            result["dimension"] = dimension_key
            result["name"] = dimension["name"]
            result["degraded"] = True
            degraded = True
    else:
        result = _rule_fallback(dimension_key, text)
        result["dimension"] = dimension_key
        result["name"] = dimension["name"]
        result["degraded"] = True
        degraded = True

    result["index"] = index
    result["total"] = total
    result["elapsed_ms"] = int((time.time() - started) * 1000)
    result["degraded"] = degraded
    return result


def audit_chunk(text: str,
                title: str = "",
                index: int = 0,
                total: int = 1,
                dimension: str | None = None,
                concurrency: int | None = None) -> dict[str, Any]:
    """审核一个文本分片。

    dimension 为空时，并行送审全部维度并聚合；
    指定 dimension 时只审核该维度（Go 侧按维度并行时使用该模式）。
    """
    started = time.time()

    if dimension:
        result = audit_dimension(dimension, text, title, index, total)
        result["elapsed_ms"] = int((time.time() - started) * 1000)
        return {
            "index": index,
            "total": total,
            "dimension": dimension,
            "status": result["status"],
            "score": result["score"],
            "hits": result["hits"],
            "reasons": [result["reason"]] if result.get("reason") else [],
            "degraded": result["degraded"],
            "dimension_results": [result],
            "elapsed_ms": result["elapsed_ms"],
        }

    keys = [d["key"] for d in config.AUDIT_DIMENSIONS]
    dimension_results = llm_client.parallel_map(
        lambda key: audit_dimension(key, text, title, index, total),
        keys,
        concurrency=concurrency,
    )
    dimension_results = [r for r in dimension_results if r]

    score = max((int(r.get("score", 0)) for r in dimension_results), default=0)
    status = "pass"
    if score >= config.REJECT_SCORE or any(r.get("status") == "reject" for r in dimension_results):
        status = "reject"
    elif score >= config.REVIEW_SCORE or any(r.get("status") == "review" for r in dimension_results):
        status = "review"

    hits: list[str] = []
    reasons: list[str] = []
    for r in dimension_results:
        for h in r.get("hits", []):
            if h not in hits:
                hits.append(h)
        if r.get("reason"):
            reasons.append(f"{r.get('name', r.get('dimension'))}：{r['reason']}")

    return {
        "index": index,
        "total": total,
        "dimension": None,
        "status": status,
        "score": score,
        "hits": hits[:10],
        "reasons": reasons[:6],
        "degraded": all(r.get("degraded") for r in dimension_results) if dimension_results else True,
        "dimension_results": dimension_results,
        "elapsed_ms": int((time.time() - started) * 1000),
    }


def audit_text(title: str,
               content: str,
               concurrency: int | None = None,
               force_chunk: bool = False) -> dict[str, Any]:
    """完整审核流程：分片并行 → 分片内维度并行 → 聚合。

    返回结果中同时包含：
      * status / score：汇总判定；
      * chunks：每个分片的明细，前端可据此展示"哪个片段有问题"；
      * elapsed_ms / sequential_ms / speedup：并行收益度量。
    """
    started = time.time()
    content = content or ""
    title = title or ""

    use_chunks = force_chunk or len(content) > config.CHUNK_THRESHOLD
    if use_chunks:
        pieces = llm_client.split_chunks(content)
        if not pieces:
            pieces = [title]
        mode = "chunked-parallel"
    else:
        pieces = [f"{title}\n{content}".strip()]
        mode = "dimension-parallel"

    chunk_results = llm_client.parallel_map(
        lambda pair: audit_chunk(pair[1], title, pair[0], len(pieces), concurrency=concurrency),
        list(enumerate(pieces)),
        concurrency=concurrency,
    )
    chunk_results = [r for r in chunk_results if r]

    # 聚合分片：最坏分片决定整篇判定。
    worst = "pass"
    score = 0
    hits: list[str] = []
    reasons: list[str] = []
    sequential_ms = 0
    for r in chunk_results:
        sequential_ms += int(r.get("elapsed_ms", 0))
        score = max(score, int(r.get("score", 0)))
        rank = {"pass": 0, "review": 1, "reject": 2}
        if rank.get(r.get("status", "pass"), 0) > rank.get(worst, 0):
            worst = r["status"]
        for h in r.get("hits", []):
            if h not in hits:
                hits.append(h)
        for reason in r.get("reasons", []):
            if reason not in reasons and len(reasons) < 6:
                reasons.append(reason)

    elapsed = int((time.time() - started) * 1000)

    # 并行收益度量：串行耗时 = 各分片（含片内维度）耗时之和；串行度 = 分片数 × 维度数。
    dimension_count = len(config.AUDIT_DIMENSIONS)
    tasks = sum(1 for r in chunk_results for _ in r.get("dimension_results", []))
    return {
        "status": worst,
        "score": score,
        "hits": hits[:10],
        "reasons": reasons,
        "mode": mode,
        "chunks": len(chunk_results),
        "chunk_results": chunk_results,
        "dimensions": dimension_count,
        "tasks": tasks,
        "elapsed_ms": max(1, elapsed),
        "sequential_ms": max(sequential_ms, 1),
        "speedup": round(max(sequential_ms, 1) / max(elapsed, 1), 2),
        "degraded": all(r.get("degraded") for r in chunk_results) if chunk_results else True,
        "concurrency": concurrency or config.CONCURRENCY,
    }


def audit_many(items: list[dict[str, Any]],
               concurrency: int | None = None) -> dict[str, Any]:
    """批量并行审核：多个独立内容同时送审（请求内一级任务并行）。

    Go 侧批量审核时也会并行发起多个请求，两者叠加形成"请求间 × 请求内"两级并行；
    这里额外提供一个服务端批量入口，便于直接压测与对照。
    """
    started = time.time()

    def _one(pair: tuple[int, dict[str, Any]]) -> dict[str, Any]:
        idx, item = pair
        result = audit_text(item.get("title", ""), item.get("content", ""), concurrency=concurrency)
        result["index"] = idx
        return result

    results = llm_client.parallel_map(_one, list(enumerate(items)), concurrency=concurrency)
    results = [r for r in results if r]
    elapsed = int((time.time() - started) * 1000)
    sequential = sum(int(r.get("elapsed_ms", 0)) for r in results)
    return {
        "results": results,
        "count": len(results),
        "elapsed_ms": max(1, elapsed),
        "sequential_ms": max(sequential, 1),
        "speedup": round(max(sequential, 1) / max(elapsed, 1), 2),
        "concurrency": concurrency or config.CONCURRENCY,
        "strategy": "batch-parallel × inner-parallel",
    }


def analyze_words(text: str) -> dict[str, Any]:
    """大模型词库辅助分析：用规则词库快速定位可疑片段。

    该接口是纯 CPU 计算，在 Go 侧并行流水线中可作为"快速通道"的补充信号。
    """
    started = time.time()
    matched: dict[str, list[str]] = {}
    for key, words in config.SENSITIVE_HINTS.items():
        found = [w for w in words if w in text]
        if found:
            matched[key] = found

    return {
        "matched": matched,
        "total": sum(len(v) for v in matched.values()),
        "dialect": "llm-assisted",
        "elapsed_ms": int((time.time() - started) * 1000),
    }
