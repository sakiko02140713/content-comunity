"""完整业务链路端到端验证：注册 → 登录 → 创作 → 并行审核发布 → 广场/热榜 → AI 辅助 → 批量审核 → 人工复核。"""

import json
import time
import urllib.error
import urllib.request

BASE = "http://127.0.0.1:8080/api"
TOKEN = None
PASS, FAIL = [], []
# 用户名带微秒级后缀，保证每轮运行都是全新账号，不受上一轮残留数据影响。
RUN_TAG = str(int(time.time() * 1000) % 100000000)


def call(method, path, payload=None, token=None, timeout=180, raw=False):
    url = BASE + path
    data = json.dumps(payload, ensure_ascii=False).encode("utf-8") if payload is not None else None
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            body = r.read().decode("utf-8", errors="replace")
            return r.status, (body if raw else json.loads(body) if body else {})
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", errors="replace")
        try:
            return e.code, json.loads(body)
        except Exception:
            return e.code, body


def check(name, cond, detail=""):
    (PASS if cond else FAIL).append(name)
    print(f"  [{'PASS' if cond else 'FAIL'}] {name}" + (f"  → {detail}" if detail else ""))


print("=" * 78)
print("步骤 0  后端能力自检（确认运行的是新代码）")
print("=" * 78)
st, info = call("GET", "/ai/audit/info")
check("GET /api/ai/audit/info 可用", st == 200, f"HTTP {st}")
if st == 200:
    print(f"       引擎: {info.get('engine')}")
    print(f"       规则 {info.get('rule_count')} 条 / 正则 {info.get('rule_patterns')} 条 / "
          f"维度 {len(info.get('dimensions', []))} 个 / 并行度上限 {info.get('max_parallel')}")

print()
print("=" * 78)
print("步骤 1  用户注册与登录")
print("=" * 78)
user = f"tester{RUN_TAG}"
st, r = call("POST", "/register", {"username": user, "password": "test123456", "nickname": "演示用户"})
check("注册普通用户", st == 201, f"HTTP {st} {r if st != 201 else ''}")

st, r = call("POST", "/login", {"username": user, "password": "test123456"})
check("登录并签发 JWT", st == 200 and "token" in r, f"HTTP {st}")
USER_TOKEN = r.get("token") if st == 200 else None

st, r = call("POST", "/login", {"username": "admin", "password": "admin123456"})
check("审核员账号自动初始化并可登录", st == 200 and r.get("user", {}).get("role") == "admin",
      f"HTTP {st} role={r.get('user', {}).get('role') if st == 200 else '-'}")
ADMIN_TOKEN = r.get("token") if st == 200 else None

st, r = call("POST", "/login", {"username": user, "password": "wrong-password"})
check("错误密码被拒绝", st == 401, f"HTTP {st}")

st, r = call("POST", "/login", {"username": user, "password": "test123456"})
USER_TOKEN = r.get("token") if st == 200 else USER_TOKEN

st, r = call("GET", "/me", token=USER_TOKEN)
check("GET /api/me 返回资料与统计", st == 200 and r.get("user", {}).get("username") == user, f"HTTP {st}")

print()
print("=" * 78)
print("步骤 2  创作：创建草稿")
print("=" * 78)
CLEAN = ("Go 的并发模型解析",
         "Go 通过 goroutine 与 channel 提供轻量级并发原语。goroutine 由运行时调度，初始栈很小；"
         "channel 提供类型安全的通信机制。实践中常用 worker pool 控制并发度，用 context 控制生命周期，"
         "并对热点数据进行分片并行处理以提升吞吐。")
AD = ("低价货源内部渠道",
      "最近拿到一批超低价货源，需要的朋友加微信 abc12345 详聊，兼职日结，扫码进群还能免费领取试用装，机不可失！")
REVIEW = ("关于社区运营的一些不成熟的想法",
          "社区里有些人就是脑残，管理水平也差，纯粹是废物在瞎指挥，看着就来气。")

ids = {}
for key, (title, content) in (("clean", CLEAN), ("ad", AD), ("review", REVIEW)):
    st, r = call("POST", "/articles", {"title": title, "content": content, "tags": "Go,并发,AI"}, token=USER_TOKEN)
    ids[key] = r.get("id") if st == 201 else None
    check(f"创建草稿 [{key}]", st == 201 and ids[key], f"HTTP {st} id={ids[key]} status={r.get('status') if st == 201 else '-'}")

st, r = call("GET", "/my/articles?status=draft", token=USER_TOKEN)
check("我的文章可查询草稿", st == 200 and r.get("total", 0) >= 3, f"草稿数={r.get('total') if st == 200 else '-'}")

print()
print("=" * 78)
print("步骤 3  正常内容：AI 审核 → 自动发布")
print("=" * 78)
t0 = time.time()
st, r = call("POST", f"/articles/{ids['clean']}/publish", {}, token=USER_TOKEN, timeout=240)
wall = int((time.time() - t0) * 1000)
audit = r.get("audit", {}) if st == 200 else {}
check("发布接口返回审核结果", st == 200 and audit, f"HTTP {st} {r if st != 200 else ''}")
if audit:
    print(f"       判定={audit.get('verdict')} 风险分={audit.get('risk_score')}")
    print(f"       实际耗时={audit.get('elapsed_ms')}ms 串行等价={audit.get('sequential_ms')}ms "
          f"加速比={audit.get('speedup')}x 峰值并行度={audit.get('peak_concurrency')} 墙钟={wall}ms")
    print(f"       原因: {audit.get('reason')}")
    check("正常内容审核通过 (pass)", audit.get("verdict") == "pass", audit.get("reason", ""))
    check("加速比 > 1（确认真并行）", float(audit.get("speedup", 0)) > 1.0, f"speedup={audit.get('speedup')}")
    check("峰值并行度 > 1", int(audit.get("peak_concurrency", 0)) > 1, f"peak={audit.get('peak_concurrency')}")
    check("文章状态变为 published", r.get("article", {}).get("status") == "published",
          f"status={r.get('article', {}).get('status')}")
    check("审核时间线包含多维度并行阶段",
          any(s.get("key") in ("dimensions", "chunks") and len(s.get("tasks", [])) >= 5
              for s in audit.get("stages", [])),
          f"阶段={[s.get('key') for s in audit.get('stages', [])]}")
    check("返回 5 个审核维度结论", len(audit.get("dimensions", [])) == 5,
          f"维度数={len(audit.get('dimensions', []))}")

print()
print("=" * 78)
print("步骤 4  违规内容：AI 审核 → 拒绝发布")
print("=" * 78)
st, r = call("POST", f"/articles/{ids['ad']}/publish", {}, token=USER_TOKEN, timeout=240)
audit_ad = r.get("audit", {}) if st == 200 else {}
check("违规内容发布接口返回结果", st == 200 and audit_ad, f"HTTP {st}")
if audit_ad:
    print(f"       判定={audit_ad.get('verdict')} 风险分={audit_ad.get('risk_score')}")
    print(f"       原因: {audit_ad.get('reason')}")
    for h in audit_ad.get("hits", []):
        print(f"       规则命中: {h.get('rule_name')} 词={h.get('keywords')} 证据={h.get('evidence') or h.get('evidences')}")
    check("违规内容被拒绝 (reject)", audit_ad.get("verdict") == "reject", audit_ad.get("reason", ""))
    check("规则引擎给出命中证据", len(audit_ad.get("hits", [])) > 0,
          f"命中 {len(audit_ad.get('hits', []))} 条规则")
    check("文章状态变为 rejected", r.get("article", {}).get("status") == "rejected",
          f"status={r.get('article', {}).get('status')}")

print()
print("=" * 78)
print("步骤 5  疑似内容：转人工复核，且对外不可见")
print("=" * 78)
st, r = call("POST", f"/articles/{ids['review']}/publish", {}, token=USER_TOKEN, timeout=240)
audit_rv = r.get("audit", {}) if st == 200 else {}
status_rv = r.get("article", {}).get("status") if st == 200 else None
check("疑似内容发布接口返回结果", st == 200 and audit_rv, f"HTTP {st}")
if audit_rv:
    print(f"       判定={audit_rv.get('verdict')} 风险分={audit_rv.get('risk_score')} 文章状态={status_rv}")
    print(f"       原因: {audit_rv.get('reason')}")
    check("疑似内容判定为 review 或 reject", audit_rv.get("verdict") in ("review", "reject"),
          f"verdict={audit_rv.get('verdict')}")
    check("未通过审核的文章不会 published", status_rv != "published", f"status={status_rv}")

print()
print("=" * 78)
print("步骤 6  文章广场与热榜（并行聚合）")
print("=" * 78)
st, r = call("GET", "/articles?page=1&size=10&status=published")
check("文章列表可查询", st == 200, f"HTTP {st}")
if st == 200:
    print(f"       已发布文章总数={r.get('total')}")
    check("列表仅返回已发布内容",
          all(i.get("status") == "published" for i in r.get("items", [])),
          f"共 {len(r.get('items', []))} 条")

st, r = call("GET", "/articles/hot?limit=10")
check("热榜接口可用", st == 200 and "hot" in r, f"HTTP {st}")
if st == 200:
    print(f"       热榜 {len(r.get('hot', []))} 条 · 实际耗时={r.get('elapsed_ms')}ms "
          f"串行等价={r.get('sequential_ms')}ms 加速比={r.get('speedup')}x")
    print(f"       并行子任务: {[(t.get('name'), str(t.get('duration')) + 'ms') for t in r.get('tasks', [])]}")
    print(f"       标签热度: {[(t.get('tag'), t.get('count')) for t in (r.get('tags') or [])][:5]}")
    check("热点聚合并行执行 3 个子任务", len(r.get("tasks", [])) == 3, f"任务数={len(r.get('tasks', []))}")
    check("热榜只含已发布文章", all(a.get("status") == "published" for a in r.get("hot", [])),
          f"共 {len(r.get('hot', []))} 条")

st, r = call("GET", f"/articles/{ids['clean']}", token=USER_TOKEN)
check("文章详情可访问", st == 200 and r.get("id") == ids["clean"], f"HTTP {st}")
if st == 200:
    print(f"       阅读量={r.get('view_count')} 审核状态={r.get('audit_status')} 风险分={r.get('audit_score')}")

st, r = call("GET", f"/articles/{ids['ad']}", token=USER_TOKEN)
check("作者本人可查看未通过审核的文章", st == 200, f"HTTP {st}")
st, r = call("GET", f"/articles/{ids['ad']}")
check("未登录用户无法访问未发布文章", st == 403, f"HTTP {st}")

print()
print("=" * 78)
print("步骤 7  AI 辅助：文章生成 与 并行总结")
print("=" * 78)
st, r = call("POST", "/ai/generate", {"title": "如何设计一个高并发的审核系统", "outline": "从任务拆分、协程池、结果聚合三方面展开"}, token=USER_TOKEN, timeout=240)
check("AI 辅助生成文章", st == 200 and len(r.get("generated_content", "")) > 50,
      f"HTTP {st} 长度={len(r.get('generated_content', '')) if st == 200 else '-'}")
if st == 200:
    print(f"       生成 {len(r['generated_content'])} 字，开头: {r['generated_content'][:60].replace(chr(10), ' ')}…")

st, r = call("POST", f"/ai/articles/{ids['clean']}/summary", {}, token=USER_TOKEN, timeout=240)
check("单篇长文 Map-Reduce 并行总结", st == 200 and r.get("summary"), f"HTTP {st}")
if st == 200:
    print(f"       模式={r.get('mode')} 分片={r.get('chunks')} 耗时={r.get('elapsed_ms')}ms")
    print(f"       摘要: {str(r.get('summary'))[:80]}…")

st, r = call("POST", "/ai/summary", {"question": "请总结当前热榜的内容趋势", "limit": 6}, token=USER_TOKEN, timeout=300)
check("热榜并行总结", st == 200 and r.get("answer"), f"HTTP {st}")
if st == 200:
    print(f"       策略={r.get('strategy')} 文章数={r.get('article_count')} 实际={r.get('elapsed_ms')}ms "
          f"串行等价={r.get('sequential_ms')}ms 加速比={r.get('speedup')}x")
    print(f"       结论: {str(r.get('answer'))[:100].replace(chr(10), ' ')}…")

print()
print("=" * 78)
print("步骤 8  批量并行审核")
print("=" * 78)
for i in range(3):
    call("POST", "/articles", {"title": f"批量测试草稿 {i + 1}",
                               "content": f"这是第 {i + 1} 篇用于批量并行审核测试的正常内容，讨论并发编程实践。",
                               "tags": "测试"}, token=USER_TOKEN)
st, r = call("POST", "/ai/audit/batch", {"status": "draft", "limit": 5, "concurrency": 4}, token=USER_TOKEN, timeout=300)
check("批量并行审核接口可用", st == 200 and r.get("items") is not None, f"HTTP {st}")
if st == 200:
    print(f"       处理 {r.get('total')} 篇 · 实际耗时={r.get('elapsed_ms')}ms 串行等价={r.get('sequential_ms')}ms "
          f"加速比={r.get('speedup')}x")
    print(f"       策略: {r.get('strategy')}")
    for it in r.get("items", []):
        print(f"         - {str(it.get('title'))[:26]:28s} {it.get('verdict')}  风险分={it.get('risk_score')}  {it.get('elapsed_ms')}ms")
    check("批量审核确实并行（加速比 > 1）", float(r.get("speedup", 0)) > 1.0, f"speedup={r.get('speedup')}")

print()
print("=" * 78)
print("步骤 9  人工复核（审核员）")
print("=" * 78)
st, r = call("GET", "/articles?status=review&page=1&size=10", token=ADMIN_TOKEN)
queue = r.get("items", []) if st == 200 else []
check("待复核队列可查询（审核员）", st == 200, f"HTTP {st}")
print(f"       待复核 {len(queue)} 篇")

st, r = call("GET", "/articles?status=review&page=1&size=10")
check("未登录不能查询待复核队列", st == 401, f"HTTP {st}")

st, r = call("POST", f"/admin/articles/{ids['review']}/review", {"decision": "approve", "reason": "人工复核通过"}, token=USER_TOKEN)
check("普通用户无权人工复核", st == 403, f"HTTP {st}")

st, r = call("POST", f"/admin/articles/{ids['review']}/review", {"decision": "approve", "reason": "人工复核确认无违规"}, token=ADMIN_TOKEN)
check("审核员可通过复核", st == 200, f"HTTP {st} {r.get('error', '') if st != 200 else ''}")
if st == 200:
    print(f"       复核后状态={r.get('article', {}).get('status')} 审核依据={r.get('article', {}).get('audit_reason')}")
    check("复核通过后文章变为 published", r.get("article", {}).get("status") == "published",
          f"status={r.get('article', {}).get('status')}")

print()
print("=" * 78)
print("步骤 10  鉴权与越权防护")
print("=" * 78)
st, r = call("GET", "/my/articles")
check("无 Token 访问受保护接口被拒", st == 401, f"HTTP {st}")
st, r = call("GET", "/my/articles", token="invalid-token-xxx")
check("无效 Token 被拒", st == 401, f"HTTP {st}")

other = f"other{RUN_TAG}"
call("POST", "/register", {"username": other, "password": "test123456", "nickname": "他人"})
st, r = call("POST", "/login", {"username": other, "password": "test123456"})
OTHER_TOKEN = r.get("token") if st == 200 else None
st, r = call("DELETE", f"/articles/{ids['clean']}", token=OTHER_TOKEN)
check("他人无法删除我的文章", st == 403, f"HTTP {st}")
st, r = call("PUT", f"/articles/{ids['clean']}", {"title": "被篡改的标题", "content": "x"}, token=OTHER_TOKEN)
check("他人无法编辑我的文章", st == 403, f"HTTP {st}")

# 未发布内容的可见性（防止越权读取他人草稿）
st, r = call("POST", "/articles", {"title": "他人的私有草稿", "content": "只有作者能看到的草稿内容。", "tags": "私有"}, token=OTHER_TOKEN)
other_draft_id = r.get("id") if st == 201 else None
check("他人创建自己的草稿", st == 201, f"HTTP {st} id={other_draft_id}")

st, r = call("GET", "/articles?status=draft&page=1&size=50", token=USER_TOKEN)
items = r.get("items", []) if st == 200 else []
mine_only = all(i.get("author_id") == r.get("author_id") for i in items) if items else True
check("普通用户查询草稿只能看到自己的", st == 200 and mine_only,
      f"HTTP {st} 返回 {len(items)} 条，越权可见={not mine_only}")

st, r = call("GET", "/articles?status=draft&page=1&size=50")
check("未登录无法查询草稿（防止泄露未发布内容）", st == 401, f"HTTP {st}")

st, r = call("GET", "/articles?status=draft&page=1&size=50", token=ADMIN_TOKEN)
admin_items = r.get("items", []) if st == 200 else []
admin_ids = {i.get("id") for i in admin_items}
check("审核员可查询全部草稿（人工复核需要）",
      st == 200 and len(admin_items) >= len(items) and other_draft_id in admin_ids,
      f"HTTP {st} 审核员可见 {len(admin_items)} 条 / 普通用户可见 {len(items)} 条 / "
      f"能看到他人的草稿={other_draft_id in admin_ids}")

st, r = call("POST", "/logout", {}, token=OTHER_TOKEN)
check("注销接口可用", st == 200, f"HTTP {st}")
st, r = call("GET", "/my/articles", token=OTHER_TOKEN)
check("注销后 Token 立即失效", st == 401, f"HTTP {st}")

print()
print("=" * 78)
print("步骤 11  编辑后重新审核 + 审核记录")
print("=" * 78)
st, r = call("PUT", f"/articles/{ids['clean']}", {
    "title": "Go 并发模型解析（修订版）",
    "content": CLEAN[1] + "本次修订补充了并行审核流水线的设计要点。",
    "tags": "Go,并发,AI,审核"},
    token=USER_TOKEN)
check("作者可编辑已发布文章", st == 200, f"HTTP {st}")
if st == 200:
    print(f"       编辑后状态={r.get('status')}（应回到 draft 等待重新审核）")
    check("编辑后回到 draft 状态", r.get("status") == "draft", f"status={r.get('status')}")

st, r = call("POST", f"/articles/{ids['clean']}/publish", {}, token=USER_TOKEN, timeout=240)
check("编辑后可重新审核发布", st == 200 and r.get("article", {}).get("status") == "published",
      f"HTTP {st} status={r.get('article', {}).get('status') if st == 200 else '-'}")

st, r = call("GET", f"/audit/records?article_id={ids['clean']}&limit=10", token=USER_TOKEN)
check("审核记录可查询", st == 200 and len(r.get("items", [])) >= 1, f"HTTP {st} 记录数={len(r.get('items', [])) if st == 200 else '-'}")
if st == 200 and r.get("items"):
    for rec in r["items"][:3]:
        print(f"       记录#{rec.get('id')} {rec.get('verdict')} 风险分={rec.get('risk_score')} "
              f"实际={rec.get('elapsed_ms')}ms 串行等价={rec.get('sequential_ms')}ms "
              f"加速比={rec.get('speedup')}x 并行度={rec.get('max_concurrency')}")

print()
print("=" * 78)
print("步骤 12  并行收益统计")
print("=" * 78)
st, r = call("GET", "/ai/stats", token=USER_TOKEN)
check("并行统计接口可用", st == 200 and "audit" in r, f"HTTP {st}")
if st == 200:
    a = r["audit"]
    print(f"       累计审核={a.get('total_audits')} 次")
    print(f"       平均实际耗时={a.get('avg_elapsed_ms')}ms 平均串行等价={a.get('avg_sequential_ms')}ms "
          f"平均加速比={a.get('avg_speedup')}x 最高={a.get('max_speedup')}x")
    print(f"       通过率={a.get('pass_rate')}% 拒绝率={a.get('reject_rate')}% 峰值并行度={a.get('peak_concurrency')}")
    check("统计显示并行收益", float(a.get("avg_speedup", 0)) > 1.0, f"avg_speedup={a.get('avg_speedup')}")

print()
print("=" * 78)
print("步骤 13  论坛：版块、回复、点赞")
print("=" * 78)
st, r = call("GET", "/boards")
boards = r.get("items", []) if st == 200 else []
check("版块列表可用（内置固定版块）", st == 200 and len(boards) >= 5, f"HTTP {st} 版块数={len(boards)}")
if boards:
    print("       " + " | ".join(f"{b['name']}({b['thread_count']}帖/{b['reply_count']}回复)" for b in boards))

st, r = call("GET", "/forum/overview")
check("论坛首页概览可用", st == 200 and "stats" in r and "boards" in r, f"HTTP {st}")
if st == 200:
    print(f"       站点统计={r['stats']}")
    print(f"       并行子任务={len(r.get('tasks', []))} 实际耗时={r.get('elapsed_ms')}ms 加速比={r.get('speedup')}x")
    check("概览并发执行 4 个子任务", len(r.get("tasks", [])) == 4, f"任务数={len(r.get('tasks', []))}")

# 指定版块发帖
st, r = call("POST", "/articles", {"title": "论坛联调：版块归属测试",
                                   "content": "这是一篇用于验证版块归属与回复功能的帖子，内容为正常的技术讨论。",
                                   "tags": "联调", "board": "tech"}, token=USER_TOKEN)
thread_id = r.get("id") if st == 201 else None
check("发帖时指定版块", st == 201 and r.get("board_id"), f"HTTP {st} board_id={r.get('board_id') if st == 201 else '-'}")

st, r = call("POST", f"/articles/{thread_id}/publish", {}, token=USER_TOKEN, timeout=240)
check("帖子审核通过并发布", st == 200 and r.get("article", {}).get("status") == "published",
      f"HTTP {st} verdict={r.get('audit', {}).get('verdict') if st == 200 else '-'}")
if st == 200:
    a = r["audit"]
    print(f"       审核判定={a['verdict']} 风险分={a['risk_score']} 实际={a['elapsed_ms']}ms "
          f"加速比={a['speedup']}x")

st, r = call("GET", "/articles?board=tech&status=published&size=50")
check("按版块过滤帖子", st == 200 and any(i["id"] == thread_id for i in r.get("items", [])),
      f"HTTP {st} 版块内帖子数={r.get('total') if st == 200 else '-'}")

# 回复（同样经过 AI 审核）
t0 = time.time()
st, r = call("POST", f"/articles/{thread_id}/replies", {"content": "写得不错，这个问题我也遇到过，感谢分享经验。"},
             token=USER_TOKEN, timeout=240)
wall = int((time.time() - t0) * 1000)
check("发表正常回复", st == 201 and r.get("reply"), f"HTTP {st}")
if r.get("audit"):
    a = r["audit"]
    print(f"       回复审核：{a['verdict']} 风险分={a['risk_score']} 实际={a['elapsed_ms']}ms "
          f"串行等价={a['sequential_ms']}ms 加速比={a['speedup']}x 并行度={a['peak_concurrency']} 墙钟={wall}ms")
    check("回复审核也是并行的", float(a.get("speedup", 0)) > 1.0, f"speedup={a.get('speedup')}")

reply_id = r.get("reply", {}).get("id") if st == 201 else None

st, r = call("POST", f"/articles/{thread_id}/replies", {"content": "加微信 abc12345 兼职日结，扫码进群免费领取"},
             token=USER_TOKEN, timeout=240)
check("违规回复被 AI 拦截", st == 200 and r.get("blocked") is True and r.get("reply") is None,
      f"HTTP {st} blocked={r.get('blocked')} 原因={str(r.get('message'))[:50]}")

st, r = call("POST", f"/articles/{thread_id}/replies", {"content": "引用回复测试", "parent_id": reply_id},
             token=USER_TOKEN, timeout=240)
check("楼中楼（引用回复）可用", st == 201 and r.get("reply", {}).get("parent_id") == reply_id,
      f"HTTP {st} parent_id={r.get('reply', {}).get('parent_id') if st == 201 else '-'}")

st, r = call("GET", f"/articles/{thread_id}/replies?page=1&size=20")
# 主楼 1 条（正常回复）+ 其下 1 条楼中楼；违规回复已被拦截不入库
main_floors = r.get("total", 0) if st == 200 else 0
children = sum(len(n.get("children", [])) for n in (r.get("items") or []))
check("回复列表可查询（两层结构）", st == 200 and main_floors >= 1 and children >= 1,
      f"HTTP {st} 主楼数={main_floors} 楼中楼={children}")
if st == 200 and r.get("items"):
    for node in r["items"]:
        author = node["reply"].get("author") or {}
        name = author.get("nickname") or author.get("username") or "匿名"
        print(f"       {node['reply']['floor']} 楼 @{name}: "
              f"{node['reply']['content'][:26]}  子回复={len(node.get('children', []))}")

st, r = call("GET", f"/articles/{thread_id}")
check("帖子详情返回回复数", st == 200 and r.get("reply_count", 0) >= 2, f"reply_count={r.get('reply_count')}")

# 点赞
st, r = call("POST", "/likes/toggle", {"target_type": "article", "target_id": thread_id}, token=USER_TOKEN)
check("帖子点赞成功", st == 200 and r.get("liked") is True and r.get("like_count", 0) >= 1,
      f"HTTP {st} liked={r.get('liked')} count={r.get('like_count')}")
st, r = call("POST", "/likes/toggle", {"target_type": "article", "target_id": thread_id}, token=USER_TOKEN)
check("再次点击取消点赞", st == 200 and r.get("liked") is False, f"liked={r.get('liked')} count={r.get('like_count')}")
st, r = call("POST", "/likes/toggle", {"target_type": "reply", "target_id": reply_id}, token=USER_TOKEN)
check("回复点赞成功", st == 200 and r.get("liked") is True, f"HTTP {st}")

# 重新给帖子点赞，再验证详情里的 liked 状态（上一步刚取消过，此处必须重新点上）
st, r = call("POST", "/likes/toggle", {"target_type": "article", "target_id": thread_id}, token=USER_TOKEN)
check("重新点赞帖子", st == 200 and r.get("liked") is True, f"liked={r.get('liked')}")
st, r = call("GET", f"/articles/{thread_id}", token=USER_TOKEN)
check("详情返回当前用户点赞状态", st == 200 and r.get("liked") is True,
      f"liked={r.get('liked') if st == 200 else '-'} count={r.get('like_count') if st == 200 else '-'}")

# 另取一个全新用户的 Token（步骤 10 里注销过 other，Token 会失效）
third = f"third{RUN_TAG}"
call("POST", "/register", {"username": third, "password": "test123456", "nickname": "第三用户"})
st, r = call("POST", "/login", {"username": third, "password": "test123456"})
THIRD_TOKEN = r.get("token") if st == 200 else None
st, r = call("POST", "/likes/toggle", {"target_type": "article", "target_id": thread_id}, token=THIRD_TOKEN)
check("其他用户可独立点赞", st == 200 and r.get("liked") is True and r.get("like_count", 0) >= 2,
      f"HTTP {st} count={r.get('like_count')}")
st, r = call("GET", f"/articles/{thread_id}", token=THIRD_TOKEN)
check("点赞状态是按用户区分的", st == 200 and r.get("liked") is True,
      f"第三用户 liked={r.get('liked') if st == 200 else '-'}")
st, r = call("GET", f"/articles/{thread_id}")
check("未登录用户 liked 恒为 false", st == 200 and r.get("liked") is False,
      f"liked={r.get('liked') if st == 200 else '-'}")

# 用户主页
st, me_res = call("GET", "/me", token=USER_TOKEN)
my_uid = me_res.get("user", {}).get("id") if st == 200 else 1
st, r = call("GET", f"/users/{my_uid}/profile")
check("用户主页可用", st == 200 and r.get("user", {}).get("id") == my_uid, f"HTTP {st}")
if st == 200:
    print(f"       用户={r['user']['username']} 发帖={r['thread_count']} "
          f"回复={r['reply_count']} 获赞={r['like_received']}")
    check("用户主页统计到帖子与回复", r["thread_count"] >= 1 and r["reply_count"] >= 2,
          f"发帖={r['thread_count']} 回复={r['reply_count']}")

# 审核员版块管理
if ADMIN_TOKEN:
    st, r = call("PUT", f"/admin/articles/{thread_id}/flags", {"pinned": True, "featured": True}, token=ADMIN_TOKEN)
    check("审核员可置顶/加精", st == 200 and r.get("article", {}).get("pinned") is True,
          f"HTTP {st} pinned={r.get('article', {}).get('pinned') if st == 200 else '-'}")
    st, r = call("PUT", f"/admin/articles/{thread_id}/flags", {"pinned": True}, token=USER_TOKEN)
    check("普通用户无权置顶", st == 403, f"HTTP {st}")
    st, r = call("GET", "/articles?sort=ess&status=published&size=20")
    check("精华筛选包含加精帖", st == 200 and any(i["id"] == thread_id for i in r.get("items", [])),
          f"HTTP {st}")

# 删除回复
st, r = call("DELETE", f"/replies/{reply_id}", token=USER_TOKEN)
check("作者可删除自己的回复", st == 200, f"HTTP {st}")

print()
print("=" * 78)
print(f"结果汇总：通过 {len(PASS)} 项，失败 {len(FAIL)} 项")
print("=" * 78)
if FAIL:
    print("失败项：")
    for f in FAIL:
        print("  ✗", f)
else:
    print("✅ 全部检查通过")
