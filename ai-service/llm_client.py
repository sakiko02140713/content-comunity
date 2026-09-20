"""大模型调用与并行调度基础设施。

本模块刻意只依赖 Python 标准库：
  * HTTP 调用使用 urllib.request（OpenAI 兼容协议），无需 openai SDK；
  * 并行调度使用 concurrent.futures + multiprocessing，无需第三方并发框架；
  * 超时、重试、降级都在此处收敛，业务模块只关心"拿到结果"。
"""

from __future__ import annotations

import json
import logging
import multiprocessing as mp
import os
import random
import re
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any, Callable, Iterable, Sequence

import config

logger = logging.getLogger("ai-service")

# ---------------------------------------------------------------------------
# 进程级工作池
# ---------------------------------------------------------------------------
# 说明：审核属于"IO 密集 + 少量 CPU"的混合负载，
# 线程池负责隐藏大模型网络延迟，进程池负责跨请求并行与 CPU 计算隔离，
# 两者叠加形成"请求间多进程 × 请求内多线程"的两级并行结构。
_PROCESS_POOL: mp.pool.Pool | None = None
_INFLIGHT = None  # threading.Semaphore，延迟创建避免导入即占用资源


def _inflight_semaphore():
    global _INFLIGHT
    if _INFLIGHT is None:
        import threading

        _INFLIGHT = threading.Semaphore(config.MAX_INFLIGHT_REQUESTS)
    return _INFLIGHT


def inflight_slot():
    """请求级并发闸门：限制同时被处理的请求数，防止大模型配额被瞬间打满。"""
    return _inflight_semaphore()


def get_pool() -> mp.pool.Pool | None:
    """获取多进程工作池；WORKER_PROCESSES<=1 时返回 None（退化为纯多线程）。"""
    global _PROCESS_POOL
    if config.WORKER_PROCESSES <= 1:
        return None
    if _PROCESS_POOL is None:
        ctx = mp.get_context("spawn")
        _PROCESS_POOL = ctx.Pool(processes=config.WORKER_PROCESSES)
    return _PROCESS_POOL


def shutdown_pool() -> None:
    global _PROCESS_POOL
    if _PROCESS_POOL is not None:
        _PROCESS_POOL.close()
        _PROCESS_POOL.join()
        _PROCESS_POOL = None


def parallel_map(fn: Callable[[Any], Any], items: Sequence[Any], concurrency: int | None = None) -> list[Any]:
    """并行执行 fn(item)，返回与输入顺序一致的结果列表。

    使用线程池实现：审核的主要耗时在大模型网络往返，
    线程在等待 socket 时会让出 GIL，因此线程池即可获得近似线性的吞吐提升，
    同时避免进程间传递大文本的序列化开销。
    """
    items = list(items)
    if not items:
        return []
    workers = max(1, min(concurrency or config.CONCURRENCY, len(items)))

    if workers == 1:
        return [fn(item) for item in items]

    results: list[Any] = [None] * len(items)
    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="ai-par") as pool:
        futures = {pool.submit(fn, item): idx for idx, item in enumerate(items)}
        for fut in as_completed(futures):
            idx = futures[fut]
            try:
                results[idx] = fut.result()
            except Exception as exc:  # 单个子任务失败不应拖垮整批
                logger.warning("并行子任务失败 index=%s: %s", idx, exc)
                results[idx] = None
    return results


# ---------------------------------------------------------------------------
# 大模型客户端
# ---------------------------------------------------------------------------
class LLMUnavailable(RuntimeError):
    """大模型不可用（未配置密钥或连续重试失败）。"""


def llm_available() -> bool:
    return bool(config.LLM_API_KEY)


def _llm_semaphore():
    """全局并发闸门：限制整个进程内同时在途的大模型调用数。

    分片并行与维度并行叠加时，理论并发数是两者之积，
    若不设全局上限会瞬间打满模型配额并触发限流。
    """
    global _LLM_SEM
    if _LLM_SEM is None:
        import threading

        _LLM_SEM = threading.BoundedSemaphore(config.CONCURRENCY)
    return _LLM_SEM


_LLM_SEM = None


def chat(messages: list[dict[str, str]],
         temperature: float = 0.3,
         max_tokens: int = 800,
         json_mode: bool = False) -> str:
    """调用 OpenAI 兼容的对话补全接口，带指数退避重试。

    参数:
        json_mode: 要求模型返回严格 JSON（DeepSeek/OpenAI 均支持该参数）。
    返回:
        模型回复的纯文本内容。
    抛出:
        LLMUnavailable: 未配置密钥，或重试耗尽仍未成功。
    """
    if not llm_available():
        raise LLMUnavailable("未配置 LLM_API_KEY / DEEPSEEK_API_KEY")

    payload: dict[str, Any] = {
        "model": config.LLM_MODEL,
        "messages": messages,
        "temperature": temperature,
        "max_tokens": max_tokens,
    }
    if json_mode:
        payload["response_format"] = {"type": "json_object"}

    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    url = f"{config.LLM_BASE_URL}/v1/chat/completions"
    last_error: Exception | None = None

    with _llm_semaphore():
        for attempt in range(config.LLM_MAX_RETRIES + 1):
            request = urllib.request.Request(
                url,
                data=body,
                headers={
                    "Content-Type": "application/json",
                    "Authorization": f"Bearer {config.LLM_API_KEY}",
                },
                method="POST",
            )
            try:
                with urllib.request.urlopen(request, timeout=config.LLM_TIMEOUT) as resp:
                    raw = resp.read().decode("utf-8", errors="replace")
                data = json.loads(raw)
                choices = data.get("choices") or []
                if not choices:
                    raise LLMUnavailable(f"模型返回为空: {raw[:200]}")
                return (choices[0].get("message") or {}).get("content") or ""
            except Exception as exc:  # noqa: BLE001 - 统一降级处理
                last_error = exc
                if attempt < config.LLM_MAX_RETRIES:
                    # 指数退避 + 抖动，避免并发重试形成脉冲。
                    delay = (0.6 * (2 ** attempt)) + random.uniform(0, 0.3)
                    logger.warning("大模型调用失败(第 %d 次)，%.2fs 后重试: %s", attempt + 1, delay, exc)
                    time.sleep(delay)

    raise LLMUnavailable(f"大模型调用失败: {last_error}")


def extract_json(text: str) -> dict[str, Any]:
    """从模型回复中稳健地提取 JSON 对象。

    模型有时会把 JSON 包在 ```json 代码块里或附加解释文字，
    这里先尝试直接解析，再退化为正则提取第一个 {...} 片段。
    """
    text = (text or "").strip()
    if not text:
        raise ValueError("模型返回为空")

    if text.startswith("```"):
        text = re.sub(r"^```[a-zA-Z]*\s*", "", text)
        text = re.sub(r"\s*```$", "", text).strip()

    try:
        return json.loads(text)
    except json.JSONDecodeError:
        pass

    match = re.search(r"\{.*\}", text, re.DOTALL)
    if not match:
        raise ValueError(f"模型返回中未找到 JSON: {text[:200]}")
    return json.loads(match.group(0))


# ---------------------------------------------------------------------------
# 文本分片
# ---------------------------------------------------------------------------
_SENTENCE_ENDS = "。！？；\n.!?;"


def split_chunks(text: str,
                 size: int | None = None,
                 overlap: int | None = None) -> list[str]:
    """把长文本切分为带重叠的分片，尽量落在句子边界上。

    重叠的作用：如果违规内容恰好横跨两个分片的接缝处，
    没有重叠就可能被两个分片各看到一半而漏判。
    """
    text = (text or "").strip()
    if not text:
        return []

    size = size or config.CHUNK_SIZE
    overlap = overlap if overlap is not None else config.CHUNK_OVERLAP
    if len(text) <= size:
        return [text]

    chunks: list[str] = []
    start = 0
    total = len(text)
    while start < total:
        end = min(start + size, total)
        if end < total:
            # 从 end 向前寻找最近的句子边界。
            window_start = start + size // 2
            for pos in range(end, window_start, -1):
                if text[pos - 1] in _SENTENCE_ENDS:
                    end = pos
                    break
        chunk = text[start:end].strip()
        if chunk:
            chunks.append(chunk)
        if end >= total:
            break
        start = max(0, end - overlap)
        if start <= 0 and chunks:
            break
    return chunks


def parallel_batches(items: Sequence[Any], batch_size: int) -> list[list[Any]]:
    """把任务切成批次，便于前端展示"批次级"的并行进度。"""
    if batch_size <= 0:
        return [list(items)]
    return [list(items[i:i + batch_size]) for i in range(0, len(items), batch_size)]


def pool_worker_count() -> int:
    pool = get_pool()
    return config.WORKER_PROCESSES if pool is not None else 0


def describe_runtime() -> dict[str, Any]:
    """运行时并行能力描述，供 /health 与前端展示。"""
    return {
        "llm_configured": llm_available(),
        "llm_model": config.LLM_MODEL if llm_available() else None,
        "llm_base_url": config.LLM_BASE_URL,
        "concurrency": config.CONCURRENCY,
        "worker_processes": config.WORKER_PROCESSES,
        "max_inflight_requests": config.MAX_INFLIGHT_REQUESTS,
        "chunk_size": config.CHUNK_SIZE,
        "chunk_threshold": config.CHUNK_THRESHOLD,
        "parallelism_mode": (
            f"多进程({config.WORKER_PROCESSES}) × 多线程({config.CONCURRENCY})"
            if config.WORKER_PROCESSES > 1
            else f"单进程 × 多线程({config.CONCURRENCY})"
        ),
    }
