/**
 * 通过 Chrome DevTools Protocol 检查真实浏览器里页面的渲染结果与控制台错误。
 * 用法: node deploy/inspect.mjs <页面URL> [等待毫秒]
 */

const url = process.argv[2] || 'http://127.0.0.1:8080/';
const waitMs = Number(process.argv[3] || 6000);

const listRes = await fetch('http://127.0.0.1:9222/json/list');
const targets = await listRes.json();
const page = targets.find((t) => t.type === 'page');
if (!page) {
  console.error('未找到可用的页面目标');
  process.exit(1);
}

const ws = new WebSocket(page.webSocketDebuggerUrl);
let msgId = 0;
const pending = new Map();
const consoleLogs = [];
const exceptions = [];

function send(method, params = {}) {
  const id = ++msgId;
  ws.send(JSON.stringify({ id, method, params }));
  return new Promise((resolve) => pending.set(id, resolve));
}

ws.addEventListener('message', (ev) => {
  const msg = JSON.parse(ev.data);
  if (msg.id && pending.has(msg.id)) {
    pending.get(msg.id)(msg.result);
    pending.delete(msg.id);
    return;
  }
  if (msg.method === 'Runtime.consoleAPICalled') {
    const text = (msg.params.args || [])
      .map((a) => a.value ?? a.description ?? a.type)
      .join(' ');
    consoleLogs.push(`[${msg.params.type}] ${text}`);
  }
  if (msg.method === 'Runtime.exceptionThrown') {
    const d = msg.params.exceptionDetails;
    exceptions.push(d.exception?.description || d.text || JSON.stringify(d));
  }
});

await new Promise((resolve) => ws.addEventListener('open', resolve));
await send('Runtime.enable');
await send('Page.enable');

await send('Page.navigate', { url });
await new Promise((r) => setTimeout(r, waitMs));

const evaluate = async (expression) => {
  const res = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
  return res?.result?.value;
};

const report = await evaluate(`(() => {
  const q = (s) => document.querySelectorAll(s).length;
  const firstRow = document.querySelector('.thread-row');
  const panel = document.querySelector('.panel');
  const out = {
    title: document.title,
    vueMounted: !!document.querySelector('#app').__vue_app__,
    counts: {
      'thread-row': q('.thread-row'),
      'board-cell': q('.board-cell'),
      'side-card': q('.side-card'),
      'panel': q('.panel'),
      'empty': q('.empty'),
    },
    listToolbarText: (document.querySelector('.list-toolbar') || {}).innerText || '',
    firstRowHTML: firstRow ? firstRow.outerHTML.slice(0, 500) : null,
    firstRowRect: firstRow ? (() => { const r = firstRow.getBoundingClientRect(); return { w: Math.round(r.width), h: Math.round(r.height), top: Math.round(r.top) }; })() : null,
    firstRowStyle: firstRow ? (() => { const c = getComputedStyle(firstRow); return { display: c.display, visibility: c.visibility, opacity: c.opacity, height: c.height, overflow: c.overflow }; })() : null,
    bodyHeight: document.body.scrollHeight,
    viewportHeight: window.innerHeight,
  };
  // 找出所有处于隐藏状态的候选元素
  out.hiddenRows = [...document.querySelectorAll('.thread-row, [class*=thread]')]
    .map((el) => {
      const c = getComputedStyle(el);
      const r = el.getBoundingClientRect();
      return { cls: el.className, h: Math.round(r.height), w: Math.round(r.width), display: c.display, color: c.color };
    })
    .slice(0, 6);
  return JSON.stringify(out, null, 2);
})()`);

console.log('===== 页面状态 =====');
console.log(report);
console.log('\n===== 控制台输出 =====');
console.log(consoleLogs.length ? consoleLogs.join('\n') : '(无)');
console.log('\n===== JS 异常 =====');
console.log(exceptions.length ? exceptions.join('\n---\n') : '(无)');

ws.close();
