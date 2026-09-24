"""面向 UGC 社区的智能内容审核 AI 服务（Python 并行计算侧）。

设计要点
--------
1. **零第三方依赖**：仅使用 Python 标准库（http.server / urllib / multiprocessing /
   concurrent.futures），clone 下来即可 `python main.py` 运行，Docker 镜像也不需要 pip 安装。
2. **两级并行**：
      请求间 —— ThreadingHTTPServer 多线程 + 多进程工作池（AI_WORKERS）
      请求内 —— 分片并行 × 审核维度并行（AI_CONCURRENCY）
   两者叠加显著提升长文审核与批量总结的吞吐。
3. **优雅降级**：未配置大模型密钥或调用失败时，自动退化为规则/词库审核与截断摘要，
   接口始终返回 200 与 `degraded` 标记，保证上游 Go 服务不会因 AI 故障而阻塞。

启动：
    python main.py                # 默认监听 0.0.0.0:8000
    AI_WORKERS=4 python main.py   # 开启 4 个审核工作进程
"""

from __future__ import annotations

import json
import logging
import os
import signal
import sys
import threading
import time
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable
from urllib.parse import parse_qs, urlparse

import audit
import config
import generate
import llm_client

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
)
logger = logging.getLogger("ai-service")

SERVICE_NAME = "content-community-ai"
SERVICE_VERSION = "2.0.0"
STARTED_AT = time.time()

# 路由表：路径 -> (处理函数, 是否需要请求体)
ROUTES: dict[str, tuple[Callable[[dict[str, Any], dict[str, list[str]]], Any], bool]] = {}


def route(path: str, need_body: bool = True):
    def decorator(fn):
        ROUTES[path] = (fn, need_body)
        return fn
    return decorator


# ===========================================================================
# 健康检查与元信息
# ===========================================================================
@route("/health", need_body=False)
def handle_health(body: dict[str, Any], query: dict[str, list[str]]) -> dict[str, Any]:
    return {
        "status": "ok",
        "service": SERVICE_NAME,
        "version": SERVICE_VERSION,
        "uptime_seconds": round(time.time() - STARTED_AT, 1),
        "runtime": llm_client.describe_runtime(),
    }


@route("/api/ai/dimensions", need_body=False)
def handle_dimensions(body: dict[str, Any], query: dict[str, list[str]]) -> dict[str, Any]:
    return {
        "dimensions": config.AUDIT_DIMENSIONS,
        "reject_score": config.REJECT_SCORE,
        "review_score": config.REVIEW_SCORE,
        "runtime": llm_client.describe_runtime(),
    }


# ===========================================================================
# AI 辅助创作（润色）
#
# 说明：本服务不提供"凭空生成文章"的能力，只对用户已经写好的内容做语言润色。
# content 为空一律拒绝，从接口层面保证"必须先有用户自己的内容"。
# ===========================================================================
@route("/api/ai/polish")
def handle_polish(body: dict[str, Any], query: dict[str, list[str]]) -> dict[str, Any]:
    title = str(body.get("title") or "").strip()
    content = str(body.get("content") or "")
    if not content.strip():
        raise ValueError("请先输入正文内容，AI 才能进行润色")
    style = str(body.get("style") or "")
    return generate.polish_article(title, content, style)


# 兼容旧路径：历史上 /api/ai/generate 用于生成文章，
# 现在统一转为润色，且同样强制要求提供 content。
@route("/api/ai/generate")
def handle_generate_legacy(body: dict[str, Any], query: dict[str, list[str]]) -> dict[str, Any]:
    title = str(body.get("title") or "").strip()
    content = str(body.get("content") or body.get("outline") or "")
    if not content.strip():
        raise ValueError("请先输入正文内容，AI 才能进行润色")
    style = str(body.get("style") or "")
    return generate.polish_article(title, content, style)


@route("/api/ai/summary")
def handle_summary(body: dict[str, Any], query: dict[str, list[str]]) -> dict[str, Any]:
    articles = body.get("articles") or []
    if not isinstance(articles, list):
        raise ValueError("articles 必须是数组")
    question = str(body.get("question") or "")
    return generate.summarize_hotlist(articles, question)


@route("/api/ai/article-summary")
def handle_article_summary(body: dict[str, Any], query: dict[str, list[str]]) -> dict[str, Any]:
    title = str(body.get("title") or "")
    content = str(body.get("content") or "")
    if not title and not content:
        raise ValueError("title 与 content 不能同时为空")
    concurrency = _int_param(body, "concurrency", config.CONCURRENCY)
    return generate.article_summary(title, content, concurrency=concurrency)


# ===========================================================================
# 内容审核
# ===========================================================================
@route("/api/ai/audit")
def handle_audit(body: dict[str, Any], query: dict[str, list[str]]) -> dict[str, Any]:
    """完整并行审核：分片并行 × 维度并行。"""
    title = str(body.get("title") or "")
    content = str(body.get("content") or body.get("chunk") or "")
    if not content.strip():
        raise ValueError("content 不能为空")
    concurrency = _int_param(body, "concurrency", config.CONCURRENCY)
    force_chunk = bool(body.get("force_chunk", False))
    return audit.audit_text(title, content, concurrency=concurrency, force_chunk=force_chunk)


@route("/api/ai/audit-chunk")
def handle_audit_chunk(body: dict[str, Any], query: dict[str, list[str]]) -> dict[str, Any]:
    """单分片审核，支持指定维度（供 Go 侧按维度并行分发）。"""
    chunk = str(body.get("chunk") or body.get("content") or "")
    if not chunk.strip():
        raise ValueError("chunk 不能为空")
    title = str(body.get("title") or "")
    index = _int_param(body, "index", 0)
    total = _int_param(body, "total", 1)
    dimension = body.get("dimension") or None
    if dimension is not None:
        dimension = str(dimension)
    concurrency = _int_param(body, "concurrency", config.CONCURRENCY)

    result = audit.audit_chunk(chunk, title, index, total, dimension, concurrency)
    # 统一成 Go 侧 ChunkAuditResult 的结构：status / score / reasons / hits / degraded。
    return {
        "index": result["index"],
        "status": result["status"],
        "score": result["score"],
        "reasons": result["reasons"],
        "hits": result["hits"],
        "degraded": result["degraded"],
        "elapsed_ms": result["elapsed_ms"],
        "dimension_results": result.get("dimension_results", []),
    }


@route("/api/ai/audit-batch")
def handle_audit_batch(body: dict[str, Any], query: dict[str, list[str]]) -> dict[str, Any]:
    """批量并行审核。"""
    items = body.get("items") or []
    if not isinstance(items, list) or not items:
        raise ValueError("items 必须是非空数组")
    concurrency = _int_param(body, "concurrency", config.CONCURRENCY)
    return audit.audit_many(items, concurrency=concurrency)


@route("/api/ai/text-analyze")
def handle_text_analyze(body: dict[str, Any], query: dict[str, list[str]]) -> dict[str, Any]:
    """词库快速分析（纯 CPU，毫秒级）。"""
    text = str(body.get("text") or body.get("content") or "")
    if not text:
        raise ValueError("text 不能为空")
    return audit.analyze_words(text)


def _int_param(body: dict[str, Any], key: str, default: int) -> int:
    try:
        value = int(body.get(key, default))
    except (TypeError, ValueError):
        return default
    return max(1, min(value, 32))


# ===========================================================================
# HTTP 层
# ===========================================================================
class AIRequestHandler(BaseHTTPRequestHandler):
    server_version = f"{SERVICE_NAME}/{SERVICE_VERSION}"
    protocol_version = "HTTP/1.1"

    # ---------- 基础设施 ----------
    def log_message(self, fmt: str, *args: Any) -> None:  # noqa: A003
        logger.info("%s - %s", self.address_string(), fmt % args)

    def _write_json(self, payload: Any, status: int = 200) -> None:
        data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()
        self.wfile.write(data)

    def _read_body(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0:
            return {}
        raw = self.rfile.read(length)
        if not raw.strip():
            return {}
        try:
            parsed = json.loads(raw.decode("utf-8", errors="replace"))
        except json.JSONDecodeError as exc:
            raise ValueError(f"请求体不是合法 JSON: {exc}") from exc
        if not isinstance(parsed, dict):
            raise ValueError("请求体必须是 JSON 对象")
        return parsed

    # ---------- 路由分发 ----------
    def do_OPTIONS(self) -> None:  # noqa: N802
        self.send_response(204)
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "Content-Type, Authorization")
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self) -> None:  # noqa: N802
        self._dispatch("GET")

    def do_POST(self) -> None:  # noqa: N802
        self._dispatch("POST")

    def _dispatch(self, method: str) -> None:
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        query = parse_qs(parsed.query)

        handler_entry = ROUTES.get(path)
        if handler_entry is None:
            self._write_json({"detail": f"未知接口: {path}"}, status=404)
            return

        fn, need_body = handler_entry
        started = time.time()
        slot = llm_client.inflight_slot()
        acquired = slot.acquire(timeout=60)
        if not acquired:
            self._write_json(
                {"detail": "服务繁忙，请稍后重试（在途请求已达上限）"},
                status=503,
            )
            return

        try:
            body = self._read_body() if need_body else {}
            result = fn(body, query)
            if isinstance(result, dict):
                result.setdefault("service_elapsed_ms", int((time.time() - started) * 1000))
            self._write_json(result)
        except ValueError as exc:
            self._write_json({"detail": str(exc)}, status=400)
        except llm_client.LLMUnavailable as exc:
            # 大模型不可用属于上游依赖问题，返回 503 让 Go 侧走降级逻辑。
            self._write_json({"detail": f"大模型不可用: {exc}"}, status=503)
        except BrokenPipeError:  # 客户端已断开，忽略
            logger.warning("客户端提前断开连接: %s", path)
        except Exception:  # noqa: BLE001 - 兜底，避免单请求异常拖垮服务
            logger.error("接口 %s 处理失败:\n%s", path, traceback.format_exc())
            self._write_json({"detail": "AI 服务内部错误"}, status=500)
        finally:
            slot.release()


def _install_signal_handlers(server: ThreadingHTTPServer) -> None:
    def _shutdown(signum, _frame):
        logger.info("收到信号 %s，正在关闭服务…", signum)
        threading.Thread(target=server.shutdown, daemon=True).start()

    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            signal.signal(sig, _shutdown)
        except (ValueError, OSError):  # 非主线程或不支持的平台
            pass


def main() -> int:
    host = os.getenv("AI_HOST", "0.0.0.0")
    port = int(os.getenv("AI_PORT", "8000"))

    runtime = llm_client.describe_runtime()
    logger.info("=" * 68)
    logger.info("内容社区 AI 服务启动中（并行审核 / 并行总结）")
    logger.info("监听地址      : http://%s:%d", host, port)
    logger.info("并行模式      : %s", runtime["parallelism_mode"])
    logger.info("请求内并发度  : %d", runtime["concurrency"])
    logger.info("分片大小/阈值 : %d 字 / %d 字", runtime["chunk_size"], runtime["chunk_threshold"])
    logger.info("审核维度      : %s", "、".join(d["name"] for d in config.AUDIT_DIMENSIONS))
    if runtime["llm_configured"]:
        logger.info("大模型        : %s @ %s", runtime["llm_model"], runtime["llm_base_url"])
    else:
        logger.warning("未检测到 DEEPSEEK_API_KEY，审核将降级为本地词库规则模式")
    logger.info("=" * 68)

    server = ThreadingHTTPServer((host, port), AIRequestHandler)
    server.daemon_threads = True
    _install_signal_handlers(server)

    try:
        server.serve_forever(poll_interval=0.5)
    finally:
        llm_client.shutdown_pool()
        server.server_close()
        logger.info("AI 服务已停止")
    return 0


if __name__ == "__main__":
    sys.exit(main())
