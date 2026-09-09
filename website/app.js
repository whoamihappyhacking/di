'use strict';
const root = document.querySelector('#playground');
const sessions = [
  { name: 'api-server', path: '~/projects/api-server', command: 'go test ./...', preview: '$ go test ./...\nok  api-server/internal/config\nok  api-server/internal/store\nRUN integration tests...', lines: [['dim', '# 持续运行的工作，不必一直守着。'], ['green', 'ok   api-server/internal/config   0.012s'], ['green', 'ok   api-server/internal/store    0.084s'], ['green', 'ok   api-server/internal/router   0.031s'], ['amber', 'RUN  integration tests...']] },
  { name: 'my-agent', path: '~/projects/di', command: 'codex', preview: '╭─ Codex · ~/projects/di ─────────╮\n│ Working on your next idea.     │\n╰───────────────────────────────╯\n› 正在阅读项目，准备下一步修改…', lines: [['dim', '# 留给 Agent 一点时间。'], ['green', '› Reading project files...'], ['', '  main.go'], ['', '  main_test.go'], ['amber', '› Preparing the next change...']] },
  { name: 'dev-server', path: '~/projects/website', command: 'npm run dev', preview: '$ npm run dev\n\n  DEV SERVER  ready\n  ➜ Local: http://localhost:5173/\n  Watching for file changes...', lines: [['dim', '> website@dev'], ['green', 'DEV SERVER  ready'], ['', '➜ Local: http://localhost:5173/'], ['', ''], ['amber', 'Watching for file changes...']] }
];
let mode = 'attached';
let selected = 0;
let progress = 38;
const action = document.querySelector('#demo-action');
const announcement = document.querySelector('#demo-announcement');
function updateProgress() {
  document.querySelector('#task-progress').style.width = `${progress}%`;
  document.querySelector('#task-percent').textContent = `${progress}%`;
  document.querySelector('#background-progress').textContent = `${progress}%`;
}
function renderSession() {
  const session = sessions[selected];
  document.querySelector('#window-title').textContent = `${session.path} — ${session.command}`;
  document.querySelector('.session-tag').textContent = `SESSION 0${selected + 1}`;
  document.querySelector('#terminal-directory').textContent = session.path;
  document.querySelector('#terminal-command').textContent = `d ${session.command}`;
  document.querySelector('#preview-name').textContent = session.name;
  document.querySelector('#preview-content').textContent = session.preview;
  const output = document.querySelector('#terminal-output');
  output.replaceChildren(...session.lines.map(([style, line]) => { const p = document.createElement('p'); p.className = style; p.textContent = line || '\u00a0'; return p; }));
  document.querySelectorAll('[data-session]').forEach(button => { const active = Number(button.dataset.session) === selected; button.classList.toggle('selected', active); button.setAttribute('aria-pressed', String(active)); });
}
function render() {
  document.querySelector('#attached-view').hidden = mode !== 'attached';
  document.querySelector('#detached-view').hidden = mode !== 'detached';
  document.querySelector('#picker-view').hidden = mode !== 'picking';
  document.querySelector('#connection-badge').textContent = {attached:'ATTACHED', detached:'DETACHED', picking:'SESSION PICKER'}[mode];
  document.querySelectorAll('[data-step]').forEach(step => step.classList.toggle('active', step.dataset.step === mode));
  const states = {
    attached: ['断开连接', 'Ctrl-]', '试着断开连接，看看会发生什么。', 'Ctrl + ]'],
    detached: ['选择会话', 'di ↵', '后台仍在运行。输入 di，找回你的会话。', 'di'],
    picking: ['重新连接', 'Enter ↵', '预览最新终端画面，确认后重新连接。', 'Enter']
  };
  const [label, shortcut, hint, key] = states[mode];
  action.replaceChildren(document.createTextNode(label));
  const kbd = document.createElement('kbd'); kbd.textContent = shortcut; action.append(kbd);
  document.querySelector('#terminal-hint').textContent = hint;
  document.querySelector('#key-hint').textContent = key;
  renderSession(); updateProgress();
}
function advance() {
  if (mode === 'attached') { mode = 'detached'; announcement.textContent = '已断开客户端。后台进度继续变化，点击“选择会话”回来。'; }
  else if (mode === 'detached') { mode = 'picking'; announcement.textContent = '点击会话切换预览，再按 Enter 或“重新连接”。'; }
  else { mode = 'attached'; announcement.textContent = `已重新进入 ${sessions[selected].name}，继续原来的会话。`; }
  render();
}
action.addEventListener('click', advance);
document.querySelector('#reset-demo').addEventListener('click', () => { mode = 'attached'; selected = 0; progress = 38; announcement.textContent = '演示已重置。点击“断开连接”重新体验。'; render(); });
document.querySelectorAll('[data-session]').forEach(button => button.addEventListener('click', () => { selected = Number(button.dataset.session); mode = 'picking'; announcement.textContent = `正在预览 ${sessions[selected].name}。点击“重新连接”进入此会话。`; render(); }));
root.addEventListener('keydown', event => {
  if (event.repeat) return;
  if (event.ctrlKey && (event.key === ']' || event.code === 'BracketRight') && mode === 'attached') { event.preventDefault(); advance(); }
  else if (event.key === 'Enter' && mode === 'picking' && !event.target.closest('#demo-action, #reset-demo')) { event.preventDefault(); advance(); }
  else if ((event.key === 'ArrowDown' || event.key === 'ArrowUp') && mode === 'picking') {
    event.preventDefault(); selected = (selected + (event.key === 'ArrowDown' ? 1 : sessions.length - 1)) % sessions.length; renderSession();
    root.focus({preventScroll:true});
  }
});
setInterval(() => { if (!document.hidden && progress < 96) { progress += 1; updateProgress(); } }, 1800);
async function copyText(value) {
  try { if (navigator.clipboard && window.isSecureContext) { await navigator.clipboard.writeText(value); return true; } } catch (_) {}
  const textarea = document.createElement('textarea'); textarea.value = value; textarea.style.position = 'fixed'; textarea.style.opacity = '0'; document.body.append(textarea); textarea.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (_) {}
  textarea.remove(); return ok;
}
document.querySelectorAll('[data-copy], [data-command]').forEach(button => button.addEventListener('click', async () => {
  const command = button.dataset.command || document.getElementById(button.dataset.copy).textContent;
  const copied = await copyText(command); button.focus({preventScroll:true});
  if (button.dataset.copy) { button.textContent = copied ? '已复制 ✓' : '请选中命令复制'; setTimeout(() => { button.innerHTML = '复制命令 <span>⧉</span>'; }, 2200); }
  else document.querySelector('#copy-status').textContent = copied ? `已复制：${command}` : '复制未成功，请手动选中命令复制。';
}));
