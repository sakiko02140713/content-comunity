/* ==========================================================================
   智能内容社区 · 论坛前端（Vue 3）
   路由：hash 路由（#/、#/b/:slug、#/t/:id、#/u/:id ...）
   ========================================================================== */

const { createApp } = Vue;

const API = window.location.port === '8080'
    ? '/api'
    : 'http://localhost:8080/api';

const VERDICT_TEXT = { pass: '审核通过', review: '待人工复核', reject: '审核拒绝' };
const STATUS_TEXT = { draft: '草稿', review: '待复核', rejected: '已拒绝', published: '已发布' };
const MODE_TEXT = { local: '本地规则', semantic: '多维度语义并行', chunked: '长文分片并发' };
const SORT_OPTIONS = [
    { key: 'last', label: '最新回复' },
    { key: 'new', label: '最新发布' },
    { key: 'hot', label: '最多浏览' },
    { key: 'ess', label: '精华' },
];
const AVATAR_COLORS = ['#2563eb', '#7c3aed', '#0891b2', '#059669', '#d97706', '#dc2626', '#db2777', '#4f46e5'];

/* ---------------------------------------------------------------------------
   通用工具
   --------------------------------------------------------------------------- */
function authorName(a, fallback) {
    if (!a) return fallback || '匿名';
    return a.nickname || a.username || fallback || '匿名';
}
function displayName(a) { return authorName(a, '匿名用户'); }

function initial(a) {
    const name = authorName(a, '?');
    return name.trim().charAt(0).toUpperCase() || '?';
}

// 依据用户名生成稳定的头像底色，避免同一用户每次刷新颜色不同。
function avatarColor(a) {
    const name = authorName(a, 'anonymous');
    let hash = 0;
    for (let i = 0; i < name.length; i++) hash = (hash * 31 + name.charCodeAt(i)) % 100000;
    return AVATAR_COLORS[hash % AVATAR_COLORS.length];
}

function avatarStyle(a) {
    const color = avatarColor(a);
    return { background: `linear-gradient(135deg, ${color}, ${color}cc)` };
}

function formatTime(d) {
    if (!d) return '';
    const t = new Date(d).getTime();
    if (isNaN(t)) return '';
    const diff = Date.now() - t;
    const min = 60000, hour = 60 * min, day = 24 * hour;
    if (diff < min) return '刚刚';
    if (diff < hour) return Math.floor(diff / min) + ' 分钟前';
    if (diff < day) return Math.floor(diff / hour) + ' 小时前';
    if (diff < 7 * day) return Math.floor(diff / day) + ' 天前';
    return new Date(d).toLocaleDateString('zh-CN');
}

function formatDate(d) {
    if (!d) return '—';
    return new Date(d).toLocaleDateString('zh-CN');
}

function splitTags(raw) {
    if (!raw) return [];
    return String(raw).split(/[,，;；|\s]+/).filter(Boolean).slice(0, 5);
}

/* ---------------------------------------------------------------------------
   帖子行组件
   --------------------------------------------------------------------------- */
const ThreadRow = {
    props: { thread: { type: Object, required: true } },
    template: '#thread-row-template',
    // 事件向上冒泡，由根组件统一处理路由跳转，避免组件直接依赖父级实例
    emits: ['open', 'open-board', 'open-user'],
    methods: {
        initial, avatarStyle, authorName, formatTime, splitTags,
        goUser(id) { this.$emit('open-user', id); },
        goBoard(slug) { this.$emit('open-board', slug); },
    },
};

/* ---------------------------------------------------------------------------
   审核结果组件：结论 + 并行指标 + 甘特图 + 维度表 + 规则命中
   --------------------------------------------------------------------------- */
const AuditResult = {
    props: { result: { type: Object, required: true } },
    template: '#audit-result-template',
    computed: {
        verdictIcon() { return { pass: '✅', review: '⚠️', reject: '⛔' }[this.result.verdict] || 'ℹ️'; },
        verdictTitle() { return VERDICT_TEXT[this.result.verdict] || this.result.verdict; },
    },
    methods: {
        modeText(m) { return MODE_TEXT[m] || m; },
        // 条形用"相对流水线起点的百分比"定位，同一起点即直观体现并行
        barStyle(stage) {
            const total = Math.max(this.result.elapsed_ms || 1, 1);
            const left = Math.min(100, Math.max(0, (stage.start_ms / total) * 100));
            const width = Math.min(100 - left, Math.max(2, (stage.duration / total) * 100));
            return { left: left + '%', width: width + '%' };
        },
        taskBarStyle(task) {
            const total = Math.max(this.result.elapsed_ms || 1, 1);
            const left = Math.min(100, Math.max(0, (task.start_ms / total) * 100));
            const width = Math.min(100 - left, Math.max(2, (task.duration / total) * 100));
            return { left: left + '%', width: width + '%' };
        },
    },
};

/* ---------------------------------------------------------------------------
   应用
   --------------------------------------------------------------------------- */
createApp({
    components: { 'thread-row': ThreadRow, 'audit-result': AuditResult },

    data() {
        return {
            route: { name: 'home', board: '', id: 0 },
            lastListRoute: '',
            token: localStorage.getItem('token') || '',
            me: JSON.parse(localStorage.getItem('user') || '{}'),
            myStats: { threads: 0, replies: 0, likes: 0 },

            boards: [],
            overview: { stats: {}, boards: [], latest: [], hot: [] },
            search: { keyword: '' },
            sortOptions: SORT_OPTIONS,

            list: { items: [], total: 0, page: 1, size: 20, sort: 'last', loading: false },

            thread: {},
            replies: { items: [], total: 0, page: 1, size: 20 },
            authorStats: {},
            summarizing: false,

            replyForm: { content: '', parent: 0, parentFloor: 0, parentAuthor: null },
            replyLoading: false,
            replyMsg: { text: '', type: '' },
            replyAudit: null,

            compose: { id: null, board: 'tech', title: '', content: '', tags: '', status: '' },
            composeLoading: false,
            draftLoading: false,
            composeMsg: { text: '', type: '' },
            aiPolishing: false,
            polishResult: null,

            liveEvents: [],
            liveAudit: { running: false, elapsed: 0, result: null, timer: null },

            profile: { user: {}, recent_posts: [], recent_replies: [] },

            mine: { items: [], total: 0, page: 1, size: 10, filter: 'all' },
            statusTabs: [
                { key: 'all', label: '全部' },
                { key: 'draft', label: '草稿' },
                { key: 'review', label: '待复核' },
                { key: 'published', label: '已发布' },
                { key: 'rejected', label: '已拒绝' },
            ],
            batchLoading: false,
            batchResult: null,
            reviewQueue: [],

            authMode: 'login',
            authForm: { username: 'admin', password: 'admin123456', nickname: '' },
            authLoading: false,
            authMsg: { text: '', type: '' },

            auditForm: { title: '', content: '', mode: 'semantic', concurrency: 8 },
            auditLoading: false,
            auditResult: null,
            auditInfo: {},
            auditInsight: {},
            aiHealth: {},
        };
    },

    computed: {
        isAdmin() { return this.me && this.me.role === 'admin'; },
    },

    methods: {
        initial, avatarStyle, authorName, displayName, formatTime, formatDate, splitTags,
        statusText(s) { return STATUS_TEXT[s] || s; },
        verdictText(v) { return VERDICT_TEXT[v] || v; },

        authHeaders() { return this.token ? { Authorization: 'Bearer ' + this.token } : {}; },

        errText(e, fallback) {
            return (e && e.response && e.response.data &&
                (e.response.data.error || e.response.data.detail)) || fallback;
        },

        /* ============================ 路由 ============================ */
        parseHash() {
            const raw = (location.hash || '#/').replace(/^#/, '');
            const parts = raw.split('/').filter(Boolean);
            if (!parts.length) return { name: 'home' };
            switch (parts[0]) {
                case 'b': return { name: 'list', board: parts[1] || '' };
                case 't': return { name: 'thread', id: Number(parts[1]) || 0 };
                case 'u': return { name: 'user', id: Number(parts[1]) || 0 };
                case 'latest': return { name: 'list', board: '', sort: 'new' };
                case 'hot': return { name: 'list', board: '', sort: 'hot' };
                case 'essence': return { name: 'list', board: '', sort: 'ess' };
                case 'compose': return { name: 'compose' };
                case 'me': return { name: 'me' };
                case 'audit-lab': return { name: 'audit' };
                case 'login': return { name: 'login' };
                case 'search': return { name: 'list', board: '', keyword: decodeURIComponent(parts[1] || '') };
                default: return { name: 'home' };
            }
        },

        go(path) {
            const h = '#' + path;
            if (location.hash !== h) location.hash = h;
            else this.onRoute();
        },

        goBoard(slug) { this.go('/b/' + slug); },
        goUser(id) { if (id) this.go('/u/' + id); },
        openThread(id) { if (id) this.go('/t/' + id); },

        onRoute() {
            this.route = this.parseHash();
            window.scrollTo({ top: 0 });

            switch (this.route.name) {
                case 'home':
                    this.loadOverview();
                    break;
                case 'list': {
                    // /latest /hot /essence 各自对应一种排序；版块与搜索沿用"最新回复"
                    const sortByRoute = { latest: 'new', hot: 'hot', essence: 'ess' };
                    if (this.route.sort) {
                        this.list.sort = this.route.sort;
                    } else if (this.lastListRoute && this.lastListRoute in sortByRoute) {
                        // 从 /latest 等排序页切换到版块页时回到默认排序
                        this.list.sort = 'last';
                    }
                    if (this.route.keyword !== undefined) {
                        this.search.keyword = this.route.keyword || '';
                    } else if (this.lastListRoute === 'search') {
                        this.search.keyword = '';
                    }
                    this.lastListRoute = this.route.keyword !== undefined ? 'search' : (this.route.sort || 'board');
                    this.loadList(1);
                    break;
                }
                case 'thread':
                    this.loadThread(this.route.id);
                    break;
                case 'user':
                    this.loadProfile(this.route.id);
                    break;
                case 'me':
                    this.loadMine(1);
                    this.loadMe();
                    this.loadHealth();
                    if (this.isAdmin) this.loadReviewQueue();
                    break;
                case 'compose':
                    this.loadBoards();
                    break;
                case 'audit':
                    this.loadAuditInfo();
                    this.loadAuditStats();
                    break;
            }
            // 侧边栏的热门讨论在所有页面都展示
            if (!['home', 'list', 'thread', 'user'].includes(this.route.name)) this.loadOverview();
        },

        doSearch() {
            const kw = (this.search.keyword || '').trim();
            if (!kw) return;
            this.go('/search/' + encodeURIComponent(kw));
        },

        changeSort(key) {
            this.list.sort = key;
            this.loadList(1);
        },

        /* ============================ 数据加载 ============================ */
        async loadBoards() {
            try {
                const res = await axios.get(API + '/boards');
                this.boards = res.data.items || [];
            } catch (e) { /* 忽略 */ }
        },

        async loadOverview() {
            try {
                const res = await axios.get(API + '/forum/overview', { headers: this.authHeaders() });
                this.overview = res.data;
                if (res.data.boards && res.data.boards.length) this.boards = res.data.boards;
            } catch (e) { /* 忽略 */ }
        },

        async loadList(page) {
            this.list.loading = true;
            this.list.page = page || 1;
            try {
                const res = await axios.get(API + '/articles', {
                    headers: this.authHeaders(),
                    params: {
                        page: this.list.page,
                        size: this.list.size,
                        sort: this.list.sort,
                        board: this.route.board || '',
                        keyword: this.search.keyword || '',
                        status: 'published',
                    },
                });
                this.list.items = res.data.items || [];
                this.list.total = res.data.total || 0;
            } catch (e) {
                this.list.items = [];
                this.list.total = 0;
            }
            this.list.loading = false;
        },

        async loadThread(id) {
            if (!id) return;
            this.thread = {};
            this.replies = { items: [], total: 0, page: 1, size: 20 };
            this.replyAudit = null;
            this.replyMsg = { text: '', type: '' };
            this.cancelQuote();

            try {
                const res = await axios.get(API + '/articles/' + id, { headers: this.authHeaders() });
                this.thread = res.data;
            } catch (e) {
                this.thread = {};
                this.replyMsg = { text: this.errText(e, '帖子不存在或尚未发布'), type: 'error' };
                return;
            }
            this.loadReplies(1);
            // 楼主资料（用于侧边栏统计）
            if (this.thread.author_id) {
                axios.get(API + '/users/' + this.thread.author_id + '/profile', { headers: this.authHeaders() })
                    .then((r) => { this.authorStats = r.data; })
                    .catch(() => { this.authorStats = {}; });
            }
            // 帖子访问会带来浏览量变化，稍后刷新热榜数字
            this.loadOverview();
        },

        async loadReplies(page) {
            if (!this.thread.id) return;
            this.replies.page = page || 1;
            try {
                const res = await axios.get(API + '/articles/' + this.thread.id + '/replies', {
                    headers: this.authHeaders(),
                    params: { page: this.replies.page, size: this.replies.size },
                });
                this.replies.items = res.data.items || [];
                this.replies.total = res.data.total || 0;
            } catch (e) { /* 忽略 */ }
        },

        async loadProfile(id) {
            if (!id) return;
            try {
                const res = await axios.get(API + '/users/' + id + '/profile', { headers: this.authHeaders() });
                this.profile = res.data;
            } catch (e) {
                this.profile = { user: {}, recent_posts: [], recent_replies: [] };
            }
        },

        async loadMe() {
            if (!this.token) return;
            try {
                const res = await axios.get(API + '/me', { headers: this.authHeaders() });
                this.me = res.data.user || {};
                localStorage.setItem('user', JSON.stringify(this.me));
                const p = await axios.get(API + '/users/' + this.me.id + '/profile', { headers: this.authHeaders() });
                this.myStats = {
                    threads: p.data.thread_count || 0,
                    replies: p.data.reply_count || 0,
                    likes: p.data.like_received || 0,
                };
            } catch (e) { /* 忽略 */ }
        },

        async loadMine(page) {
            if (!this.token) return;
            this.mine.page = page || 1;
            try {
                const res = await axios.get(API + '/my/articles', {
                    headers: this.authHeaders(),
                    params: { page: this.mine.page, size: this.mine.size, status: this.mine.filter },
                });
                this.mine.items = res.data.items || [];
                this.mine.total = res.data.total || 0;
            } catch (e) { /* 忽略 */ }
        },

        switchMine(key) {
            this.mine.filter = key;
            this.loadMine(1);
        },

        async loadReviewQueue() {
            try {
                const res = await axios.get(API + '/articles', {
                    headers: this.authHeaders(),
                    params: { status: 'review', page: 1, size: 10 },
                });
                this.reviewQueue = res.data.items || [];
            } catch (e) { /* 忽略 */ }
        },

        async loadHealth() {
            try {
                const res = await axios.get(API + '/ai/health');
                this.aiHealth = res.data;
            } catch (e) { this.aiHealth = { status: 'unavailable' }; }
        },

        async loadAuditInfo() {
            try {
                const res = await axios.get(API + '/ai/audit/info');
                this.auditInfo = res.data;
            } catch (e) { /* 忽略 */ }
        },

        async loadAuditStats() {
            if (!this.token) return;
            try {
                const res = await axios.get(API + '/ai/stats', { headers: this.authHeaders() });
                this.auditInsight = res.data.audit || {};
            } catch (e) { /* 忽略 */ }
        },

        /* ============================ 认证 ============================ */
        async doAuth() {
            this.authLoading = true;
            this.authMsg = { text: '', type: '' };
            try {
                if (this.authMode === 'register') {
                    await axios.post(API + '/register', this.authForm);
                    this.authMsg = { text: '注册成功，正在为你登录…', type: 'success' };
                }
                const res = await axios.post(API + '/login', {
                    username: this.authForm.username,
                    password: this.authForm.password,
                });
                this.token = res.data.token;
                this.me = res.data.user || {};
                localStorage.setItem('token', this.token);
                localStorage.setItem('user', JSON.stringify(this.me));
                this.go('/');
                this.loadMe();
            } catch (e) {
                this.authMsg = { text: this.errText(e, '登录失败，请检查用户名与密码'), type: 'error' };
            }
            this.authLoading = false;
        },

        async logout() {
            try { await axios.post(API + '/logout', {}, { headers: this.authHeaders() }); } catch (e) { /* 忽略 */ }
            this.token = '';
            this.me = {};
            this.myStats = { threads: 0, replies: 0, likes: 0 };
            localStorage.removeItem('token');
            localStorage.removeItem('user');
            this.go('/');
        },

        canEdit(obj) {
            if (!this.token || !obj) return false;
            return this.isAdmin || obj.author_id === this.me.id;
        },

        /* ============================ 互动：点赞 ============================ */
        async toggleLike(targetType, target) {
            if (!this.token) { this.go('/login'); return; }
            try {
                const res = await axios.post(API + '/likes/toggle', {
                    target_type: targetType,
                    target_id: target.id,
                }, { headers: this.authHeaders() });
                target.liked = res.data.liked;
                target.like_count = res.data.like_count;
                if (targetType === 'article') this.thread.like_count = res.data.like_count;
            } catch (e) {
                this.replyMsg = { text: this.errText(e, '点赞失败'), type: 'error' };
            }
        },

        /* ============================ 互动：回复 ============================ */
        focusReply() {
            const el = this.$refs.replyBox;
            if (el && el.scrollIntoView) el.scrollIntoView({ behavior: 'smooth', block: 'center' });
        },

        quote(reply) {
            this.replyForm.parent = reply.id;
            this.replyForm.parentFloor = reply.floor || 0;
            this.replyForm.parentAuthor = reply.author;
            this.focusReply();
        },

        cancelQuote() {
            this.replyForm.parent = 0;
            this.replyForm.parentFloor = 0;
            this.replyForm.parentAuthor = null;
        },

        async submitReply() {
            if (!this.replyForm.content.trim()) {
                this.replyMsg = { text: '回复内容不能为空', type: 'error' };
                return;
            }
            this.replyLoading = true;
            this.replyMsg = { text: '', type: '' };
            this.replyAudit = null;
            try {
                const res = await axios.post(API + '/articles/' + this.thread.id + '/replies', {
                    content: this.replyForm.content,
                    parent_id: this.replyForm.parent,
                }, { headers: this.authHeaders() });

                this.replyAudit = res.data.audit || null;
                if (res.data.blocked) {
                    this.replyMsg = { text: res.data.message, type: 'error' };
                } else {
                    this.replyMsg = {
                        text: res.data.message || '回复成功',
                        type: res.data.audit && res.data.audit.verdict === 'review' ? 'warn' : 'success',
                    };
                    this.replyForm.content = '';
                    this.cancelQuote();
                    this.loadReplies(this.replies.page);
                    this.thread.reply_count = (this.thread.reply_count || 0) + 1;
                }
            } catch (e) {
                this.replyMsg = { text: this.errText(e, '回复失败'), type: 'error' };
            }
            this.replyLoading = false;
        },

        async removeReply(reply) {
            if (typeof window.confirm === 'function' && !window.confirm('确认删除这条回复？')) return;
            try {
                await axios.delete(API + '/replies/' + reply.id, { headers: this.authHeaders() });
                this.loadReplies(this.replies.page);
                if (this.thread.reply_count > 0) this.thread.reply_count -= 1;
            } catch (e) {
                this.replyMsg = { text: this.errText(e, '删除失败'), type: 'error' };
            }
        },

        /* ============================ 发帖 ============================ */
        openCompose(prefill) {
            this.compose = Object.assign(
                { id: null, board: 'tech', title: '', content: '', tags: '', status: '' },
                prefill || {}
            );
            this.composeMsg = { text: '', type: '' };
            this.polishResult = null;
            this.liveEvents = [];
            this.liveAudit = { running: false, elapsed: 0, result: null, timer: null };
            this.go('/compose');
        },

        async saveDraft() {
            if (!this.compose.title.trim() || !this.compose.content.trim()) {
                this.composeMsg = { text: '标题和正文不能为空', type: 'error' };
                return;
            }
            this.draftLoading = true;
            try {
                const payload = {
                    title: this.compose.title,
                    content: this.compose.content,
                    tags: this.compose.tags,
                    board: this.compose.board,
                };
                let res;
                if (this.compose.id) {
                    res = await axios.put(API + '/articles/' + this.compose.id, payload, { headers: this.authHeaders() });
                    this.composeMsg = { text: '已保存（修改后需重新审核才能发布）', type: 'success' };
                } else {
                    res = await axios.post(API + '/articles', payload, { headers: this.authHeaders() });
                    this.composeMsg = { text: '草稿已保存', type: 'success' };
                }
                this.compose.id = res.data.id;
                this.compose.status = res.data.status;
            } catch (e) {
                this.composeMsg = { text: this.errText(e, '保存失败'), type: 'error' };
            }
            this.draftLoading = false;
        },

        // 帖子必须经过审核才能发布：先入库（草稿）→ 再触发并行审核发布
        async submitPost() {
            if (!this.compose.title.trim() || !this.compose.content.trim()) {
                this.composeMsg = { text: '标题和正文不能为空', type: 'error' };
                return;
            }
            this.composeLoading = true;
            this.composeMsg = { text: '', type: '' };
            this.liveEvents = [];
            this.liveAudit = { running: true, elapsed: 0, result: null, timer: null };
            const started = Date.now();
            this.liveAudit.timer = setInterval(() => { this.liveAudit.elapsed = Date.now() - started; }, 100);

            try {
                // 1) 保存（新建或更新）
                const payload = {
                    title: this.compose.title,
                    content: this.compose.content,
                    tags: this.compose.tags,
                    board: this.compose.board,
                };
                let articleRes;
                if (this.compose.id) {
                    articleRes = await axios.put(API + '/articles/' + this.compose.id, payload, { headers: this.authHeaders() });
                } else {
                    articleRes = await axios.post(API + '/articles', payload, { headers: this.authHeaders() });
                }
                const id = articleRes.data.id;
                this.compose.id = id;

                // 2) 并行审核并发布（SSE 实时展示审核分支）
                const audit = await this.postAuditStream(
                    API + '/ai/audit/stream?article_id=' + id,
                    { title: this.compose.title, content: this.compose.content, mode: 'semantic', concurrency: 8 }
                );
                this.liveAudit.result = audit;

                if (audit.verdict === 'pass') {
                    this.composeMsg = { text: '✅ 审核通过，帖子已发布！', type: 'success' };
                    setTimeout(() => this.openThread(id), 700);
                } else if (audit.verdict === 'review') {
                    this.composeMsg = { text: '⚠️ 疑似违规，已转人工复核，暂不对外展示', type: 'warn' };
                } else {
                    this.composeMsg = { text: '⛔ 审核未通过：' + audit.reason, type: 'error' };
                }
            } catch (e) {
                this.composeMsg = { text: this.errText(e, '提交失败'), type: 'error' };
            }
            clearInterval(this.liveAudit.timer);
            this.liveAudit.running = false;
            this.composeLoading = false;
        },

        async publishExisting(id) {
            this.composeMsg = { text: '正在并行审核…', type: 'info' };
            try {
                const res = await axios.post(API + '/articles/' + id + '/publish', {}, { headers: this.authHeaders() });
                this.composeMsg = {
                    text: res.data.message,
                    type: res.data.audit && res.data.audit.verdict === 'pass' ? 'success' : 'warn',
                };
                this.loadMine(this.mine.page);
            } catch (e) {
                this.composeMsg = { text: this.errText(e, '发布失败'), type: 'error' };
            }
        },

        editThread(t) {
            this.openCompose({
                id: t.id,
                board: (t.board && t.board.slug) || 'tech',
                title: t.title,
                content: t.content || '',
                tags: t.tags || '',
                status: t.status,
            });
        },

        async removeThread(t) {
            if (typeof window.confirm === 'function' && !window.confirm('确认删除这个帖子？')) return;
            try {
                await axios.delete(API + '/articles/' + t.id, { headers: this.authHeaders() });
                if (this.route.name === 'thread') this.go('/');
                else if (this.route.name === 'me') this.loadMine(this.mine.page);
                else this.loadList(this.list.page);
            } catch (e) {
                this.composeMsg = { text: this.errText(e, '删除失败'), type: 'error' };
            }
        },

        async toggleFlag(t, field) {
            try {
                const body = {};
                body[field] = !t[field];
                const res = await axios.put(API + '/admin/articles/' + t.id + '/flags', body, { headers: this.authHeaders() });
                this.thread = res.data.article;
            } catch (e) { /* 忽略 */ }
        },

        async review(id, decision) {
            try {
                await axios.post(API + '/admin/articles/' + id + '/review', {
                    decision,
                    reason: decision === 'approve' ? '人工复核通过' : '人工复核判定违规',
                }, { headers: this.authHeaders() });
                this.loadReviewQueue();
                this.loadMine(this.mine.page);
            } catch (e) { /* 忽略 */ }
        },

        async summarize(t) {
            this.summarizing = true;
            try {
                const res = await axios.post(API + '/ai/articles/' + t.id + '/summary', {}, { headers: this.authHeaders() });
                this.thread.summary = res.data.summary;
            } catch (e) {
                this.replyMsg = { text: this.errText(e, 'AI 总结失败'), type: 'error' };
            }
            this.summarizing = false;
        },

        /**
         * AI 润色：只对用户已经写好的正文做语言打磨。
         * 正文为空时按钮本身是禁用的，这里再兜一层校验，双保险。
         */
        async aiPolish() {
            const content = (this.compose.content || '').trim();
            if (!content) {
                this.composeMsg = { text: '请先输入正文内容，AI 才能进行润色', type: 'error' };
                return;
            }
            this.aiPolishing = true;
            this.polishResult = null;
            this.composeMsg = { text: '', type: '' };
            try {
                const res = await axios.post(API + '/ai/polish', {
                    title: this.compose.title,
                    content: this.compose.content,
                }, { headers: this.authHeaders() });

                this.polishResult = {
                    content: res.data.polished_content,
                    title: res.data.title || '',
                    originalLength: content.length,
                    elapsedMs: res.data.elapsed_ms || 0,
                    degraded: res.data.degraded,
                    changed: res.data.changed,
                };
                if (res.data.degraded) {
                    this.composeMsg = { text: 'AI 服务当前不可用，未做任何改写（已保留你的原文）', type: 'warn' };
                } else if (!res.data.changed) {
                    this.composeMsg = { text: 'AI 认为原文已经不错，未做改动', type: 'info' };
                }
            } catch (e) {
                this.composeMsg = { text: this.errText(e, 'AI 润色失败'), type: 'error' };
            }
            this.aiPolishing = false;
        },

        /** 用润色结果替换正文 */
        applyPolish() {
            if (!this.polishResult) return;
            this.compose.content = this.polishResult.content;
            this.composeMsg = { text: '已用润色后的内容替换正文，可继续修改或直接发布', type: 'success' };
            this.polishResult = null;
        },

        /* ============================ 批量审核 ============================ */
        async batchAudit() {
            this.batchLoading = true;
            this.batchResult = null;
            try {
                const res = await axios.post(API + '/ai/audit/batch', {
                    status: 'draft', limit: 10, concurrency: 4,
                }, { headers: this.authHeaders() });
                this.batchResult = res.data;
                this.loadMine(1);
            } catch (e) {
                this.batchResult = { items: [], total: 0, message: this.errText(e, '批量审核失败') };
            }
            this.batchLoading = false;
        },

        /* ============================ 审核实验室 ============================ */
        fillSample(kind) {
            if (kind === 'ad') {
                this.auditForm.title = '低价货源推荐';
                this.auditForm.content = '最近发现一个超低价货源渠道，需要的朋友加微信 abc12345 详聊，' +
                    '兼职日结，扫码进群还能免费领取试用装，机不可失！';
            } else if (kind === 'long') {
                this.auditForm.title = '我的社区创作心得（长文）';
                this.auditForm.content = Array.from({ length: 40 }, (_, i) =>
                    `第 ${i + 1} 段：这一部分讲述我在社区创作中的经验与思考，` +
                    '内容创作需要持续输出与复盘，才能逐步形成自己的风格。').join('\n') +
                    '\n最后补充一句：想要合作的朋友可以加微信详聊。';
            } else {
                this.auditForm.title = '如何理解 Go 的并发模型';
                this.auditForm.content = 'Go 通过 goroutine 与 channel 提供了轻量级并发原语。' +
                    'goroutine 由运行时调度，初始栈很小；channel 提供类型安全的通信机制。' +
                    '实践中常用 worker pool 控制并发度，用 context 控制生命周期。';
            }
        },

        // POST + SSE：EventSource 不支持 POST，因此手动解析流式响应
        async postAuditStream(url, body) {
            const res = await fetch(url, {
                method: 'POST',
                headers: Object.assign({ 'Content-Type': 'application/json' }, this.authHeaders()),
                body: JSON.stringify(body),
            });
            if (!res.ok) {
                let detail = 'HTTP ' + res.status;
                try {
                    const j = await res.json();
                    detail = j.error || j.detail || detail;
                } catch (e) { /* 忽略 */ }
                throw { response: { data: { error: detail } } };
            }

            const reader = res.body.getReader();
            const decoder = new TextDecoder('utf-8');
            let buffer = '';
            let finalResult = null;
            let streamError = null;

            for (;;) {
                const { done, value } = await reader.read();
                if (done) break;
                buffer += decoder.decode(value, { stream: true });

                let idx;
                while ((idx = buffer.indexOf('\n\n')) >= 0) {
                    const raw = buffer.slice(0, idx);
                    buffer = buffer.slice(idx + 2);
                    if (!raw || raw.startsWith(':')) continue;

                    let event = 'message';
                    let data = '';
                    raw.split('\n').forEach((line) => {
                        if (line.startsWith('event:')) event = line.slice(6).trim();
                        else if (line.startsWith('data:')) data += line.slice(5).trim();
                    });
                    if (!data) continue;

                    let payload;
                    try { payload = JSON.parse(data); } catch (e) { continue; }

                    if (event === 'progress') {
                        this.liveEvents.push(payload);
                        this.liveAudit.elapsed = payload.elapsed_ms;
                    } else if (event === 'result') {
                        finalResult = payload;
                    } else if (event === 'error') {
                        streamError = payload.error;
                    }
                }
            }
            if (streamError) throw { response: { data: { error: streamError } } };
            if (!finalResult) throw { response: { data: { error: '未收到审核结果' } } };
            return finalResult;
        },

        async runAuditStream() {
            if (!this.auditForm.content.trim()) {
                this.auditResult = null;
                return;
            }
            this.auditLoading = true;
            this.liveEvents = [];
            this.auditResult = null;
            try {
                this.auditResult = await this.postAuditStream(API + '/ai/audit/stream', Object.assign({}, this.auditForm));
                this.loadAuditStats();
            } catch (e) {
                this.auditResult = {
                    verdict: 'review', risk_score: 0, dims: [], stages: [], hits: [], dimensions: [],
                    reason: this.errText(e, '审核失败（请确认已登录且 AI 服务可用）'),
                };
            }
            this.auditLoading = false;
        },
    },

    mounted() {
        window.addEventListener('hashchange', this.onRoute);
        this.onRoute();
        this.loadBoards();
        this.loadHealth();
        this.loadAuditInfo();
        if (this.token) {
            this.loadMe();
            this.loadAuditStats();
        }
    },
}).mount('#app');
