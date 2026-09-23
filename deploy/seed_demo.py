"""为界面走查准备演示数据：版块分布、置顶/精华、多回复、多状态帖子。"""

import json
import time
import urllib.error
import urllib.request

B = "http://127.0.0.1:8080/api"
TAG = str(int(time.time()))[-6:]


def call(m, p, payload=None, tok=None, timeout=240):
    d = json.dumps(payload, ensure_ascii=False).encode() if payload is not None else None
    h = {"Content-Type": "application/json"}
    if tok:
        h["Authorization"] = "Bearer " + tok
    try:
        with urllib.request.urlopen(urllib.request.Request(B + p, data=d, headers=h, method=m), timeout=timeout) as r:
            body = r.read().decode()
            return r.status, (json.loads(body) if body else {})
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def login(u, p):
    st, r = call("POST", "/login", {"username": u, "password": p})
    return r["token"] if st == 200 else None


admin = login("admin", "admin123456")
users = []
for name in ("xuanye", "linzhi", "chenmu"):
    uname = f"{name}{TAG}"
    call("POST", "/register", {"username": uname, "password": "demo123456", "nickname": name})
    t = login(uname, "demo123456")
    if t:
        users.append((uname, t))
print(f"演示用户 {len(users)} 个")

THREADS = [
    ("tech", "Go 并发编排：把串行审核改成同时开工，实测加速比 4.3x",
     "内容审核的传统做法是串行流水线：先过敏感词、再判广告、再判辱骂，总耗时是各段之和。\n\n"
     "这次我们把互不依赖的环节改成同时启动：规则引擎在本地 CPU 上跑，五个语义维度并行送审大模型，"
     "长文再按句子边界切片做二级并行。\n\n"
     "实测单篇审核从串行等价 5307ms 降到 1280ms，加速比 4.15x；批量审核 4 篇达到 9.83x。"
     "关键不在于用多少协程，而在于把任务依赖关系理清楚——只有真正独立的计算才能并行。"),
    ("ai", "AI 审核会不会误杀？谈谈我们的兜底策略",
     "很多人担心大模型审核把正常内容误判成违规。我们的做法是把判定拆成三档：通过、待人工复核、拒绝。\n\n"
     "只有命中致命规则或一票否决维度（涉黄、涉政、违法）才直接拒绝；风险分在 30-60 之间转人工；"
     "AI 服务整体不可用时，宁可全部转人工也绝不静默放行。\n\n"
     "审核结论里会带上命中的规则词和证据片段，人工复核时不用重新通读全文。"),
    ("create", "写了三年技术博客，我总结出这五条",
     "第一，先写结论再补论证，读者没耐心等你铺垫。\n"
     "第二，一篇文章只讲一件事，想讲五件事就拆成五篇。\n"
     "第三，代码示例要能跑，贴伪代码不如不贴。\n"
     "第四，标题里放具体数字，比形容词有说服力。\n"
     "第五，写完隔一天再改，你会删掉三成内容。"),
    ("ask", "分片并行时，片与片的接缝处怎么避免漏判？",
     "长文切分后送审，如果违规内容刚好横跨两个分片的边界，两边各看到一半，就可能都判成正常。\n\n"
     "我们现在的做法是相邻分片保留 40 个字符的重叠。切分点还会优先选择句号、换行这类自然边界，"
     "而不是硬切在第 600 个字上。\n\n"
     "想听听大家还有没有更好的思路？"),
    ("notice", "社区规范与 AI 审核说明（新人必读）",
     "本社区所有帖子与回复在提交时都会经过 AI 并行审核。\n\n"
     "审核通过即自动发布；判定疑似违规会转人工复核，期间内容不对外展示；"
     "明确违规则直接拒绝并告知原因。\n\n"
     "如果你认为审核结果有误，可以在帖子下留言申诉，审核员会人工复查。"),
    ("chat", "你们的显示器都多大？我 1707 宽的屏总感觉页面用不满",
     "如题，感觉很多网站内容都挤在中间一条，两边白白空着。"),
    ("tech", "为什么我把 worker pool 的并发度从 16 调回 8",
     "一开始觉得并发越高越快，实测发现超过某个点之后总耗时反而上升。\n\n"
     "原因是下游大模型服务有配额限制，并发太高会触发限流，重试带来额外延迟。"
     "现在按 AI_CONCURRENCY 和 MAX_INFLIGHT 两个闸门控制：前者限制单请求内部并发，"
     "后者限制同时在途的请求数。"),
]

created = []
for board, title, content in THREADS:
    st, r = call("POST", "/articles", {"title": title, "content": content,
                                       "tags": "并发,审核,实践" if board == "tech" else "讨论,社区",
                                       "board": board}, users[0][1])
    if st == 201:
        created.append(r["id"])
        st2, r2 = call("POST", f"/articles/{r['id']}/publish", {}, users[0][1])
        print(f"  发帖 {st2} {r2.get('audit', {}).get('verdict') if st2 == 200 else '-'}  {title[:22]}")

print(f"已发布 {len(created)} 篇")

# 给第一帖加回复，构造楼层与楼中楼
if created:
    tid = created[0]
    replies = [
        (users[1][1], "思路很清晰。我们团队之前也在做类似的事，不过卡在了结果聚合上——多个维度的结论怎么加权才合理？"),
        (users[2][1], "同问，尤其是某个维度分数很高但其他维度都正常的情况。"),
        (users[0][1], "我们的做法是权重 + 一票否决：涉黄涉政涉违法这三个维度只要判违规就直接拒绝，其余走加权求和，超过 60 分拒绝、30-60 转人工。"),
        (users[1][1], "受教了，回头试试。另外建议把每个维度的耗时也打出来，方便定位是哪个分支拖慢了整体。"),
    ]
    root = None
    for tok, text in replies:
        payload = {"content": text}
        if root and "同问" in text:
            payload["parent_id"] = root
        st, r = call("POST", f"/articles/{tid}/replies", payload, tok)
        if st == 201 and root is None:
            root = r["reply"]["id"]
        print(f"  回复 {st} floor={r.get('reply', {}).get('floor') if st == 201 else '-'}")

    # 点赞
    for _, tok in users[1:]:
        call("POST", "/likes/toggle", {"target_type": "article", "target_id": tid}, tok)
    call("POST", "/likes/toggle", {"target_type": "article", "target_id": created[1]}, users[1][1])

# 置顶 + 加精
if len(created) >= 2:
    call("PUT", f"/admin/articles/{created[0]}/flags", {"pinned": True, "featured": True}, admin)
    call("PUT", f"/admin/articles/{created[1]}/flags", {"featured": True}, admin)
    print("已设置置顶与精华")

# 制造几种状态：草稿、待复核、已拒绝
call("POST", "/articles", {"title": "这是一篇还没写完的草稿", "content": "草稿内容……", "board": "tech"}, users[0][1])
st, r = call("POST", "/articles", {"title": "低价货源内部渠道，加微信详聊",
                                   "content": "加微信 abc12345 兼职日结，扫码进群免费领取", "board": "chat"}, users[0][1])
if st == 201:
    call("POST", f"/articles/{r['id']}/publish", {}, users[0][1])

st, ov = call("GET", "/forum/overview")
print("\n最终站点数据:", ov.get("stats"))
