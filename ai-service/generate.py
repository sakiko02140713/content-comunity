"""AI 辅助创作与并行文章总结。

文章总结采用 Map-Reduce 并行模式：

    Map   ：长文切分为 N 片，N 片并行生成片段摘要        （N 路并行）
    Reduce：把 N 片摘要并行归并为一段最终摘要            （1 次调用）

串行做法需要 N 次调用依次排队，总耗时约 N×T；
并行后总耗时约为 T_map + T_reduce，接近单次调用耗时。
"""

from __future__ import annotations

import logging
import time
from typing import Any

import config
import llm_client

logger = logging.getLogger("ai-service.generate")

_POLISH_SYSTEM_PROMPT = (
    "你是一位资深的中文编辑，负责润色作者已经写好的文章草稿。\n"
    "必须遵守以下原则：\n"
    "1. 保留作者的原意、观点、立场与叙事顺序，不得改变文章主旨；\n"
    "2. 保留作者的个人语气与表达风格，不要改写成另一种文风；\n"
    "3. 保留原文中的所有事实、数字、专有名词、代码与引用，不得增删或篡改；\n"
    "4. 不要凭空添加原文没有的内容、例子或结论；\n"
    "5. 只做语言层面的打磨：修正错别字与语病、理顺句子结构、调整标点、"
    "增强段落之间的衔接、去掉冗余重复的表达。\n"
    "输出要求：只输出润色后的完整正文，不要输出修改说明、对比、标题或任何解释性文字，"
    "不要用 Markdown 代码块包裹全文。"
)

_POLISH_TITLE_PROMPT = (
    "你是一位标题编辑。请在不改变原意、不夸大、不标题党的前提下润色标题，"
    "使其更准确通顺，长度控制在 30 字以内。只输出润色后的标题本身，不要加引号或说明。"
)

_SUMMARY_SYSTEM_PROMPT = (
    "你是一位擅长信息压缩的编辑。请用简洁的中文概括给定内容的核心信息，"
    "保留关键结论与数据，不要添加原文没有的内容。"
)


def polish_article(title: str, content: str, style: str = "") -> dict[str, Any]:
    """润色用户已经写好的文章正文。

    与"生成"的本质区别：本函数不产出新内容，只对用户提供的文本做语言层面的打磨。
    content 为空时直接拒绝，从接口层面保证"必须先有用户内容"。
    """
    started = time.time()
    content = (content or "").strip()
    if not content:
        raise ValueError("润色需要先提供正文内容")

    if not llm_client.llm_available():
        # 降级：无大模型时不做任何改写，原样返回并明确标记未润色，
        # 避免用机器模板覆盖用户自己的文字。
        return {
            "polished_content": content,
            "title": title,
            "changed": False,
            "degraded": True,
            "model": "offline-passthrough",
            "elapsed_ms": int((time.time() - started) * 1000),
        }

    user_prompt = f"文章标题：{title or '（未填写）'}\n\n需要润色的正文如下：\n{content}"
    if style:
        user_prompt += f"\n\n额外要求（仅限语言风格层面）：{style}"

    polished = llm_client.chat(
        messages=[
            {"role": "system", "content": _POLISH_SYSTEM_PROMPT},
            {"role": "user", "content": user_prompt},
        ],
        temperature=0.35,
        max_tokens=2600,
    ).strip()

    # 标题也顺带润色，失败时保留原标题，不影响正文结果
    polished_title = (title or "").strip()
    if polished_title:
        try:
            new_title = llm_client.chat(
                messages=[
                    {"role": "system", "content": _POLISH_TITLE_PROMPT},
                    {"role": "user", "content": polished_title},
                ],
                temperature=0.3,
                max_tokens=80,
            ).strip().strip('"“”《》')
            if 0 < len(new_title) <= 60:
                polished_title = new_title
        except Exception as exc:  # noqa: BLE001 - 标题润色失败不影响正文
            logger.warning("标题润色失败，保留原标题: %s", exc)

    return {
        "polished_content": polished,
        "title": polished_title,
        "changed": polished != content,
        "degraded": False,
        "model": config.LLM_MODEL,
        "elapsed_ms": int((time.time() - started) * 1000),
    }


def summarize_text(text: str, title: str = "") -> str:
    """生成单段文本摘要（Map 阶段的最小单元）。"""
    if not llm_client.llm_available():
        # 降级：取前若干字符作为摘要，保证流程可用。
        cleaned = " ".join((text or "").split())
        return cleaned[:120] + ("…" if len(cleaned) > 120 else "")

    prefix = f"文章标题：{title}\n" if title else ""
    return llm_client.chat(
        messages=[
            {"role": "system", "content": _SUMMARY_SYSTEM_PROMPT},
            {"role": "user", "content": f"{prefix}请用不超过 120 字概括以下内容：\n{text}"},
        ],
        temperature=0.3,
        max_tokens=300,
    ).strip()


def article_summary(title: str,
                    content: str,
                    concurrency: int | None = None) -> dict[str, Any]:
    """Map-Reduce 并行文章总结。

    返回 chunk_summaries 便于前端展示"并行 map 阶段产出了哪些片段摘要"。
    """
    started = time.time()
    content = content or ""
    pieces = llm_client.split_chunks(content)

    if len(pieces) <= 1:
        # 短文无需 map-reduce，直接单次总结。
        answer = summarize_text(content or title, title)
        elapsed = int((time.time() - started) * 1000)
        return {
            "answer": answer,
            "chunk_summaries": [answer],
            "chunk_count": 1,
            "mode": "single-pass",
            "elapsed_ms": max(1, elapsed),
            "sequential_ms": max(1, elapsed),
            "speedup": 1.0,
            "concurrency": 1,
        }

    # ---- Map 阶段：分片并行总结 ----
    chunk_summaries = llm_client.parallel_map(
        lambda pair: summarize_text(pair[1], f"{title}（第 {pair[0] + 1} 段）"),
        list(enumerate(pieces)),
        concurrency=concurrency,
    )
    chunk_summaries = [s for s in chunk_summaries if s]
    map_elapsed = int((time.time() - started) * 1000)

    # ---- Reduce 阶段：归并为最终摘要 ----
    joined = "\n".join(f"{i + 1}. {s}" for i, s in enumerate(chunk_summaries))
    if llm_client.llm_available():
        answer = llm_client.chat(
            messages=[
                {"role": "system", "content": _SUMMARY_SYSTEM_PROMPT},
                {"role": "user", "content": (
                    f"以下是《{title}》各段落的分段摘要，请归并成一段不超过 250 字的整体摘要，"
                    f"突出核心结论：\n{joined}"
                )},
            ],
            temperature=0.3,
            max_tokens=500,
        ).strip()
    else:
        answer = " ".join(chunk_summaries)[:250]

    elapsed = int((time.time() - started) * 1000)
    return {
        "answer": answer,
        "chunk_summaries": chunk_summaries,
        "chunk_count": len(pieces),
        "mode": "map-reduce-parallel",
        "elapsed_ms": max(1, elapsed),
        # 串行等价耗时：各分片依次总结（每片约等于并行阶段的平均耗时）+ 归并
        "sequential_ms": max(1, map_elapsed * len(pieces) // max(1, len(chunk_summaries)) + (elapsed - map_elapsed)),
        "speedup": round(
            max(1, map_elapsed * len(pieces) // max(1, len(chunk_summaries))) / max(1, elapsed), 2
        ),
        "concurrency": concurrency or config.CONCURRENCY,
    }


def summarize_hotlist(articles: list[dict[str, Any]], question: str = "") -> dict[str, Any]:
    """热榜整体分析：把各篇文章的标题/阅读量/摘要汇总后交给大模型分析趋势。"""
    started = time.time()
    question = question or "请总结当前社区热榜的内容趋势，并推荐最值得阅读的文章"

    lines = []
    for i, a in enumerate(articles):
        line = f"{i + 1}. 《{a.get('title', '')}》— 作者：{a.get('author', '匿名')}，阅读量：{a.get('view_count', 0)}"
        if a.get("summary"):
            line += f"\n   摘要：{a['summary']}"
        lines.append(line)
    listing = "\n".join(lines) or "（暂无文章）"

    if not llm_client.llm_available():
        return {
            "answer": f"当前热榜共 {len(articles)} 篇文章，AI 服务未配置大模型密钥，暂无法生成趋势分析。",
            "degraded": True,
            "elapsed_ms": int((time.time() - started) * 1000),
        }

    answer = llm_client.chat(
        messages=[
            {"role": "system", "content": "你是一位社区内容分析师，回答简洁专业，突出重点。"},
            {"role": "user", "content": f"当前社区热榜（Top {len(articles)}）：\n{listing}\n\n用户问题：{question}"},
        ],
        temperature=0.5,
        max_tokens=700,
    )
    return {
        "answer": answer.strip(),
        "degraded": False,
        "article_count": len(articles),
        "elapsed_ms": int((time.time() - started) * 1000),
    }
