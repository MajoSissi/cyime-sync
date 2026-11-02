/* CyIME 剪贴板同步 · 设置页逻辑
   设置走 JSON API，事件走 SSE（断线由 EventSource 自动重连，不做轮询兜底）。 */
(() => {
  'use strict';

  const TOKEN = (document.querySelector('meta[name="cyime-token"]')?.content || '').trim();
  const IS_LOCAL = (document.querySelector('meta[name="cyime-local"]')?.content || '0').trim() === '1';

  const MAX_EVENTS = 200;
  // 与 /api/pair 的设备上限一致，仅用于列表头部 "n / 8" 提示。
  const MAX_DEVICES = 8;
  // 成功是默认结果，不再逐条标注「已同步」；只把异常状态显示出来。
  const STATUS_TEXT = {
    duplicate: '重复跳过',
    sensitive: '敏感跳过',
    oversize: '超长跳过',
    conflict: '写入冲突',
    busy: '剪贴板占用',
    invalid: '请求非法',
    empty: '内容为空',
  };
  // 方向按"本机"视角命名：推送给手机 / 从手机拉取。
  const DIR_TEXT = { out: '推送', in: '拉取' };
  const DIR_TITLE = { out: '本机剪贴板推送到手机', in: '从手机拉取到本机剪贴板' };
  // 预览右侧固定保留的尾部字符数，剩余部分由浏览器按可用宽度截断。
  const PREVIEW_TAIL = 10;

  const $ = (id) => document.getElementById(id);
  const el = {
    banner: $('remoteBanner'),
    pill: $('statusPill'),
    statusText: $('statusText'),
    master: $('masterSwitch'),
    qr: $('qr'),
    qrExpiry: $('qrExpiry'),
    refreshQR: $('refreshQR'),
    devices: $('devices'),
    devicesWrap: $('devicesWrap'),
    devicesCount: $('devicesCount'),
    devicesEmpty: $('devicesEmpty'),
    maxChars: $('maxCharsInput'),
    forceChars: $('forceCharsInput'),
    skipOTP: $('skipOTP'),
    port: $('portInput'),
    listenSelect: $('listenSelect'),
    listenError: $('listenError'),
    autoStart: $('autoStart'),
    events: $('events'),
    empty: $('emptyState'),
    clearEvents: $('clearEvents'),
    snackbar: $('snackbar'),
    rowTemplate: $('eventRow'),
  };

  let state = null;
  let events = [];
  let stream = null;
  let portChanged = false;

  // 配对窗口 2 分钟：到期前自动换一张，避免用户扫到过期码。
  let pairing = null;
  let qrDeadline = 0;
  let qrNonce = 0;
  let qrBusy = false;

  /* ---------------- 工具 ---------------- */

  async function api(path, options = {}) {
    const { method = 'GET', body } = options;
    const headers = { 'X-Cyime-Token': TOKEN };
    if (body !== undefined) headers['Content-Type'] = 'application/json';

    const res = await fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: 'no-store',
    });

    const raw = await res.text();
    let data = null;
    if (raw) {
      try {
        data = JSON.parse(raw);
      } catch {
        data = null;
      }
    }
    if (!res.ok) throw new Error(data?.message || `请求失败（HTTP ${res.status}）`);
    return data;
  }

  let snackTimer = 0;
  function snack(message, tone = 'info') {
    const node = el.snackbar;
    node.textContent = message;
    node.hidden = false;
    node.style.background = tone === 'error' ? 'var(--md-error-container)' : '';
    node.style.color = tone === 'error' ? 'var(--md-on-error-container)' : '';
    requestAnimationFrame(() => node.classList.add('show'));
    clearTimeout(snackTimer);
    snackTimer = setTimeout(() => {
      node.classList.remove('show');
      setTimeout(() => {
        node.hidden = true;
      }, 200);
    }, 3000);
  }

  function formatBytes(n) {
    if (!Number.isFinite(n) || n <= 0) return '0 B';
    return n < 1024 ? `${n} B` : `${(n / 1024).toFixed(1)} KiB`;
  }

  /* ---------------- 渲染 ---------------- */

  function render() {
    if (!state) return;
    const s = state.settings;

    el.master.setAttribute('aria-checked', String(!!s.enabled));
    el.autoStart.setAttribute('aria-checked', String(!!s.auto_start));
    el.skipOTP.setAttribute('aria-checked', String(!!s.skip_suspected_otp));

    el.pill.dataset.state = s.enabled ? 'on' : 'off';
    el.statusText.textContent = s.enabled ? '同步中' : '已停用';

    if (document.activeElement !== el.port) el.port.value = String(s.port);
    if (document.activeElement !== el.maxChars) el.maxChars.value = String(s.max_push_chars);
    if (document.activeElement !== el.forceChars) el.forceChars.value = String(s.force_push_chars);
    renderListenOptions();

    el.listenError.hidden = !state.last_error;
    el.listenError.textContent = state.last_error ? `监听失败：${state.last_error}` : '';

    el.refreshQR.disabled = !IS_LOCAL;
  }

  /* ---------------- 配对 ---------------- */

  function qrImageURL() {
    qrNonce += 1;
    return `/api/qr.png?token=${encodeURIComponent(TOKEN)}&n=${qrNonce}`;
  }

  // 二维码来自服务端的限时配对窗口。未过期时服务端会复用同一窗口（扫描中途刷新不致失效），
  // 所以「刷新」按钮要显式要求轮换，否则拿回来的还是同一份凭据、界面毫无变化。
  async function refreshQR(rotate = false) {
    if (!IS_LOCAL || qrBusy) return;
    qrBusy = true;
    try {
      const path = rotate ? '/api/pairing?rotate=1' : '/api/pairing';
      pairing = await api(path, { method: 'POST', body: {} });
      qrDeadline = Date.now() + Math.max(0, pairing.seconds_left || 0) * 1000;
      el.qr.src = qrImageURL();
      renderPairing();
      if (rotate) snack('已生成新的二维码，旧码立即失效');
    } catch {
      el.qrExpiry.textContent = '二维码不可用';
    } finally {
      qrBusy = false;
    }
  }

  function renderPairing() {
    const list = pairing?.devices || [];
    el.devices.replaceChildren();
    for (const dev of list) el.devices.appendChild(deviceRow(dev));
    el.devicesCount.textContent = list.length ? `${list.length} / ${MAX_DEVICES}` : '';
    el.devicesWrap.hidden = list.length === 0;
    el.devicesEmpty.hidden = list.length > 0;
    syncDevicesFade();
    tickPairing();
  }

  // 列表还能往下滚时给底部加渐隐，避免出现半截行被"裁坏"的观感。
  function syncDevicesFade() {
    const list = el.devices;
    const more = list.scrollHeight - list.scrollTop - list.clientHeight > 1;
    el.devicesWrap.dataset.fade = String(more);
  }

  // 一台设备一行：左侧图标 + 标题（备注优先）+ 右侧操作，整行同一块底色。
  function deviceRow(dev) {
    const row = document.createElement('div');
    row.className = 'device';

    const main = document.createElement('div');
    main.className = 'device__main';

    const name = document.createElement('span');
    name.className = 'device__name';
    name.textContent = dev.note || dev.name;
    name.title = dev.note ? `${dev.note}（${dev.name}）` : dev.name;
    main.appendChild(name);

    // 有备注时把型号降到次要位置，仍能看到是哪台机器。
    if (dev.note) {
      const model = document.createElement('span');
      model.className = 'device__model';
      model.textContent = dev.name;
      main.appendChild(model);
    }

    const actions = document.createElement('div');
    actions.className = 'device__actions';
    actions.append(noteButton(dev, row), revokeButton(dev));

    row.append(deviceAvatar(), main, actions);
    return row;
  }

  // 行首的机型图标，让整行在视觉上有起点，不与右侧按钮割裂。
  function deviceAvatar() {
    const box = document.createElement('span');
    box.className = 'device__avatar';
    box.setAttribute('aria-hidden', 'true');
    box.innerHTML =
      '<svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.7">' +
      '<rect x="6.6" y="2.9" width="10.8" height="18.2" rx="2.6"/>' +
      '<path d="M10.6 5.6h2.8" stroke-linecap="round"/>' +
      '<circle cx="12" cy="18.1" r=".85" fill="currentColor" stroke="none"/>' +
      '</svg>';
    return box;
  }

  function noteButton(dev, row) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'icon-btn';
    btn.title = '设置备注';
    btn.setAttribute('aria-label', `设置「${dev.note || dev.name}」的备注`);
    btn.innerHTML =
      '<svg viewBox="0 0 24 24" width="15" height="15" aria-hidden="true">' +
      '<path d="M4 20h4.4L19.1 9.3a2.2 2.2 0 0 0-3.1-3.1L5.3 16.9 4 20z" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linejoin="round"/>' +
      '</svg>';
    btn.addEventListener('click', () => editNote(dev, row));
    return btn;
  }

  function revokeButton(dev) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'text-btn';
    btn.textContent = '断开';
    btn.addEventListener('click', () => revokeDevice(dev));
    return btn;
  }

  // 就地编辑备注：回车保存、Esc 取消、失焦保存。
  function editNote(dev, row) {
    if (row.querySelector('.device__input')) return;

    const input = document.createElement('input');
    input.className = 'device__input';
    input.type = 'text';
    input.maxLength = 32;
    input.value = dev.note || '';
    input.placeholder = dev.name;
    input.setAttribute('aria-label', '设备备注');

    let done = false;
    const finish = async (save) => {
      if (done) return;
      done = true;
      const next = input.value.trim();
      if (!save || next === (dev.note || '')) {
        renderPairing();
        return;
      }
      try {
        pairing = await api(`/api/devices?id=${encodeURIComponent(dev.id)}`, {
          method: 'PATCH',
          body: { note: next },
        });
        renderPairing();
        snack(next ? `备注已改为「${next}」` : '备注已清空');
      } catch (err) {
        snack(err.message || '保存备注失败', 'error');
        renderPairing();
      }
    };

    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        finish(true);
      } else if (e.key === 'Escape') {
        e.preventDefault();
        finish(false);
      }
    });
    input.addEventListener('blur', () => finish(true));

    row.querySelector('.device__main').replaceChildren(input);
    input.focus();
    input.select();
  }

  function tickPairing() {
    if (!pairing) {
      el.qrExpiry.textContent = '—';
      return;
    }
    const left = Math.max(0, Math.round((qrDeadline - Date.now()) / 1000));
    const m = Math.floor(left / 60);
    const s = String(left % 60).padStart(2, '0');
    el.qrExpiry.textContent = left > 0 ? `${m}:${s} 后失效` : '已失效';
    if (left === 0 && !qrBusy && !portChanged) refreshQR();
  }

  async function revokeDevice(dev) {
    if (!confirm(`断开「${dev.name}」？该设备需要重新扫码才能同步。`)) return;
    try {
      pairing = await api(`/api/devices?id=${encodeURIComponent(dev.id)}`, { method: 'DELETE' });
      renderPairing();
      snack(`已断开 ${dev.name}`);
    } catch (err) {
      snack(err.message || '断开失败', 'error');
    }
  }

  function renderListenOptions() {
    if (!state) return;
    const current = state.settings.listen_address;
    const options = [{ value: '0.0.0.0', label: '所有网卡' }];
    for (const ip of state.addresses || []) options.push({ value: ip, label: ip });
    options.push({ value: '127.0.0.1', label: '仅本机' });
    if (!options.some((o) => o.value === current)) options.unshift({ value: current, label: current });

    const signature = options.map((o) => o.value).join('|');
    if (el.listenSelect.dataset.signature !== signature) {
      el.listenSelect.dataset.signature = signature;
      el.listenSelect.replaceChildren();
      for (const o of options) {
        const node = document.createElement('option');
        node.value = o.value;
        node.textContent = o.label;
        el.listenSelect.appendChild(node);
      }
    }
    el.listenSelect.value = current;
  }

  function setReadOnly() {
    el.banner.hidden = false;
    for (const node of document.querySelectorAll('.switch, .text-btn, input, select')) {
      node.disabled = true;
    }
  }

  /* ---------------- 事件 ---------------- */

  function eventNode(ev) {
    const frag = el.rowTemplate.content.cloneNode(true);
    const row = frag.querySelector('.event');

    const time = new Date(ev.time);
    row.querySelector('.event__time').textContent = Number.isNaN(time.getTime())
      ? '--:--:--'
      : time.toLocaleTimeString('zh-CN', { hour12: false });

    const dir = row.querySelector('.event__dir');
    dir.textContent = DIR_TEXT[ev.direction] || ev.direction;
    dir.classList.add(ev.direction === 'in' ? 'dir-in' : 'dir-out');
    if (DIR_TITLE[ev.direction]) dir.title = DIR_TITLE[ev.direction];

    const status = row.querySelector('.event__status');
    if (ev.status !== 'synced') {
      status.textContent = STATUS_TEXT[ev.status] || ev.status;
      status.classList.add(`st-${ev.status}`);
    }

    // 只显示字节数：hash 前缀对用户没有意义，排障看日志文件。
    row.querySelector('.event__meta').textContent = ev.bytes ? formatBytes(ev.bytes) : '';

    if (ev.note) row.querySelector('.event__note').textContent = ev.note;
    if (ev.preview) renderPreview(row.querySelector('.event__preview'), ev.preview);

    return frag;
  }

  // 单行预览：头部按可用宽度自动省略，尾部固定保留。
  // 长文本在窄栏里也能同时看到"开头是什么"和"结尾是什么"。
  function renderPreview(node, text) {
    const chars = Array.from(text);
    if (chars.length <= PREVIEW_TAIL * 2) {
      node.textContent = text;
      return;
    }
    const head = document.createElement('span');
    head.className = 'event__preview-head';
    head.textContent = chars.slice(0, chars.length - PREVIEW_TAIL).join('');

    const tail = document.createElement('span');
    tail.className = 'event__preview-tail';
    tail.textContent = chars.slice(-PREVIEW_TAIL).join('');

    node.replaceChildren(head, tail);
  }

  function renderEvents() {
    for (const row of el.events.querySelectorAll('.event')) row.remove();
    el.empty.hidden = events.length > 0;
    for (const ev of events) el.events.appendChild(eventNode(ev));
  }

  function prependEvent(ev) {
    if (events.some((e) => e.id === ev.id)) return;
    events.unshift(ev);
    if (events.length > MAX_EVENTS) events.length = MAX_EVENTS;
    el.empty.hidden = true;
    el.events.prepend(eventNode(ev));
    while (el.events.querySelectorAll('.event').length > MAX_EVENTS) {
      el.events.querySelector('.event:last-of-type')?.remove();
    }
  }

  /* ---------------- SSE ---------------- */

  function connectStream() {
    if (!IS_LOCAL || !TOKEN || portChanged) return;
    if (stream) stream.close();

    stream = new EventSource(`/api/events?token=${encodeURIComponent(TOKEN)}`);
    stream.addEventListener('snapshot', (e) => {
      try {
        const payload = JSON.parse(e.data);
        events = (payload.events || []).slice(0, MAX_EVENTS);
        renderEvents();
        if (payload.pairing) {
          pairing = payload.pairing;
          qrDeadline = Date.now() + Math.max(0, payload.pairing.seconds_left || 0) * 1000;
          renderPairing();
        }
      } catch {
        /* 单帧解析失败忽略 */
      }
    });
    stream.addEventListener('sync', (e) => {
      try {
        prependEvent(JSON.parse(e.data));
      } catch {
        /* 同上 */
      }
    });
    stream.addEventListener('pairing', (e) => {
      try {
        const payload = JSON.parse(e.data);
        if (payload.state) {
          pairing = payload.state;
          renderPairing();
        }
        if (payload.event === 'paired') {
          snack(`${payload.detail || '设备'} 已连接`);
          // 配对成功后窗口已关闭，取下一张二维码供下一台设备使用。
          refreshQR();
        } else if (payload.event === 'revoked') {
          snack(`已断开 ${payload.detail || '设备'}`);
        }
      } catch {
        /* 忽略单帧错误 */
      }
    });
  }

  /* ---------------- 交互 ---------------- */

  async function loadAll() {
    if (!IS_LOCAL) {
      setReadOnly();
      return;
    }

    try {
      state = await api('/api/settings');
      render();
      connectStream();
      await refreshQR();
    } catch (err) {
      snack(err.message || '读取设置失败', 'error');
    }
  }

  async function patch(body) {
    try {
      state = await api('/api/settings', { method: 'PUT', body });
      render();
      return true;
    } catch (err) {
      snack(err.message || '保存失败', 'error');
      await refresh();
      return false;
    }
  }

  async function refresh() {
    try {
      state = await api('/api/settings');
      render();
    } catch {
      /* 忽略 */
    }
  }

  function bindSwitch(node, field) {
    node.addEventListener('click', async () => {
      const next = node.getAttribute('aria-checked') !== 'true';
      node.setAttribute('aria-checked', String(next));
      await patch({ [field]: next });
    });
  }

  // number 输入框获得焦点后滚轮会直接改数值，很容易误触；聚焦时拦下滚轮并让出焦点。
  function lockNumberWheel(input) {
    input.addEventListener(
      'wheel',
      (e) => {
        if (document.activeElement !== input) return;
        e.preventDefault();
        input.blur();
      },
      { passive: false }
    );
  }

  // 两个字符上限的校验与保存逻辑完全一致，只有字段名与提示文案不同。
  function bindCharLimit(input, field, label) {
    lockNumberWheel(input);
    input.addEventListener('change', async () => {
      const value = Number(input.value);
      if (!Number.isInteger(value) || value < 1 || value > 100000) {
        snack(`${label}需在 1-100000 之间`, 'error');
        await refresh();
        return;
      }
      if (state && value === state.settings[field]) return;
      await patch({ [field]: value });
    });
  }

  function bindUI() {
    bindSwitch(el.master, 'enabled');
    bindSwitch(el.autoStart, 'auto_start');
    bindSwitch(el.skipOTP, 'skip_suspected_otp');

    el.refreshQR.addEventListener('click', () => refreshQR(true));

    el.devices.addEventListener('scroll', syncDevicesFade, { passive: true });
    window.addEventListener('resize', syncDevicesFade);

    bindCharLimit(el.maxChars, 'max_push_chars', '推送字符上限');
    bindCharLimit(el.forceChars, 'force_push_chars', '强制推送字符上限');

    el.port.addEventListener('change', async () => {
      const value = Number(el.port.value);
      if (!Number.isInteger(value) || value < 1 || value > 65535) {
        snack('端口需在 1-65535 之间', 'error');
        await refresh();
        return;
      }
      if (state && value === state.settings.port) return;
      if (await patch({ port: value })) {
        portChanged = true;
        stream?.close();
        snack(`端口已改为 ${value}，请用 http://127.0.0.1:${value} 重新打开`);
      }
    });

    el.listenSelect.addEventListener('change', async () => {
      if (await patch({ listen_address: el.listenSelect.value })) refreshQR();
    });

    el.clearEvents.addEventListener('click', async () => {
      try {
        await api('/api/events', { method: 'DELETE' });
        events = [];
        renderEvents();
        snack('已清空');
      } catch (err) {
        snack(err.message || '清空失败', 'error');
      }
    });
  }

  /* ---------------- 启动 ---------------- */

  bindUI();
  if (!IS_LOCAL) setReadOnly();
  loadAll();

  // 二维码倒计时在本地推进，不发起网络请求。
  setInterval(() => {
    if (IS_LOCAL && !portChanged) tickPairing();
  }, 1000);
})();
