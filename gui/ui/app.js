'use strict';

// ---------- helpers ----------

const $ = (selector, root = document) => root.querySelector(selector);

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value == null || value === false) continue;
    if (key === 'class') el.className = value;
    else if (key.startsWith('on')) el.addEventListener(key.slice(2), value);
    else el.setAttribute(key, value === true ? '' : value);
  }
  for (const child of children.flat()) {
    if (child == null || child === false) continue;
    el.append(child instanceof Node ? child : String(child));
  }
  return el;
}

async function api(method, url, body) {
  const options = { method, headers: { 'X-UAG': '1' } };
  if (body instanceof FormData) options.body = body;
  else if (body !== undefined) {
    options.headers['Content-Type'] = 'application/json';
    options.body = JSON.stringify(body);
  }
  const response = await fetch(url, options);
  const type = response.headers.get('Content-Type') || '';
  const data = type.includes('application/json') ? await response.json() : await response.text();
  if (!response.ok) throw new Error(typeof data === 'string' ? data || response.statusText : data.error);
  return data;
}

const query = params => new URLSearchParams(
  Object.entries(params).filter(([, value]) => value != null && value !== ''),
).toString();

const extension = path => (path.match(/\.([^./]+)$/) || [, ''])[1].toLowerCase();
const basename = path => path.split('/').pop();
const firstLine = text => (text || '').split('\n').find(line => line.trim()) || '';
const formatSize = bytes => bytes < 1024 ? `${bytes} B` : bytes < 1 << 20 ? `${(bytes / 1024).toFixed(0)} KB` : `${(bytes / (1 << 20)).toFixed(1)} MB`;
const formatCount = n => n < 1000 ? String(n) : n < 1e6 ? `${(n / 1e3).toFixed(1)}k` : `${(n / 1e6).toFixed(2)}M`;

// Inside the native window, bound Go functions stand in for browser features.
const NATIVE = typeof window.uagOpenExternal === 'function';

const IMAGE = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'svg', 'bmp', 'ico', 'avif']);
const VIDEO = new Set(['mp4', 'webm', 'mov', 'm4v']);
const AUDIO = new Set(['mp3', 'wav', 'ogg', 'm4a', 'flac']);
const DOCS = new Set(['docx', 'docm', 'pptx', 'pptm', 'xlsx', 'xlsm', 'odt', 'ods', 'odp', 'doc', 'rtf']);

// ---------- state ----------

const S = {
  state: null,
  session: null,
  events: null,
  lastSeq: 0,
  calls: new Map(),
  running: false,
  attachments: [],
  filesDir: '',
  previewPath: null,
  usage: { input: 0, output: 0, cached: 0 },
  models: {},
};

const sessionMeta = () => S.state?.sessions.find(s => s.id === S.session);
const workspaceRoot = () => sessionMeta()?.workspace || S.state?.config.workspace || '';
const shortPath = path => {
  const home = S.state?.home;
  return home && path.startsWith(home) ? '~' + path.slice(home.length) : path;
};
const wsQuery = extra => query({ session: S.session, ...extra });
const rawURL = (path, extra = {}) => `/api/raw?${wsQuery({ path, ...extra })}`;

// Resolve a link from agent output to a workspace-relative path.
function workspacePath(href) {
  let path = href.replace(/^file:\/\//, '').split('#')[0].split('?')[0];
  try { path = decodeURIComponent(path); } catch { /* keep as-is */ }
  const root = workspaceRoot();
  if (root && path.startsWith(root + '/')) path = path.slice(root.length + 1);
  return path.replace(/^\.\//, '');
}

// ---------- markdown ----------

marked.use({ gfm: true });
DOMPurify.addHook('afterSanitizeAttributes', node => {
  if (node.tagName === 'A') {
    const href = node.getAttribute('href') || '';
    if (/^(https?:|mailto:)/i.test(href)) {
      node.setAttribute('target', '_blank');
      node.setAttribute('rel', 'noopener noreferrer');
    } else if (href && !href.startsWith('#')) {
      node.setAttribute('data-file', workspacePath(href));
    }
  } else if (node.tagName === 'IMG') {
    const src = node.getAttribute('src') || '';
    if (src && !/^(https?:|data:|blob:|\/api\/)/i.test(src)) node.setAttribute('src', rawURL(workspacePath(src)));
  }
});

function markdown(text, className = 'msg assistant') {
  const el = h('div', { class: className });
  el.innerHTML = DOMPurify.sanitize(marked.parse(text || ''));
  return el;
}

// ---------- transcript ----------

let scrollQueued = false;
function nearBottom() {
  const scroller = $('#scroll');
  return scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 120;
}
function scrollToBottom() {
  if (scrollQueued) return;
  scrollQueued = true;
  requestAnimationFrame(() => {
    scrollQueued = false;
    $('#scroll').scrollTop = $('#scroll').scrollHeight;
  });
}
function append(el) {
  const stick = nearBottom();
  $('#log').append(el);
  $('#empty').hidden = true;
  if (stick) scrollToBottom();
}

function onItem(item) {
  const data = item.Data;
  switch (item.Kind) {
    case 'input':
      if (data.Kind === 'external') addUser(typeof data.Payload === 'string' ? data.Payload : JSON.stringify(data.Payload));
      else if (data.Kind === 'control' && data.Payload?.Mode === 'hard') append(h('div', { class: 'note' }, 'Stopped'));
      break;
    case 'model_response':
      addResponse(data.Response);
      break;
    case 'tool_call_status':
      updateCall(data);
      break;
  }
}

function splitAttachments(text) {
  const index = text.lastIndexOf('[Attached files]\n');
  if (index < 0 && !text.startsWith('[Attached files]')) return [text, []];
  const start = index < 0 ? 0 : index;
  const paths = text.slice(start).split('\n').slice(1)
    .map(line => line.match(/^- (.+?)(?: \(.*\))?$/)?.[1]).filter(Boolean);
  return [text.slice(0, start).trim(), paths];
}

function attachmentChip(path, onRemove) {
  return h('span', { class: 'chip', title: path },
    IMAGE.has(extension(path)) ? h('img', { src: rawURL(path), alt: '' }) : h('span', {}, '📄'),
    h('button', { type: 'button', class: 'link name', onclick: () => preview(path) }, basename(path)),
    onRemove && h('button', { type: 'button', class: 'x', title: 'Remove', onclick: onRemove }, '×'));
}

function addUser(text) {
  const [body, paths] = splitAttachments(text);
  append(h('div', { class: 'msg user' },
    body && h('div', { class: 'bubble' }, body),
    paths.length > 0 && h('div', { class: 'chips' }, paths.map(path => attachmentChip(path)))));
}

function addResponse(response) {
  for (const output of response.Output || []) {
    const value = output.Data;
    if (output.Type === 'reasoning' && value.Summary?.length) {
      const details = h('details', { class: 'reasoning' }, h('summary', {}, 'Thinking'));
      details.append(markdown(value.Summary.join('\n\n'), 'body'));
      append(details);
    } else if (output.Type === 'message' && value.Text) {
      append(markdown(value.Text));
    } else if (output.Type === 'tool_call') {
      addCall(value);
    }
  }
  if (response.Failure) append(h('div', { class: 'error-box' }, `${response.Failure.Code || 'Model error'}: ${response.Failure.Message}`));
  if (response.Stop === 'max_output_tokens') append(h('div', { class: 'note' }, 'The response hit the output token limit.'));
  if (response.Stop === 'refused') append(h('div', { class: 'note' }, 'The model refused this request.'));
  const usage = response.Usage || {};
  S.usage.input += usage.InputTokens || 0;
  S.usage.output += usage.OutputTokens || 0;
  S.usage.cached += usage.CachedInputTokens || 0;
  renderUsage();
}

function describeCall(name, args) {
  if (name === 'Bash') {
    const command = args.command || '';
    const helper = command.match(/^\s*"?\$\{?UAG\}?"?\s+(search|fetch|read)\s+([\s\S]*)$/);
    if (helper) {
      const rest = helper[2].trim().replace(/^(?:-\w+(?:[ =]\S+)?\s+)*/, '');
      const quoted = rest.match(/^(["'])(.*?)\1/);
      return { label: helper[1], text: quoted ? quoted[2] + rest.slice(quoted[0].length) : rest };
    }
    return { label: 'shell', text: command };
  }
  if (name === 'ViewImage') return { label: 'image', text: args.path || '' };
  if (name === 'SkillUse') return { label: 'skill', text: args.name || '' };
  return { label: name, text: JSON.stringify(args) };
}

function addCall(call) {
  let args = {};
  try { args = JSON.parse(call.Arguments || '{}'); } catch { args = { raw: call.Arguments }; }
  const { label, text } = describeCall(call.Name, args);
  const state = h('span', { class: 'tstate running' }, 'running');
  const body = h('div', { class: 'tbody' });
  if (call.Name === 'Bash') body.append(h('pre', {}, '$ ' + (args.command || '')));
  const el = h('details', { class: 'tool' },
    h('summary', {}, h('span', { class: 'tname' }, label), h('code', { class: 'targ', title: text }, firstLine(text)), state),
    body);
  S.calls.set(call.CallID, { el, state, body, done: false });
  append(el);
}

function setCallState(call, className, label) {
  call.state.className = `tstate ${className}`;
  call.state.textContent = label;
}

function finishCall(call, ok, label, text) {
  call.done = true;
  setCallState(call, ok ? 'done' : 'error', label);
  if (text) call.body.append(h('pre', {}, text));
  scheduleFilesRefresh();
}

function updateCall(data) {
  const call = S.calls.get(data.CallID);
  if (!call || call.done) return;
  const status = data.Status || {};
  if (status.Error) return finishCall(call, false, 'error', status.Error);
  const operations = data.Operations || [];
  const op = operations.find(o => (status.WaitingFor || []).includes(o.ID)) || operations[0];
  if (!op) return;
  if (['ready', 'awaiting', 'canceling'].includes(op.Status)) {
    setCallState(call, 'running', op.Status === 'canceling' ? 'canceling' : 'running');
    return;
  }
  const state = op.State || {};
  if (op.Type === 'shell') {
    const result = state.Result;
    let output = result ? [result.Out, result.Err].filter(Boolean).join(result.Out && result.Err ? '\n' : '') : '';
    if (!result && state.OutPath) output = `Output saved to ${state.OutPath}`;
    const failure = state.TerminalError || (op.Status !== 'completed' ? `shell ${op.Status}` : '');
    const code = result ? result.ExitCode : null;
    const label = failure ? (op.Status === 'canceled' ? 'canceled' : 'failed') : code === 0 ? 'done' : `exit ${code}`;
    finishCall(call, !failure && code === 0, label, [output || (failure ? '' : '(no output)'), failure].filter(Boolean).join('\n'));
  } else if (op.Type === 'view_image') {
    const result = state.Result || {};
    if (op.Status === 'completed' && result.Content) {
      finishCall(call, true, 'done');
      call.body.append(h('img', { src: `data:${result.EncodedMIMEType};base64,${result.Content}`, alt: '' }));
    } else finishCall(call, false, 'failed', result.Error || op.Status);
  } else {
    const ok = op.Status === 'completed';
    finishCall(call, ok, ok ? 'done' : op.Status, state.TerminalError || '');
  }
}

function renderUsage() {
  const { input, output, cached } = S.usage;
  $('#usage').textContent = input || output
    ? `${formatCount(input)} in (${input ? Math.round(cached / input * 100) : 0}% cached) · ${formatCount(output)} out`
    : '';
}

// ---------- sessions ----------

function openSession(id) {
  if (S.events) S.events.close();
  S.events = null;
  S.session = id;
  S.lastSeq = 0;
  S.calls.clear();
  S.usage = { input: 0, output: 0, cached: 0 };
  S.running = false;
  renderUsage();
  $('#log').replaceChildren();
  $('#working').hidden = true;
  $('#stop').hidden = true;
  hideBanner();
  S.filesDir = '';
  closePreview();
  renderSessions();
  renderTitle();
  loadFiles();
  history.replaceState(null, '', id ? `#${id}` : location.pathname);
  if (!id) {
    localStorage.removeItem('uag.session');
    $('#empty').hidden = false;
    $('#input').focus();
    return;
  }
  localStorage.setItem('uag.session', id);
  $('#empty').hidden = true;
  const events = new EventSource(`/api/sessions/${id}/events`);
  S.events = events;
  events.addEventListener('item', event => {
    const item = JSON.parse(event.data);
    if (item.Sequence <= S.lastSeq) return;
    S.lastSeq = item.Sequence;
    onItem(item);
  });
  events.addEventListener('status', event => setStatus(JSON.parse(event.data)));
}

function setStatus({ running, error }) {
  const changed = S.running !== running;
  S.running = running;
  $('#stop').hidden = !running;
  $('#working').hidden = !running;
  if (running && nearBottom()) scrollToBottom();
  if (error) showBanner(error, true);
  else if (!$('#banner').dataset.sticky) hideBanner();
  const meta = sessionMeta();
  if (meta && meta.running !== running) {
    meta.running = running;
    renderSessions();
  }
  if (changed && !running) {
    scheduleFilesRefresh();
    refreshState();
  }
}

function renderSessions() {
  const sessions = S.state?.sessions || [];
  $('#sessions').replaceChildren(...sessions.map(session => h('div', {
    class: 'sess' + (session.id === S.session ? ' active' : ''),
    title: `${session.title}\n${shortPath(session.workspace)}`,
    onclick: () => session.id !== S.session && openSession(session.id),
  },
  h('span', { class: 'sess-title' }, session.title || 'Untitled'),
  session.running && h('span', { class: 'dot', title: 'Working' }),
  h('button', { class: 'icon del', title: 'Delete chat', onclick: event => { event.stopPropagation(); deleteSession(session, event.currentTarget); } }, '×'))));
}

function renderTitle() {
  const meta = sessionMeta();
  $('#title-text').textContent = meta ? meta.title || 'Untitled' : 'New chat';
  $('#title-ws').textContent = shortPath(workspaceRoot());
  $('#ws-change').textContent = shortPath(S.state?.config.workspace || '');
}

// Deleting takes a second click; the native window has no confirm().
async function deleteSession(session, button) {
  if (button.dataset.armed !== '1') {
    button.dataset.armed = '1';
    button.textContent = 'Delete';
    button.classList.add('danger');
    button.title = 'Click again to delete this chat (workspace files are kept)';
    setTimeout(() => {
      delete button.dataset.armed;
      button.textContent = '×';
      button.classList.remove('danger');
    }, 3000);
    return;
  }
  try {
    await api('DELETE', `/api/sessions/${session.id}`);
    if (session.id === S.session) openSession(null);
    await refreshState();
  } catch (error) { showBanner(error.message); }
}

async function refreshState() {
  S.state = await api('GET', '/api/state');
  renderSessions();
  renderTitle();
  renderToolbar();
}

// ---------- composer ----------

async function send(event) {
  event?.preventDefault();
  const input = $('#input');
  const text = input.value.trim();
  if (!text && S.attachments.length === 0) return;
  const attachments = S.attachments;
  input.value = '';
  S.attachments = [];
  autosize();
  renderAttachments();
  hideBanner();
  const restore = message => {
    if (!input.value) input.value = text;
    S.attachments = attachments.concat(S.attachments);
    renderAttachments();
    autosize();
    showBanner(message);
  };
  try {
    const result = await api('POST', `/api/sessions/${S.session || 'new'}/messages`, {
      text, attachments: attachments.map(a => a.path),
    });
    await refreshState();
    if (result.session.id !== S.session) openSession(result.session.id);
    if (result.error) restore(result.error);
  } catch (error) { restore(error.message); }
}

async function upload(files) {
  files = [...files];
  if (files.length === 0) return;
  const form = new FormData();
  for (const file of files) form.append('file', file, file.name || `pasted-${Date.now()}.png`);
  try {
    const result = await api('POST', `/api/upload?${wsQuery()}`, form);
    S.attachments.push(...result.files);
    renderAttachments();
    scheduleFilesRefresh();
  } catch (error) { showBanner(`Upload failed: ${error.message}`); }
}

function renderAttachments() {
  $('#attachments').replaceChildren(...S.attachments.map((attachment, index) => attachmentChip(attachment.path, () => {
    S.attachments.splice(index, 1);
    renderAttachments();
  })));
}

function autosize() {
  const input = $('#input');
  input.style.height = 'auto';
  input.style.height = Math.min(input.scrollHeight, window.innerHeight * 0.4) + 'px';
}

function showBanner(text, retry = false) {
  $('#banner').hidden = false;
  $('#banner-text').textContent = text;
  $('#banner-retry').hidden = !retry || !S.session;
  if (!retry) $('#banner').dataset.sticky = '1';
}
function hideBanner() {
  $('#banner').hidden = true;
  delete $('#banner').dataset.sticky;
}

// ---------- toolbar ----------

function currentProvider() {
  return S.state.providers.find(p => p.name === S.state.config.provider) || S.state.providers[0];
}

function renderToolbar() {
  const { config, providers, thinking } = S.state;
  const provider = currentProvider();
  const select = $('#provider');
  if (select.options.length !== providers.length) {
    select.replaceChildren(...providers.map(p => h('option', { value: p.name }, p.label)));
  }
  select.value = provider.name;
  const levels = $('#thinking');
  if (levels.options.length !== thinking.length) {
    levels.replaceChildren(...thinking.map(level => h('option', { value: level }, `think: ${level}`)));
  }
  levels.value = config.thinking;
  const model = $('#model');
  if (document.activeElement !== model) model.value = config.models[provider.name] || '';
  model.placeholder = provider.default_model || 'model id';
  const web = $('#websearch');
  web.checked = config.web_search && provider.hosted_search;
  web.disabled = !provider.hosted_search;
  $('#websearch-label').classList.toggle('disabled', !provider.hosted_search);
  $('#websearch-label').title = provider.hosted_search
    ? 'Provider-hosted web search. The web-research skill works either way.'
    : `${provider.label} has no hosted search; the agent uses the web-research skill instead.`;
  loadModels(provider);
}

async function loadModels(provider) {
  if (!provider.list_models || S.models[provider.name]) {
    fillModels(S.models[provider.name] || []);
    return;
  }
  S.models[provider.name] = [];
  try {
    const result = await api('GET', `/api/models?${query({ provider: provider.name })}`);
    S.models[provider.name] = result.models;
  } catch { /* the model field still accepts any id */ }
  if (currentProvider().name === provider.name) fillModels(S.models[provider.name]);
}

function fillModels(models) {
  $('#model-list').replaceChildren(...models.map(id => h('option', { value: id })));
}

async function saveConfig(update) {
  try {
    S.state.config = await api('PUT', '/api/config', update);
    renderToolbar();
    renderTitle();
    if (S.running) showBanner('Settings apply from the next run.');
  } catch (error) { showBanner(error.message); }
}

// ---------- files ----------

let filesTimer = 0;
function scheduleFilesRefresh() {
  clearTimeout(filesTimer);
  filesTimer = setTimeout(() => loadFiles(S.filesDir, true), 700);
}

async function loadFiles(dir = S.filesDir, quiet = false) {
  const list = $('#file-list');
  try {
    const result = await api('GET', `/api/files?${wsQuery({ dir })}`);
    S.filesDir = result.dir;
    $('#files-path').textContent = result.dir ? `${shortPath(result.root)}/${result.dir}` : shortPath(result.root);
    $('#files-path').title = result.root + (result.dir ? '/' + result.dir : '');
    $('#files-up').disabled = !result.dir;
    list.replaceChildren(...(result.entries.length ? result.entries.map(entry => h('div', {
      class: 'file' + (entry.dir ? ' dir' : '') + (entry.path === S.previewPath ? ' active' : ''),
      title: entry.path,
      onclick: () => entry.dir ? loadFiles(entry.path) : preview(entry.path),
    }, h('span', { class: 'fname' }, (entry.dir ? '▸ ' : '') + entry.name),
    h('span', { class: 'muted small' }, entry.dir ? '' : formatSize(entry.size)))) : [h('div', { class: 'muted pad small' }, 'Empty folder. Files the agent creates show up here.')]));
  } catch (error) {
    if (!quiet) list.replaceChildren(h('div', { class: 'muted pad small' }, error.message));
  }
}

function closePreview() {
  S.previewPath = null;
  $('#preview').hidden = true;
  $('#preview-body').replaceChildren();
  $('#files').classList.remove('previewing', 'wide');
}

function parseDelimited(text, delimiter) {
  const rows = [];
  let row = [], field = '', quoted = false;
  for (let i = 0; i < text.length && rows.length < 1000; i++) {
    const c = text[i];
    if (quoted) {
      if (c === '"' && text[i + 1] === '"') { field += '"'; i++; }
      else if (c === '"') quoted = false;
      else field += c;
    } else if (c === '"') quoted = true;
    else if (c === delimiter) { row.push(field); field = ''; }
    else if (c === '\n') { row.push(field.replace(/\r$/, '')); rows.push(row); row = []; field = ''; }
    else field += c;
  }
  if (field || row.length) { row.push(field); rows.push(row); }
  return rows;
}

async function preview(path) {
  S.previewPath = path;
  document.body.classList.remove('hide-files');
  $('#preview').hidden = false;
  $('#files').classList.add('previewing');
  $('#preview-name').textContent = path;
  $('#preview-name').title = path;
  $('#preview-download').href = rawURL(path, { download: '1' });
  const body = $('#preview-body');
  const ext = extension(path);
  const url = rawURL(path);
  const wide = ['pdf', 'html', 'htm'].includes(ext) || DOCS.has(ext);
  $('#files').classList.toggle('wide', wide);
  body.replaceChildren(h('div', { class: 'muted pad small' }, 'Loading…'));
  for (const el of document.querySelectorAll('.file')) el.classList.toggle('active', el.title === path);
  try {
    if (IMAGE.has(ext)) body.replaceChildren(h('img', { class: 'pv', src: url, alt: path }));
    else if (ext === 'pdf') body.replaceChildren(h('iframe', { class: 'pv', src: url, title: path }));
    else if (ext === 'html' || ext === 'htm') body.replaceChildren(h('iframe', { class: 'pv', src: url, title: path, sandbox: 'allow-scripts allow-popups' }));
    else if (VIDEO.has(ext)) body.replaceChildren(h('video', { class: 'pv', src: url, controls: true }));
    else if (AUDIO.has(ext)) body.replaceChildren(h('audio', { class: 'pv', src: url, controls: true }));
    else if (DOCS.has(ext)) body.replaceChildren(h('pre', { class: 'pv' }, await api('GET', `/api/text?${wsQuery({ path })}`)));
    else {
      const response = await fetch(url);
      if (!response.ok) throw new Error((await response.text()) || response.statusText);
      const type = response.headers.get('Content-Type') || '';
      const size = Number(response.headers.get('Content-Length') || 0);
      if (!/^text\/|json|xml|javascript|yaml|toml/.test(type) && !['md', 'csv', 'tsv', 'txt', 'log'].includes(ext)) {
        body.replaceChildren(h('div', { class: 'muted pad small' }, `No preview for ${type || 'this file'}. Use ⧉ to open it in its default app.`));
        return;
      }
      if (size > 5 << 20) {
        body.replaceChildren(h('div', { class: 'muted pad small' }, `Too large to preview (${formatSize(size)}).`));
        return;
      }
      const text = await response.text();
      if (S.previewPath !== path) return;
      if (ext === 'md' || ext === 'markdown') body.replaceChildren(markdown(text, 'md msg'));
      else if (ext === 'csv' || ext === 'tsv') {
        const [header = [], ...rows] = parseDelimited(text, ext === 'tsv' ? '\t' : ',');
        body.replaceChildren(h('table', { class: 'pv' },
          h('thead', {}, h('tr', {}, header.map(cell => h('th', {}, cell)))),
          h('tbody', {}, rows.map(row => h('tr', {}, row.map(cell => h('td', {}, cell)))))));
      } else if (ext === 'json') {
        let pretty = text;
        try { pretty = JSON.stringify(JSON.parse(text), null, 2); } catch { /* show raw */ }
        body.replaceChildren(h('pre', { class: 'pv' }, pretty));
      } else body.replaceChildren(h('pre', { class: 'pv' }, text));
    }
  } catch (error) {
    body.replaceChildren(h('div', { class: 'error-box' }, error.message));
  }
}

// ---------- settings ----------

async function openSettings(focusProvider) {
  const { config, providers, data_dir } = S.state;
  $('#settings-error').hidden = true;
  $('#config-path').textContent = shortPath(`${data_dir}/config.json`);
  $('#provider-fields').replaceChildren(...providers.map(p => {
    const status = config.key_status[p.name];
    const keyPlaceholder = !p.key_env ? 'no key needed'
      : status === 'saved' ? 'saved (type to replace, - to clear)'
      : status === 'env' ? `using $${p.key_env}` : `API key or $${p.key_env}`;
    return h('div', { class: 'provider-field' },
      h('label', {}, p.label),
      h('input', { type: 'password', 'data-key': p.name, placeholder: keyPlaceholder, disabled: !p.key_env, autocomplete: 'off' }),
      h('input', { 'data-base': p.name, placeholder: p.base_url, value: config.base_urls[p.name] || '', spellcheck: 'false', title: 'Base URL override' }));
  }));
  $('#instructions').value = config.instructions || '';
  $('#env').value = Object.entries(config.env).map(([name, value]) => `${name}=${value}`).join('\n');
  $('#gh-detail').textContent = 'Checking GitHub…';
  $('#settings').showModal();
  if (focusProvider) $(`[data-key="${focusProvider}"]`)?.focus();
  refreshGitHub(true);
}

function settingsError(message) {
  $('#settings-error').textContent = message;
  $('#settings-error').hidden = false;
}

async function saveSettings(event) {
  if (event.submitter?.value !== 'save') return;
  event.preventDefault();
  const env = {};
  for (const line of $('#env').value.split('\n')) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith('#')) continue;
    const at = trimmed.indexOf('=');
    if (at <= 0) { settingsError(`Expected NAME=value: ${trimmed}`); return; }
    env[trimmed.slice(0, at).trim()] = trimmed.slice(at + 1).trim();
  }
  const apiKeys = {}, baseURLs = {};
  for (const input of document.querySelectorAll('[data-key]')) if (input.value.trim()) apiKeys[input.dataset.key] = input.value.trim();
  for (const input of document.querySelectorAll('[data-base]')) baseURLs[input.dataset.base] = input.value.trim();
  try {
    S.state.config = await api('PUT', '/api/config', {
      api_keys: apiKeys, base_urls: baseURLs, env, instructions: $('#instructions').value,
    });
    S.models = {};
    $('#settings').close();
    renderToolbar();
    refreshGitHub();
  } catch (error) { settingsError(error.message); }
}

async function refreshGitHub(detail = false) {
  try {
    const gh = await api('GET', '/api/github');
    const summary = gh.connected ? `GitHub: ${gh.account || 'connected'}` : gh.installed ? 'GitHub: not logged in' : 'GitHub: gh not installed';
    $('#gh-status').textContent = summary;
    $('#gh-status').title = gh.output || '';
    if (detail) {
      $('#gh-detail').textContent = gh.connected
        ? `GitHub connected through the gh CLI as ${gh.account || 'you'}; the agent can use gh and git.`
        : gh.installed ? 'gh is installed but not logged in. Run `gh auth login` in a terminal, or add GH_TOKEN above.'
        : 'Install the GitHub CLI (brew install gh) and run `gh auth login`, or add GH_TOKEN above.';
    }
  } catch { /* optional */ }
}

// ---------- wiring ----------

function setPanels() {
  document.body.classList.toggle('hide-side', localStorage.getItem('uag.side') === '0');
  const files = localStorage.getItem('uag.files');
  document.body.classList.toggle('hide-files', files === '0' || (files === null && window.innerWidth < 1100));
}

function wire() {
  $('#composer').addEventListener('submit', send);
  const input = $('#input');
  input.addEventListener('keydown', event => {
    if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) send(event);
  });
  input.addEventListener('input', autosize);
  input.addEventListener('paste', event => {
    const files = [...(event.clipboardData?.files || [])];
    if (files.length) { event.preventDefault(); upload(files); }
  });
  $('#attach').addEventListener('click', () => $('#file-input').click());
  $('#file-input').addEventListener('change', event => { upload(event.target.files); event.target.value = ''; });
  const chat = $('#chat');
  let dragDepth = 0;
  chat.addEventListener('dragenter', event => { if (event.dataTransfer?.types.includes('Files')) { dragDepth++; chat.classList.add('dragging'); } });
  chat.addEventListener('dragleave', () => { if (--dragDepth <= 0) { dragDepth = 0; chat.classList.remove('dragging'); } });
  chat.addEventListener('dragover', event => event.preventDefault());
  chat.addEventListener('drop', event => {
    event.preventDefault();
    dragDepth = 0;
    chat.classList.remove('dragging');
    upload(event.dataTransfer.files);
  });

  $('#stop').addEventListener('click', () => S.session && api('POST', `/api/sessions/${S.session}/stop`).catch(error => showBanner(error.message)));
  $('#banner-retry').addEventListener('click', () => {
    hideBanner();
    api('POST', `/api/sessions/${S.session}/retry`).catch(error => showBanner(error.message));
  });
  $('#banner-close').addEventListener('click', hideBanner);
  $('#new-chat').addEventListener('click', () => openSession(null));
  $('#ws-change').addEventListener('click', async () => {
    let path = '';
    try { path = (await api('POST', '/api/pick-folder')).path; }
    catch (error) {
      if (NATIVE) return showBanner(error.message);
      path = prompt('Workspace folder for new chats', S.state.config.workspace) || '';
    }
    if (path) {
      await saveConfig({ workspace: path });
      if (!S.session) loadFiles('');
    }
  });

  $('#provider').addEventListener('change', event => saveConfig({ provider: event.target.value }));
  $('#thinking').addEventListener('change', event => saveConfig({ thinking: event.target.value }));
  $('#websearch').addEventListener('change', event => saveConfig({ web_search: event.target.checked }));
  $('#model').addEventListener('change', event => saveConfig({ models: { [currentProvider().name]: event.target.value.trim() } }));

  $('#toggle-side').addEventListener('click', () => {
    localStorage.setItem('uag.side', document.body.classList.toggle('hide-side') ? '0' : '1');
  });
  $('#toggle-files').addEventListener('click', () => {
    localStorage.setItem('uag.files', document.body.classList.toggle('hide-files') ? '0' : '1');
  });
  $('#files-up').addEventListener('click', () => loadFiles(S.filesDir.split('/').slice(0, -1).join('/')));
  $('#files-refresh').addEventListener('click', () => loadFiles());
  $('#files-reveal').addEventListener('click', () => api('POST', `/api/open?${wsQuery({ path: S.filesDir || '.' })}`).catch(error => showBanner(error.message)));
  $('#preview-close').addEventListener('click', closePreview);
  $('#preview-open').addEventListener('click', () => S.previewPath && api('POST', `/api/open?${wsQuery({ path: S.previewPath })}`).catch(error => showBanner(error.message)));

  $('#log').addEventListener('click', event => {
    const link = event.target.closest('a[data-file]');
    if (link) { event.preventDefault(); preview(link.dataset.file); }
  });
  if (NATIVE) {
    $('#preview-download').hidden = true;
    document.addEventListener('click', event => {
      const link = event.target.closest('a[href]');
      if (link && /^(https?:|mailto:)/i.test(link.getAttribute('href'))) {
        event.preventDefault();
        window.uagOpenExternal(link.href);
      }
    });
  }

  $('#open-settings').addEventListener('click', () => openSettings());
  $('#settings-form').addEventListener('submit', saveSettings);

  window.addEventListener('focus', () => refreshState().catch(() => {}));
  document.addEventListener('keydown', event => {
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') { event.preventDefault(); openSession(null); }
    if (event.key === 'Escape' && S.previewPath && !$('#settings').open) closePreview();
  });
}

async function start() {
  setPanels();
  wire();
  try {
    await refreshState();
  } catch (error) {
    showBanner(`Could not reach the GUI server: ${error.message}`);
    return;
  }
  const saved = location.hash.slice(1) || localStorage.getItem('uag.session');
  openSession(S.state.sessions.some(s => s.id === saved) ? saved : null);
  refreshGitHub();
  // First run: ask for the provider's key before the first message fails.
  const provider = currentProvider();
  if (provider.key_env && !S.state.config.key_status[provider.name]) openSettings(provider.name);
}

start();
