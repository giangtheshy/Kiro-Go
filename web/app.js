/*
 * Kiro-Go admin UI logic.
 */
(() => {
  'use strict';

  // State
  const baseUrl = location.origin;
  // Auth is cookie-based: the server issues an opaque HttpOnly session token at
  // login. The admin password is never stored client-side. Purge any credential
  // left over from older builds that persisted the raw password.
  ['admin_password', 'admin_login_time', 'kiro_remembered_pwd'].forEach((k) => {
    localStorage.removeItem(k);
    sessionStorage.removeItem(k);
  });
  let currentLang = localStorage.getItem('kiro_lang') || 'zh';
  const dict = { en: null, zh: null };
  let accountsData = [];
  const selectedAccounts = new Set();
  // Canonical Kiro model IDs offered in the Force-Model and per-key model dropdowns.
  // Keep in sync with the model list advertised at /v1/models (handler.go buildModelInfo).
  const KIRO_MODEL_OPTIONS = [
    'claude-opus-4.8', 'claude-opus-4.7', 'claude-opus-4.6', 'claude-opus-4.5',
    'claude-sonnet-4.6', 'claude-sonnet-4.5', 'claude-sonnet-4',
    'claude-haiku-4.5'
  ];
  let filterKeyword = '';
  let filterStatus = 'all';
  let privacyModeEnabled = true;
  let promptRules = [];
  let builderIdSession = '';
  let builderIdPollTimer = null;
  let iamSession = '';
  let exportSelectedIds = new Set();
  let apiKeyExportSelectedIds = new Set();
  let currentVersion = '';
  let testLogs = [];
  let testModalAccountId = '';
  let testModalModels = [];
  let testModalLoadingModels = false;
  let testModalModelError = false;
  let testModalRunning = false;
  let customSelectUid = 0;
  let customSelectObserver = null;
  let customSelectRefreshQueued = false;

  // DOM helpers
  const $ = (id) => document.getElementById(id);
  const qsa = (sel, root) => Array.from((root || document).querySelectorAll(sel));
  function escapeHtml(s) {
    const d = document.createElement('div');
    d.textContent = s == null ? '' : String(s);
    return d.innerHTML;
  }
  function escapeAttr(s) {
    return escapeHtml(s).replace(/"/g, '&quot;');
  }
  function maskProxyForDisplay(raw) {
    if (!raw) return '';
    try {
      const u = new URL(raw);
      return `${u.protocol}//${u.host}`;
    } catch (e) {
      return '';
    }
  }
  async function copyText(input) {
    const isPromise = input && typeof input.then === 'function';
    if (isPromise && typeof ClipboardItem !== 'undefined' && navigator.clipboard && navigator.clipboard.write) {
      const blobPromise = Promise.resolve(input).then(t => new Blob([String(t == null ? '' : t)], { type: 'text/plain' }));
      await navigator.clipboard.write([new ClipboardItem({ 'text/plain': blobPromise })]);
      return;
    }
    const text = isPromise ? await input : input;
    const str = String(text == null ? '' : text);
    if (navigator.clipboard && navigator.clipboard.writeText) {
      try {
        await navigator.clipboard.writeText(str);
        return;
      } catch (e) { }
    }
    const ta = document.createElement('textarea');
    ta.value = str;
    ta.readOnly = true;
    ta.className = 'clipboard-proxy';
    document.body.appendChild(ta);
    const range = document.createRange();
    range.selectNodeContents(ta);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    ta.setSelectionRange(0, str.length);
    document.execCommand('copy');
    sel.removeAllRanges();
    document.body.removeChild(ta);
  }
  function renderEndpointCode(id, value) {
    const el = $(id);
    if (!el) return;
    const raw = String(value || '');
    el.dataset.rawValue = raw;
    try {
      const url = new URL(raw);
      const path = url.pathname + url.search + url.hash;
      el.innerHTML =
        '<span class="api-code-protocol">' + escapeHtml(url.protocol + '//') + '</span>' +
        '<span class="api-code-host">' + escapeHtml(url.host) + '</span>' +
        '<span class="api-code-path">' + escapeHtml(path) + '</span>';
    } catch (e) {
      el.textContent = raw;
    }
  }

  // i18n
  async function loadLocale(lang) {
    if (dict[lang]) return dict[lang];
    try {
      const res = await fetch('/admin/locales/' + lang + '.json?v=' + Date.now(), { cache: 'no-store' });
      dict[lang] = await res.json();
    } catch (e) {
      dict[lang] = {};
    }
    return dict[lang];
  }
  function t(key, ...args) {
    const active = dict[currentLang] || {};
    const fallback = dict.zh || {};
    let text = active[key] || fallback[key] || key;
    args.forEach((arg, idx) => { text = text.replace('{' + idx + '}', arg); });
    return text;
  }
  function applyTranslations() {
    qsa('[data-i18n]').forEach(el => { el.textContent = t(el.dataset.i18n); });
    qsa('[data-i18n-placeholder]').forEach(el => { el.placeholder = t(el.dataset.i18nPlaceholder); });
    qsa('[data-i18n-title]').forEach(el => { el.title = t(el.dataset.i18nTitle); });
    qsa('[data-i18n-aria-label]').forEach(el => { el.setAttribute('aria-label', t(el.dataset.i18nAriaLabel)); });
    document.title = t('app.title');
    document.documentElement.lang = currentLang;
    updateLangButtons();
    applyTheme(getThemePref());
    refreshCustomSelects();
  }
  async function setLang(lang) {
    currentLang = lang;
    localStorage.setItem('kiro_lang', lang);
    await loadLocale(lang);
    applyTranslations();
    renderVersionBadge();
    renderAccounts();
    renderPromptRules();
  }
  function updateLangButtons() {
    qsa('.lang-btn').forEach(btn => btn.classList.toggle('active', btn.dataset.lang === currentLang));
    qsa('.lang-toggle').forEach(btn => {
      const label = btn.querySelector('.lang-toggle-label');
      if (label) label.textContent = currentLang === 'zh' ? t('lang.zh') : t('lang.en');
    });
  }
  function toggleLang() {
    setLang(currentLang === 'zh' ? 'en' : 'zh');
  }

  // Custom select
  function getCustomSelectLabel(select) {
    const option = select.selectedOptions && select.selectedOptions[0];
    return ((option && option.textContent) || select.value || '').trim();
  }
  function syncCustomSelect(select) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const value = wrap.querySelector('.custom-select-value');
    const trigger = wrap.querySelector('.custom-select-trigger');
    if (value) value.textContent = getCustomSelectLabel(select);
    if (trigger) trigger.disabled = select.disabled;
    wrap.classList.toggle('is-disabled', select.disabled);
    qsa('.custom-select-option', wrap).forEach(option => {
      const selected = option.dataset.index === String(select.selectedIndex);
      option.classList.toggle('is-selected', selected);
      option.setAttribute('aria-selected', String(selected));
    });
  }
  function renderCustomSelectOptions(select) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const content = wrap.__content;
    const trigger = wrap.querySelector('.custom-select-trigger');
    if (!content) return;
    if (trigger) labelCustomSelect(select, trigger, content, select.id);
    content.innerHTML = '';
    Array.from(select.options).forEach((option, index) => {
      const item = document.createElement('button');
      item.type = 'button';
      item.className = 'custom-select-option';
      item.setAttribute('role', 'option');
      item.dataset.index = String(index);
      item.disabled = option.disabled;
      item.textContent = (option.textContent || option.value || '').trim();
      content.appendChild(item);
    });
    syncCustomSelect(select);
  }
  function placeCustomSelectContent(select) {
    const wrap = select && select.__customSelect;
    if (!wrap || !wrap.classList.contains('is-open')) return;
    const trigger = wrap.querySelector('.custom-select-trigger');
    const content = wrap.__content;
    if (!trigger || !content) return;
    const rect = trigger.getBoundingClientRect();
    const gap = 4;
    const below = window.innerHeight - rect.bottom - gap;
    const above = rect.top - gap;
    const openUp = below < 180 && above > below;
    const available = Math.max(96, Math.min(224, (openUp ? above : below) - 4));
    content.style.left = Math.round(rect.left) + 'px';
    content.style.width = Math.round(rect.width) + 'px';
    content.style.maxHeight = Math.round(available) + 'px';
    content.style.top = openUp ? 'auto' : Math.round(rect.bottom + gap) + 'px';
    content.style.bottom = openUp ? Math.round(window.innerHeight - rect.top + gap) + 'px' : 'auto';
    content.dataset.side = openUp ? 'top' : 'bottom';
  }
  function setCustomSelectOpen(select, open) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const trigger = wrap.querySelector('.custom-select-trigger');
    const content = wrap.__content;
    if (!trigger || !content) return;
    if (open && !select.disabled) {
      closeAllCustomSelects(select);
      renderCustomSelectOptions(select);
      wrap.classList.add('is-open');
      trigger.setAttribute('aria-expanded', 'true');
      content.hidden = false;
      placeCustomSelectContent(select);
      requestAnimationFrame(() => placeCustomSelectContent(select));
      const selected = content.querySelector('.custom-select-option.is-selected:not(:disabled)') || content.querySelector('.custom-select-option:not(:disabled)');
      if (selected) selected.focus({ preventScroll: true });
    } else {
      wrap.classList.remove('is-open');
      trigger.setAttribute('aria-expanded', 'false');
      content.hidden = true;
    }
  }
  function closeAllCustomSelects(except) {
    qsa('select.custom-select-native').forEach(select => {
      if (select !== except) setCustomSelectOpen(select, false);
    });
  }
  function chooseCustomSelectOption(select, index) {
    const option = select.options[index];
    if (!option || option.disabled) return;
    select.value = option.value;
    select.dispatchEvent(new Event('input', { bubbles: true }));
    select.dispatchEvent(new Event('change', { bubbles: true }));
    syncCustomSelect(select);
    setCustomSelectOpen(select, false);
    const trigger = select.__customSelect && select.__customSelect.querySelector('.custom-select-trigger');
    if (trigger && trigger.isConnected) trigger.focus({ preventScroll: true });
  }
  function focusSiblingCustomOption(current, dir) {
    const options = qsa('.custom-select-option:not(:disabled)', current.parentElement);
    const index = options.indexOf(current);
    const next = options[(index + dir + options.length) % options.length];
    if (next) next.focus({ preventScroll: true });
  }
  function getCustomSelectLabelElement(select) {
    const explicit = qsa('label').find(label => label.htmlFor === select.id);
    if (explicit) return explicit;
    const group = select.closest('.form-group');
    return group ? group.querySelector('label') : null;
  }
  function labelCustomSelect(select, trigger, content, id) {
    trigger.id = id + '-trigger';
    const valueId = id + '-value';
    const value = trigger.querySelector('.custom-select-value');
    if (value) value.id = valueId;
    const label = getCustomSelectLabelElement(select);
    if (label) {
      if (!label.id) label.id = id + '-label';
      trigger.removeAttribute('aria-label');
      trigger.setAttribute('aria-labelledby', label.id + ' ' + valueId);
    } else {
      trigger.removeAttribute('aria-labelledby');
      trigger.setAttribute('aria-label', select.getAttribute('aria-label') || getCustomSelectLabel(select));
    }
    content.setAttribute('aria-labelledby', trigger.id);
  }
  function enhanceCustomSelect(select) {
    if (!select || select.__customSelect || select.dataset.nativeSelect === 'true') return;

    const id = select.id || 'custom-select-' + (++customSelectUid);
    if (!select.id) select.id = id;

    const wrap = document.createElement('div');
    wrap.className = 'custom-select';
    wrap.dataset.customSelect = 'true';
    if (select.id === 'filterStatusSelect') wrap.classList.add('custom-select-filter');

    const trigger = document.createElement('button');
    trigger.type = 'button';
    trigger.className = 'custom-select-trigger';
    trigger.setAttribute('aria-haspopup', 'listbox');
    trigger.setAttribute('aria-expanded', 'false');
    trigger.setAttribute('aria-controls', id + '-menu');
    trigger.innerHTML =
      '<span class="custom-select-value"></span>' +
      '<i class="fa-solid fa-chevron-down custom-select-icon" aria-hidden="true"></i>';

    const content = document.createElement('div');
    content.id = id + '-menu';
    content.className = 'custom-select-content';
    content.setAttribute('role', 'listbox');
    content.hidden = true;
    labelCustomSelect(select, trigger, content, id);

    wrap.appendChild(trigger);
    document.body.appendChild(content);
    wrap.__content = content;
    select.insertAdjacentElement('afterend', wrap);
    select.classList.add('custom-select-native');
    select.setAttribute('aria-hidden', 'true');
    select.tabIndex = -1;
    select.__customSelect = wrap;
    wrap.__nativeSelect = select;

    trigger.addEventListener('click', () => setCustomSelectOpen(select, !wrap.classList.contains('is-open')));
    trigger.addEventListener('keydown', e => {
      if (['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(e.key)) {
        e.preventDefault();
        setCustomSelectOpen(select, true);
      }
    });
    content.addEventListener('click', e => {
      const option = e.target.closest('.custom-select-option');
      if (!option) return;
      chooseCustomSelectOption(select, parseInt(option.dataset.index, 10));
    });
    content.addEventListener('keydown', e => {
      const option = e.target.closest('.custom-select-option');
      if (!option) return;
      if (e.key === 'ArrowDown') { e.preventDefault(); focusSiblingCustomOption(option, 1); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); focusSiblingCustomOption(option, -1); }
      else if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); chooseCustomSelectOption(select, parseInt(option.dataset.index, 10)); }
      else if (e.key === 'Escape') { e.preventDefault(); setCustomSelectOpen(select, false); trigger.focus({ preventScroll: true }); }
    });
    select.addEventListener('change', () => syncCustomSelect(select));
    renderCustomSelectOptions(select);
  }
  function enhanceCustomSelects(root) {
    qsa('select:not(.custom-select-native)', root || document).forEach(enhanceCustomSelect);
  }
  function refreshCustomSelects(root) {
    enhanceCustomSelects(root);
    qsa('select.custom-select-native', root || document).forEach(renderCustomSelectOptions);
  }
  function positionOpenCustomSelects() {
    qsa('select.custom-select-native').forEach(placeCustomSelectContent);
  }
  function queueCustomSelectRefresh() {
    if (customSelectRefreshQueued) return;
    customSelectRefreshQueued = true;
    requestAnimationFrame(() => {
      customSelectRefreshQueued = false;
      refreshCustomSelects();
      positionOpenCustomSelects();
    });
  }
  function initCustomSelectObserver() {
    if (customSelectObserver || !document.body || typeof MutationObserver === 'undefined') return;
    customSelectObserver = new MutationObserver(mutations => {
      let shouldRefresh = false;
      for (const mutation of mutations) {
        const target = mutation.target;
        if (target && target.closest && target.closest('.custom-select')) continue;
        if (target && target.matches && target.matches('select')) {
          shouldRefresh = true;
          break;
        }
        for (const node of mutation.addedNodes || []) {
          if (node.nodeType !== 1) continue;
          if ((node.matches && node.matches('select')) || (node.querySelector && node.querySelector('select'))) {
            shouldRefresh = true;
            break;
          }
        }
        if (shouldRefresh) break;
      }
      if (shouldRefresh) queueCustomSelectRefresh();
    });
    customSelectObserver.observe(document.body, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ['disabled', 'class', 'id', 'data-native-select']
    });
  }

  // Theme
  const THEME_ORDER = ['system', 'light', 'dark'];
  const themeMQ = window.matchMedia('(prefers-color-scheme: dark)');
  function resolveTheme(pref) {
    if (pref === 'dark') return 'dark';
    if (pref === 'light') return 'light';
    return themeMQ.matches ? 'dark' : 'light';
  }
  function applyTheme(pref) {
    const resolved = resolveTheme(pref);
    const root = document.documentElement;
    root.classList.toggle('dark', resolved === 'dark');
    root.dataset.themePref = pref;
    qsa('.theme-toggle').forEach(btn => {
      btn.dataset.theme = pref;
      const themeLabel = t('theme.status', t('theme.' + pref));
      btn.setAttribute('aria-label', themeLabel);
      btn.setAttribute('title', themeLabel);
    });
  }
  function getThemePref() {
    const saved = localStorage.getItem('kiro_theme');
    return THEME_ORDER.includes(saved) ? saved : 'system';
  }
  function initTheme() {
    applyTheme(getThemePref());
    themeMQ.addEventListener('change', () => {
      if (getThemePref() === 'system') applyTheme('system');
    });
  }
  function toggleTheme() {
    const cur = getThemePref();
    const next = THEME_ORDER[(THEME_ORDER.indexOf(cur) + 1) % THEME_ORDER.length];
    localStorage.setItem('kiro_theme', next);
    applyTheme(next);
  }

  // Privacy and email mask
  function initPrivacyMode() {
    const saved = localStorage.getItem('privacyMode');
    privacyModeEnabled = saved === null ? true : saved === 'true';
    const toggle = $('privacyModeToggle');
    if (toggle) toggle.checked = privacyModeEnabled;
  }
  function maskEmail(email) {
    if (!privacyModeEnabled || !email || email.indexOf('@') === -1) return email;
    const [local, domain] = email.split('@');
    const maskedLocal = local.length <= 2 ? local : local.substring(0, 2) + '***';
    const parts = domain.split('.');
    if (parts.length >= 2) {
      const tld = parts[parts.length - 1];
      const sld = parts[parts.length - 2];
      const maskedSld = sld.length <= 2 ? sld : sld.substring(0, 2) + '***';
      const subs = parts.slice(0, -2).map(s => s.length <= 2 ? s : s.substring(0, 2) + '***');
      return maskedLocal + '@' + [...subs, maskedSld, tld].join('.');
    }
    return maskedLocal + '@' + domain;
  }
  function getDisplayEmail(email, id) {
    const raw = email || (id ? id.substring(0, 12) + '...' : '-');
    return maskEmail(raw);
  }

  // Toast bridge
  const toast = function (msg, variant, opts) {
    if (typeof window.toast === 'function') return window.toast(msg, variant, opts);
    try { console.warn('[toast missing]', variant, msg); } catch (_) { }
    return function () {};
  };
  const toastPrimary = (msg, opts) => toast(msg, 'primary', opts);
  const toastWarning = (msg, opts) => toast(msg, 'warning', opts);
  const toastError = (msg, opts) => toast(msg, 'error', opts);

  // Modal helpers
  let modalScrollY = 0;
  let confirmResolve = null;
  const modalFocusStack = [];
  function lockModalScroll() {
    if (document.body.classList.contains('modal-open')) return;
    modalScrollY = window.scrollY || document.documentElement.scrollTop || 0;
    document.body.style.top = '-' + modalScrollY + 'px';
    document.body.classList.add('modal-open');
  }
  function unlockModalScrollIfIdle() {
    if (qsa('.modal.active').length > 0) return;
    if (!document.body.classList.contains('modal-open')) return;
    document.body.classList.remove('modal-open');
    document.body.style.top = '';
    window.scrollTo(0, modalScrollY);
  }
  function getModalFocusable(modal) {
    return qsa('a[href], button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])', modal)
      .filter(el => !el.closest('[hidden]'));
  }
  function prepareDialog(modal) {
    modal.setAttribute('role', 'dialog');
    modal.setAttribute('aria-modal', 'true');
    modal.setAttribute('aria-hidden', 'false');
    if (!modal.hasAttribute('tabindex')) modal.tabIndex = -1;
    const title = modal.querySelector('.modal-title');
    if (title) {
      if (!title.id) title.id = modal.id + 'Title';
      modal.setAttribute('aria-labelledby', title.id);
    }
  }
  function focusDialog(modal) {
    if (modal.contains(document.activeElement) && document.activeElement !== modal) return;
    const focusable = getModalFocusable(modal);
    const target = focusable[0] || modal;
    if (target && target.focus) target.focus({ preventScroll: true });
  }
  function trapDialogFocus(e) {
    const modal = e.currentTarget;
    if (e.key !== 'Tab' || !modal.classList.contains('active')) return;
    const focusable = getModalFocusable(modal);
    if (!focusable.length) {
      e.preventDefault();
      modal.focus({ preventScroll: true });
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus({ preventScroll: true });
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus({ preventScroll: true });
    }
  }
  function openDialog(id) {
    const modal = $(id);
    if (!modal) return;
    prepareDialog(modal);
    modalFocusStack.push({ id, el: document.activeElement });
    modal.removeEventListener('keydown', trapDialogFocus);
    modal.addEventListener('keydown', trapDialogFocus);
    modal.classList.add('active');
    lockModalScroll();
    focusDialog(modal);
    setTimeout(() => focusDialog(modal), 0);
  }
  function closeDialog(id) {
    const modal = $(id);
    if (!modal) return;
    modal.classList.remove('active');
    modal.setAttribute('aria-hidden', 'true');
    const stackIndex = modalFocusStack.map(item => item.id).lastIndexOf(id);
    const previous = stackIndex >= 0 ? modalFocusStack.splice(stackIndex, 1)[0].el : null;
    unlockModalScrollIfIdle();
    if (previous && previous.isConnected && previous.focus) {
      requestAnimationFrame(() => previous.focus({ preventScroll: true }));
    }
  }
  function bindDialogBackdropClose(id, closeFn) {
    const modal = $(id);
    if (!modal) return;
    let startedOnBackdrop = false;
    modal.addEventListener('pointerdown', e => {
      startedOnBackdrop = e.target === modal;
    });
    modal.addEventListener('click', e => {
      if (startedOnBackdrop && e.target === modal) closeFn();
      startedOnBackdrop = false;
    });
  }
  function closeConfirm(value) {
    if (!confirmResolve) return;
    const resolve = confirmResolve;
    confirmResolve = null;
    closeDialog('confirmModal');
    resolve(!!value);
  }
  function confirmAction(message, opts) {
    opts = opts || {};
    if (confirmResolve) closeConfirm(false);
    const modal = $('confirmModal');
    const title = $('confirmTitle');
    const msg = $('confirmMessage');
    const ok = $('confirmOk');
    const cancel = $('confirmCancel');
    const close = $('confirmClose');
    if (!modal || !title || !msg || !ok || !cancel || !close) {
      return Promise.resolve(false);
    }
    title.textContent = opts.title || t('common.confirm');
    msg.textContent = message || '';
    ok.textContent = opts.confirmText || t('common.confirm');
    cancel.textContent = opts.cancelText || t('common.cancel');
    ok.className = 'btn ' + (opts.variant === 'danger' ? 'btn-danger' : 'btn-primary');
    cancel.className = 'btn btn-secondary';
    ok.onclick = () => closeConfirm(true);
    cancel.onclick = () => closeConfirm(false);
    close.onclick = () => closeConfirm(false);
    const pending = new Promise(resolve => { confirmResolve = resolve; });
    openDialog('confirmModal');
    ok.focus({ preventScroll: true });
    return pending;
  }

  // Fetch wrapper. Auth rides on the HttpOnly session cookie (same-origin), so no
  // credential header is sent from JS.
  function api(path, opts) {
    opts = opts || {};
    opts.credentials = 'same-origin';
    opts.headers = Object.assign({}, opts.headers || {});
    if (opts.body && !opts.headers['Content-Type']) opts.headers['Content-Type'] = 'application/json';
    return fetch('/admin/api' + path, opts);
  }

  // Login
  //
  // The admin password is exchanged once at /admin/api/login for an opaque, expiring
  // session token that the server stores server-side and delivers in an HttpOnly,
  // SameSite=Strict (Secure on HTTPS) cookie. The browser never stores the password,
  // and the cookie cannot be read by JavaScript — so an XSS can't lift a reusable
  // credential. The SSE log stream (EventSource) authenticates with the same cookie
  // automatically. "Remember me" controls whether the cookie persists across browser
  // restarts (server sets Max-Age) versus lasting only for the browser session.
  async function tryAutoLogin() {
    // No stored credential — just probe whether the session cookie is still valid.
    try {
      const res = await api('/status');
      if (res.ok) { showMain(); loadData(); }
    } catch (e) { }
  }
  async function login() {
    const pwd = $('pwdField').value;
    const rememberEl = $('rememberPwd');
    const remember = !!(rememberEl && rememberEl.checked);
    try {
      const res = await api('/login', {
        method: 'POST',
        body: JSON.stringify({ password: pwd, remember: remember }),
      });
      if (res.ok) {
        // Persist only the non-secret remember preference, never the password.
        if (remember) localStorage.setItem('kiro_remember', '1');
        else localStorage.removeItem('kiro_remember');
        $('pwdField').value = '';
        showMain(); loadData();
      } else if (res.status === 429) {
        toast(t('login.error'), 'error');
      } else {
        toast(t('login.error'), 'error');
      }
    } catch (e) {
      toast(t('login.connectError'), 'error');
    }
  }
  function initRememberMe() {
    const remember = $('rememberPwd');
    if (!remember) return;
    // Restore only the checkbox state; never pre-fill the password field.
    if (localStorage.getItem('kiro_remember') === '1') {
      remember.checked = true;
    }
  }
  async function logout() {
    try { await api('/logout', { method: 'POST' }); } catch (e) { }
    location.reload();
  }
  function showMain() {
    $('loginPage').classList.add('hidden');
    $('mainPage').classList.remove('hidden');
    restoreTabFromUrl();
  }

  // Data loaders
  async function loadData() {
    await Promise.all([loadStats(), loadAccounts(), loadSettings(), loadVersion()]);
    renderEndpointCode('claudeEndpoint', baseUrl + '/v1/messages');
    renderEndpointCode('openaiEndpoint', baseUrl + '/v1/chat/completions');
    renderEndpointCode('openaiResponsesEndpoint', baseUrl + '/v1/responses');
    renderEndpointCode('modelsEndpoint', baseUrl + '/v1/models');
    renderEndpointCode('statsEndpoint', baseUrl + '/v1/stats');
    setTimeout(checkUpdate, 2000);
  }
  async function loadStats() {
    const res = await api('/status');
    const d = await res.json();
    $('statAccounts').textContent = d.accounts || 0;
    $('statRequests').textContent = d.totalRequests || 0;
    $('statSuccess').textContent = d.successRequests || 0;
    $('statFailed').textContent = d.failedRequests || 0;
    $('statTokens').textContent = formatNum(d.totalTokens || 0);
    $('statCredits').textContent = (d.totalCredits || 0).toFixed(1);
    renderMaintenanceState(!!d.maintenance, d.maintenanceMessage);
  }
  async function loadAccounts() {
    const res = await api('/accounts');
    accountsData = await res.json();
    renderAccounts();
    renderAppliedProxies();
  }

  // Account list
  function getFilteredAccounts() {
    return accountsData.filter(a => {
      if (filterStatus === 'enabled' && !a.enabled) return false;
      if (filterStatus === 'disabled' && (a.enabled || (a.banStatus && a.banStatus !== 'ACTIVE'))) return false;
      if (filterStatus === 'banned' && (!a.banStatus || a.banStatus === 'ACTIVE')) return false;
      if (filterKeyword) {
        const kw = filterKeyword.toLowerCase();
        if (!(a.email || '').toLowerCase().includes(kw)) return false;
      }
      return true;
    });
  }
  function onFilterChange() {
    filterKeyword = $('filterSearch').value;
    filterStatus = $('filterStatusSelect').value;
    renderAccounts();
  }
  function toggleSelectAll(checked) {
    const filtered = getFilteredAccounts();
    if (checked) filtered.forEach(a => selectedAccounts.add(a.id));
    else selectedAccounts.clear();
    renderAccounts();
    updateBatchBar();
  }
  function toggleSelectAccount(id) {
    if (selectedAccounts.has(id)) selectedAccounts.delete(id);
    else selectedAccounts.add(id);
    updateBatchBar();
  }
  function updateBatchBar() {
    const bar = $('batchBar');
    const count = selectedAccounts.size;
    const cb = $('selectAllCheckbox');
    if (cb) {
      const filtered = getFilteredAccounts();
      const selectedFiltered = filtered.filter(a => selectedAccounts.has(a.id)).length;
      cb.checked = filtered.length > 0 && selectedFiltered === filtered.length;
      cb.indeterminate = selectedFiltered > 0 && selectedFiltered < filtered.length;
    }
    if (count > 0) {
      bar.classList.remove('hidden');
      $('batchCount').textContent = String(count);
    } else {
      bar.classList.add('hidden');
    }
  }

  function formatSubscriptionLabel(type) {
    const s = (type || '').toUpperCase();
    if (s.includes('POWER')) return t('subscription.power');
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return t('subscription.proPlus');
    if (s.includes('PRO')) return t('subscription.pro');
    if (s.includes('FREE')) return t('subscription.free');
    return type || t('subscription.free');
  }
  function getSubBadge(type) {
    const s = (type || '').toUpperCase();
    if (s.includes('POWER')) return '<span class="badge badge-power">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return '<span class="badge badge-proplus">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    if (s.includes('PRO')) return '<span class="badge badge-pro">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    return '<span class="badge badge-free">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
  }
  function getTrialBadge(a) {
    if (a.trialStatus === 'ACTIVE' && a.trialUsageLimit > 0) {
      return '<span class="badge badge-trial">' + escapeHtml(t('accounts.trial')) + '</span>';
    }
    return '';
  }
  function formatTrialExpiry(ts) {
    if (!ts) return '';
    const date = new Date(ts * 1000);
    const diffDays = Math.ceil((date - new Date()) / (1000 * 60 * 60 * 24));
    if (diffDays < 0) return '(' + t('accounts.trialExpired') + ')';
    if (diffDays === 0) return '(' + t('accounts.trialToday') + ')';
    if (diffDays <= 7) return '(' + diffDays + t('accounts.trialDays') + ')';
    return '';
  }
  function formatAuthMethod(method) {
    if (!method) return '-';
    const normalized = String(method).toLowerCase();
    if (normalized === 'idc') return t('auth.enterprise');
    if (normalized === 'social') return t('auth.social');
    if (normalized === 'builderid') return 'BuilderID';
    if (normalized === 'github') return t('local.providerGithub');
    if (normalized === 'google') return t('local.providerGoogle');
    return method;
  }
  function getStatusBadge(a) {
    const out = [];
    const isBanned = a.banStatus && a.banStatus !== 'ACTIVE';
    if (isBanned) {
      if (a.banStatus === 'BANNED') out.push('<span class="badge badge-banned">' + escapeHtml(t('accounts.banned')) + '</span>');
      else if (a.banStatus === 'SUSPENDED') out.push('<span class="badge badge-suspended">' + escapeHtml(t('accounts.suspended')) + '</span>');
      out.push('<span class="badge badge-warning">' + escapeHtml(t('accounts.disabled')) + '</span>');
    } else {
      if (!a.hasToken)
        out.push('<span class="badge badge-error">' + escapeHtml(t('accounts.noToken')) + '</span>');
      else if (a.expiresAt && a.expiresAt < Date.now() / 1000)
        out.push('<span class="badge badge-warning">' + escapeHtml(t('accounts.expired')) + '</span>');
      else
        out.push('<span class="badge badge-success">' + escapeHtml(t('accounts.normal')) + '</span>');
      out.push(a.enabled
        ? '<span class="badge badge-info">' + escapeHtml(t('accounts.enabled')) + '</span>'
        : '<span class="badge badge-warning">' + escapeHtml(t('accounts.disabled')) + '</span>');
    }
    return out.join('');
  }
  function formatTokenExpiry(ts) {
    if (!ts) return '-';
    const diff = ts - Date.now() / 1000;
    if (diff <= 0) return t('time.expired');
    if (diff < 3600) return Math.floor(diff / 60) + t('time.minutes');
    if (diff < 86400) return Math.floor(diff / 3600) + t('time.hours');
    return Math.floor(diff / 86400) + t('time.days');
  }
  function formatNum(n) {
    if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
    return n.toString();
  }
  function applyUsageBars(root) {
    qsa('.usage-fill[data-usage-pct]', root).forEach(el => {
      const pct = Math.max(0, Math.min(100, parseFloat(el.dataset.usagePct) || 0));
      el.style.width = pct + '%';
    });
  }

  function renderAppliedProxies() {
    const box = $('appliedProxyList');
    if (!box) return;
    const globalMasked = maskProxyForDisplay(window.__globalProxyURL);
    const groups = new Map();
    for (const a of accountsData) {
      const key = maskProxyForDisplay(a.proxyURL) || (globalMasked ? globalMasked + '|global' : '|none');
      if (!groups.has(key)) groups.set(key, 0);
      groups.set(key, groups.get(key) + 1);
    }
    if (groups.size === 0) {
      box.innerHTML = '<p class="help-block">' + escapeHtml(t('proxyApplied.empty')) + '</p>';
      return;
    }
    const entries = [...groups.entries()].sort((x, y) => y[1] - x[1]);
    box.innerHTML = entries.map(([key, count]) => {
      let label, cls = 'proxy-badge';
      if (key === '|none') { label = t('account.proxyBadgeNone'); if (window.__requireProxy) cls += ' proxy-badge-warn'; }
      else if (key.endsWith('|global')) { label = key.slice(0, -('|global'.length)) + ' (' + t('account.proxyBadgeGlobal') + ')'; }
      else { label = key; }
      return '<div class="applied-proxy-row">' +
        '<span class="' + cls + '" title="' + escapeAttr(label) + '">' + escapeHtml(label) + '</span>' +
        '<span class="applied-proxy-count">' + escapeHtml(t('proxyApplied.accountCount', count)) + '</span>' +
        '</div>';
    }).join('');
  }
  function renderAccounts() {
    const container = $('accountsList');
    if (!container) return;
    const filtered = getFilteredAccounts();
    if (filtered.length === 0) {
      container.innerHTML = '<div class="empty-state">' + escapeHtml(t('accounts.empty')) + '</div>';
      return;
    }
    container.innerHTML = filtered.map(a => {
      const usagePct = (a.usagePercent || 0) * 100;
      const usageClass = usagePct > 90 ? 'critical' : usagePct > 70 ? 'high' : '';
      const trialPct = (a.trialUsagePercent || 0) * 100;
      const trialClass = trialPct > 90 ? 'critical' : trialPct > 70 ? 'high' : '';
      const isSelected = selectedAccounts.has(a.id);
      const weight = a.weight || 0;
      const weightBadge = weight >= 2 ? '<span class="badge badge-warning">' + escapeHtml(t('accounts.weightShort')) + ':' + weight + '</span>' : '';
      const overageBadge = renderOverageBadge(a);
      let proxyBadge = '';
      const maskedProxy = maskProxyForDisplay(a.proxyURL);
      if (maskedProxy) {
        proxyBadge = '<span class="proxy-badge" title="' + escapeAttr(maskedProxy) + '">' + escapeHtml(maskedProxy) + '</span>';
      } else if (window.__globalProxyURL) {
        proxyBadge = '<span class="proxy-badge">' + escapeHtml(t('account.proxyBadgeGlobal')) + '</span>';
      } else if (window.__requireProxy) {
        proxyBadge = '<span class="proxy-badge proxy-badge-warn">' + escapeHtml(t('account.proxyBadgeNone')) + '</span>';
      } else {
        proxyBadge = '<span class="proxy-badge">' + escapeHtml(t('account.proxyBadgeDirect')) + '</span>';
      }
      const banned = a.banStatus && a.banStatus !== 'ACTIVE';
      const idAttr = escapeAttr(a.id);
      const displayEmail = getDisplayEmail(a.email, a.id);
      const selectLabel = t('accounts.selectAccount', displayEmail);

      const refreshSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>';
      const userSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg>';
      const copySvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';

      return '' +
        '<div class="account-card' + (isSelected ? ' selected' : '') + '" data-id="' + idAttr + '">' +
        '<div class="account-header">' +
        '<div class="account-info">' +
        '<input type="checkbox" class="account-checkbox" ' + (isSelected ? 'checked' : '') + ' data-id="' + idAttr + '" aria-label="' + escapeAttr(selectLabel) + '" />' +
        '<div class="account-info-text">' +
        '<div class="account-email">' + escapeHtml(displayEmail) + '</div>' +
        '<div class="account-meta">' +
        getSubBadge(a.subscriptionType) +
        getTrialBadge(a) +
        weightBadge +
        overageBadge +
        proxyBadge +
        '<span class="badge badge-info">' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + '</span>' +
        getStatusBadge(a) +
        '</div>' +
        '</div>' +
        '</div>' +
        '<div class="account-actions">' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="refresh" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.refresh')) + '">' + refreshSvg + '</button>' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="detail" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.detail')) + '">' + userSvg + '</button>' +
        '<button class="btn btn-icon btn-sm btn-ghost" data-action="copyJSON" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.copyJSON')) + '">' + copySvg + '</button>' +
        (banned ? '' :
          '<button class="btn btn-sm ' + (a.enabled ? 'btn-outline' : 'btn-primary') + '" data-action="toggle" data-id="' + idAttr + '" data-enabled="' + (!a.enabled) + '">' +
          escapeHtml(a.enabled ? t('accounts.disable') : t('accounts.enable')) +
          '</button>') +
        '<button class="btn btn-sm btn-secondary" data-action="test" data-id="' + idAttr + '" id="test-' + idAttr + '">' + escapeHtml(t('accounts.test')) + '</button>' +
        '<button class="btn btn-sm btn-danger" data-action="delete" data-id="' + idAttr + '">' + escapeHtml(t('accounts.delete')) + '</button>' +
        '</div>' +
        '</div>' +
        (a.usageLimit > 0 ?
          '<div class="account-usage">' +
          '<div class="usage-label">' + escapeHtml(t('accounts.mainQuota')) + '</div>' +
          '<div class="usage-bar"><div class="usage-fill ' + usageClass + '" data-usage-pct="' + escapeAttr(usagePct) + '"></div></div>' +
          '<div class="usage-text"><span>' + (a.usageCurrent != null ? a.usageCurrent.toFixed(1) : 0) + ' / ' + (a.usageLimit != null ? a.usageLimit.toFixed(0) : 0) + '</span><span>' + usagePct.toFixed(1) + '%</span></div>' +
          '</div>' : '') +
        (a.trialUsageLimit > 0 ?
          '<div class="account-usage">' +
          '<div class="usage-label">' + escapeHtml(t('accounts.trialQuota')) + ' ' + escapeHtml(formatTrialExpiry(a.trialExpiresAt)) + '</div>' +
          '<div class="usage-bar"><div class="usage-fill ' + trialClass + '" data-usage-pct="' + escapeAttr(trialPct) + '"></div></div>' +
          '<div class="usage-text"><span>' + (a.trialUsageCurrent != null ? a.trialUsageCurrent.toFixed(1) : 0) + ' / ' + (a.trialUsageLimit != null ? a.trialUsageLimit.toFixed(0) : 0) + '</span><span>' + trialPct.toFixed(1) + '%</span></div>' +
          '</div>' : '') +
        '<div class="account-stats">' +
        '<div class="account-stat"><div class="account-stat-value">' + (a.requestCount || 0) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.requests')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + formatNum(a.totalTokens || 0) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.tokens')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + (a.totalCredits || 0).toFixed(1) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.credits')) + '</div></div>' +
        '<div class="account-stat"><div class="account-stat-value">' + escapeHtml(formatTokenExpiry(a.expiresAt)) + '</div><div class="account-stat-label">' + escapeHtml(t('accounts.expiry')) + '</div></div>' +
        '</div>' +
        '</div>';
    }).join('');
    applyUsageBars(container);
    enhanceCustomSelects(container);
  }

  // Account actions
  async function refreshAccount(id, card) {
    if (card) card.classList.add('loading');
    try {
      const res = await api('/accounts/' + id + '/refresh', { method: 'POST' });
      const d = await res.json();
      if (d.success) loadAccounts();
      else toastError(t('accounts.refreshFailed') + ': ' + (d.error || ''));
    } catch (e) {
      toastError(t('accounts.refreshFailed'));
    }
    if (card) card.classList.remove('loading');
  }
  async function toggleAccount(id, enabled) {
    await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify({ enabled }) });
    loadAccounts();
  }
  async function deleteAccount(id) {
    const ok = await confirmAction(t('accounts.confirmDelete'), {
      title: t('accounts.delete'),
      confirmText: t('accounts.delete'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/accounts/' + id, { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('accounts.deleteSuccess'), 'danger', { icon: 'fa-solid fa-trash' });
      loadAccounts(); loadStats();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function copyAccountJSON(id, btn) {
    try {
      const jsonPromise = api('/accounts/' + id + '/full').then(async res => {
        if (!res.ok) throw new Error('Failed');
        const a = await res.json();
        const { clientId, clientSecret, accessToken, refreshToken,
          authMethod, provider, issuerUrl, idpClientId, scopes, loginHint, region, expiresAt } = a;
        return JSON.stringify({ clientId, clientSecret, accessToken, refreshToken,
          authMethod, provider, issuerUrl, idpClientId, scopes, loginHint, region, expiresAt }, null, 2);
      });
      await copyText(jsonPromise);
      flashCopySuccess(btn);
      toastPrimary(t('accounts.copyJSONSuccess'));
    } catch (e) {
      toastError(t('common.failed'));
    }
  }
  function flashCopySuccess(btn) {
    if (!btn) return;
    const html = btn.innerHTML, cls = btn.className;
    btn.disabled = true;
    btn.className = 'btn btn-icon btn-sm btn-success';
    btn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>';
    setTimeout(() => { btn.disabled = false; btn.className = cls; btn.innerHTML = html; }, 800);
  }

  // Batch actions
  async function batchAction(action) {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmKey = 'batch.confirm' + action.charAt(0).toUpperCase() + action.slice(1);
    const ok = await confirmAction(t(confirmKey, ids.length), {
      title: t('common.confirm'),
      confirmText: t('common.confirm'),
      variant: action === 'disable' ? 'danger' : 'primary'
    });
    if (!ok) return;
    const dismiss = toast(t('batch.processing'), 'info', { duration: 0 });
    try {
      const res = await api('/accounts/batch', { method: 'POST', body: JSON.stringify({ ids, action }) });
      const d = await res.json();
      if (!res.ok || !d.success) throw new Error(d.error || t('common.failed'));
      dismiss();
      if (action === 'refresh') {
        toast(t('batch.refreshResult', d.refreshed || 0, d.failed || 0), d.failed ? 'warning' : 'success');
      } else if (action === 'enable') {
        toast(t('batch.enableResult', d.count || ids.length), 'success');
      } else if (action === 'disable') {
        toast(t('batch.disableResult', d.count || ids.length), 'success');
      } else {
        toast(t('batch.done'), 'success');
      }
      selectedAccounts.clear();
      updateBatchBar();
      loadAccounts(); loadStats();
    } catch (e) {
      dismiss();
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function batchRefreshModels() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmed = await confirmAction(t('batch.confirmRefreshModels', ids.length), {
      title: t('models.refreshAll'),
      confirmText: t('common.confirm')
    });
    if (!confirmed) return;
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    let ok = 0, fail = 0;
    for (const id of ids) {
      try {
        const res = await api('/accounts/' + id + '/models/refresh', { method: 'POST' });
        const d = await res.json();
        if (d.success) ok++; else fail++;
      } catch { fail++; }
    }
    dismiss();
    toast(t('batch.refreshModelsResult', ok, fail), fail ? 'warning' : 'success');
    selectedAccounts.clear();
    updateBatchBar();
    loadAccounts();
  }
  async function batchDelete() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmed = await confirmAction(t('batch.confirmDelete', ids.length), {
      title: t('accounts.delete'),
      confirmText: t('accounts.delete'),
      variant: 'danger'
    });
    if (!confirmed) return;
    const dismiss = toast(t('batch.deleting'), 'info', { duration: 0 });
    let ok = 0, fail = 0;
    for (const id of ids) {
      try {
        const res = await api('/accounts/' + id, { method: 'DELETE' });
        const d = await res.json().catch(() => ({}));
        if (res.ok && d.success !== false) ok++; else fail++;
      } catch { fail++; }
    }
    dismiss();
    toast(t('batch.deleteResult', ok, fail), fail ? 'warning' : 'success', { icon: 'fa-solid fa-trash' });
    selectedAccounts.clear();
    updateBatchBar();
    loadAccounts(); loadStats();
  }
  async function refreshAllModels() {
    const ok = await confirmAction(t('models.confirmRefreshAll'), {
      title: t('models.refreshAll'),
      confirmText: t('models.refreshAll')
    });
    if (!ok) return;
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/models/refresh', { method: 'POST' });
      const d = await res.json();
      dismiss();
      toast(t('models.refreshAllDone', d.refreshed || 0), 'success');
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    }
  }
  async function refreshAccountModels(id) {
    const dismiss = toast(t('detail.refreshModelCache') + '…', 'info', { duration: 0 });
    try {
      const res = await api('/accounts/' + id + '/models/refresh', { method: 'POST' });
      const d = await res.json();
      dismiss();
      if (d.success) toast(t('detail.refreshModelCache') + ' · ' + (d.count || 0), 'success');
      else toast(t('common.failed') + (d.error ? ': ' + d.error : ''), 'error');
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    }
  }

  // Detail modal
  function detailItem(label, value) {
    return '<div class="detail-item"><div class="detail-label">' + escapeHtml(label) + '</div><div class="detail-value">' + escapeHtml(value) + '</div></div>';
  }
  function showDetail(id) {
    const a = accountsData.find(x => x.id === id);
    if (!a) return;
    const idAttr = escapeAttr(id);
    $('detailBody').innerHTML =
      '<div class="detail-section"><h4>' + escapeHtml(t('detail.basicInfo')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.email'), getDisplayEmail(a.email, null)) +
      detailItem(t('detail.userId'), a.userId || '-') +
      detailItem(t('detail.authMethod'), formatAuthMethod(a.provider || a.authMethod)) +
      detailItem(t('detail.region'), a.region || 'us-east-1') +
      '</div></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.machineId')) + '</h4><div class="machine-id-row">' +
      '<input type="text" id="machineIdInput" value="' + escapeAttr(a.machineId || '') + '" placeholder="UUID" />' +
      '<button class="btn btn-sm btn-outline" id="generateMachineIdBtn" type="button">' + escapeHtml(t('detail.generate')) + '</button>' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveMachineId" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.weight')) + '</h4>' +
      '<div class="form-group">' +
      '<input type="number" id="weightInput" value="' + (a.weight || 0) + '" min="0" max="10" />' +
      '<small>' + escapeHtml(t('detail.weightHint')) + '</small>' +
      '</div>' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveWeight" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.priority')) + '</h4>' +
      '<div class="form-group">' +
      '<input type="number" id="priorityInput" value="' + (a.priority || 0) + '" min="0" max="99" />' +
      '<small>' + escapeHtml(t('detail.priorityHint')) + '</small>' +
      '</div>' +
      '<button class="btn btn-sm btn-primary" data-detail-action="savePriority" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div>' +

      '<div class="detail-section">' +
      '<h4>' + escapeHtml(t('detail.overage')) +
      ' <button class="btn btn-sm btn-outline" data-detail-action="refreshOverage" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.overageRefresh')) + '</button>' +
      '</h4>' +
      '<p class="help-block">' + escapeHtml(t('detail.overageHint')) + '</p>' +
      renderOverageBlock(a, idAttr) +
      '</div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.proxyURL')) + '</h4><div class="machine-id-row">' +
      '<input type="text" id="proxyURLInput" value="' + escapeAttr(a.proxyURL || '') + '" placeholder="socks5://host:port" />' +
      '<button class="btn btn-sm btn-primary" data-detail-action="saveProxyURL" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>' +
      '</div><p class="help-block">' + escapeHtml(t('detail.proxyHint')) + '</p></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.subscription')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.subscriptionType'), a.subscriptionTitle || (a.subscriptionType ? formatSubscriptionLabel(a.subscriptionType) : '-')) +
      detailItem(t('detail.tokenExpiry'), a.expiresAt ? new Date(a.expiresAt * 1000).toLocaleString() : '-') +
      detailItem(t('detail.mainQuota'), (a.usageCurrent != null ? a.usageCurrent.toFixed(1) : 0) + ' / ' + (a.usageLimit != null ? a.usageLimit.toFixed(0) : 0)) +
      detailItem(t('detail.resetDate'), a.nextResetDate || '-') +
      (a.trialUsageLimit > 0 ?
        detailItem(t('detail.trialQuota'), (a.trialUsageCurrent != null ? a.trialUsageCurrent.toFixed(1) : 0) + ' / ' + a.trialUsageLimit.toFixed(0)) +
        detailItem(t('detail.trialStatus'), a.trialStatus || '-') +
        detailItem(t('detail.trialExpiry'), a.trialExpiresAt ? new Date(a.trialExpiresAt * 1000).toLocaleString() : '-')
        : '') +
      '</div></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.statistics')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.requestCount'), a.requestCount || 0) +
      detailItem(t('detail.errorCount'), a.errorCount || 0) +
      detailItem(t('detail.totalTokens'), formatNum(a.totalTokens || 0)) +
      detailItem(t('detail.totalCredits'), (a.totalCredits || 0).toFixed(2)) +
      '</div></div>' +

      '<div class="detail-section">' +
      '<h4>' + escapeHtml(t('detail.models')) +
      ' <button class="btn btn-sm btn-outline" data-detail-action="loadModels" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.loadModels')) + '</button>' +
      ' <button class="btn btn-sm btn-outline" data-detail-action="refreshModels" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.refreshModelCache')) + '</button>' +
      '</h4>' +
      '<div id="modelsList" class="model-list"></div>' +
      '</div>';

    openDialog('detailModal');
  }
  async function loadModels(id) {
    const c = $('modelsList');
    c.innerHTML = '<p class="empty-state">' + escapeHtml(t('detail.loading')) + '</p>';
    try {
      const res = await api('/accounts/' + id + '/models');
      const d = await res.json();
      if (d.success && d.models) {
        const sorted = d.models.slice().sort((a, b) => {
          if (a.modelId === 'auto') return -1;
          if (b.modelId === 'auto') return 1;
          return (a.rateMultiplier || 1) - (b.rateMultiplier || 1);
        });
        c.innerHTML = sorted.map(m => {
          const ratio = m.rateMultiplier || 1;
          return '<div class="model-item">' +
            '<div class="model-name">' + escapeHtml(m.modelId) + '</div>' +
            '<div class="model-credit"><span class="credit-ratio">' + escapeHtml(t('detail.creditMultiplier', ratio)) + '</span></div>' +
            '<div class="model-info">' + escapeHtml(m.description || '') + '</div>' +
            '</div>';
        }).join('') || '<p class="empty-state">' + escapeHtml(t('detail.noModels')) + '</p>';
      } else {
        c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + ': ' + escapeHtml(d.error || '') + '</p>';
        toast(t('detail.loadFailed') + (d.error ? ': ' + d.error : ''), 'error');
      }
    } catch (e) {
      c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + '</p>';
      toast(t('detail.loadFailed'), 'error');
    }
  }
  async function generateMachineId() {
    try {
      const res = await api('/generate-machine-id');
      const d = await res.json();
      if (d.machineId) $('machineIdInput').value = d.machineId;
    } catch (e) {
      toast(t('detail.generateFailed'), 'error');
    }
  }
  async function putAccount(id, body, successMsg) {
    try {
      const res = await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify(body) });
      const d = await res.json();
      if (d.success) {
        toast(successMsg, 'success');
        loadAccounts();
      } else {
        toast(t('detail.saveFailed') + (d.error ? ': ' + d.error : ''), 'error');
      }
    } catch (e) {
      toast(t('detail.saveFailed'), 'error');
    }
  }
  async function saveMachineId(id) {
    const m = $('machineIdInput').value.trim();
    if (m && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(m) && !/^[0-9a-f]{32}$/i.test(m)) {
      toast(t('detail.machineIdError'), 'warning'); return;
    }
    await putAccount(id, { machineId: m }, t('detail.saved'));
  }
  async function saveWeight(id) {
    const weight = parseInt($('weightInput').value, 10) || 0;
    await putAccount(id, { weight }, t('detail.saved'));
  }
  async function savePriority(id) {
    const priority = Math.max(0, parseInt($('priorityInput').value, 10) || 0);
    await putAccount(id, { priority }, t('detail.saved'));
  }
  function renderOverageBadge(a) {
    const status = (a.overageStatus || '').toUpperCase();
    if (status === 'ENABLED') {
      return '<span class="badge badge-warning">' + escapeHtml(t('accounts.overageOn')) + '</span>';
    }
    if (status === 'DISABLED') {
      return '<span class="badge badge-muted">' + escapeHtml(t('accounts.overageOff')) + '</span>';
    }
    return '';
  }
  function renderOverageBlock(a, idAttr) {
    const status = (a.overageStatus || '').toUpperCase();
    const capable = !a.overageCapability || a.overageCapability === 'OVERAGE_CAPABLE';
    const checked = status === 'ENABLED';
    const checkedAt = a.overageCheckedAt ? new Date(a.overageCheckedAt * 1000).toLocaleString() : '-';
    const statusText = status === 'ENABLED' ? t('detail.overageEnabled')
      : status === 'DISABLED' ? t('detail.overageDisabled')
      : t('detail.overageUnknown');
    const disabledAttr = capable ? '' : ' disabled';
    return '<div class="form-group flex items-center gap-2">' +
      '<label class="switch"><input type="checkbox" id="overageSwitchInput-' + idAttr + '" data-detail-action="toggleOverage" data-id="' + idAttr + '" ' + (checked ? 'checked' : '') + disabledAttr + ' /><span class="slider"></span></label>' +
      '<span id="overageSwitchLabel-' + idAttr + '">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      (capable ? '' : '<p class="help-block" style="color:#ef4444">' + escapeHtml(t('detail.overageNotCapable')) + '</p>') +
      '<div class="detail-grid">' +
      detailItem(t('detail.overageStatus'), status || '-') +
      detailItem(t('detail.overageCap'), a.overageCap ? '$' + Number(a.overageCap).toFixed(2) : '-') +
      detailItem(t('detail.overageRate'), a.overageRate ? '$' + Number(a.overageRate).toFixed(4) : '-') +
      detailItem(t('detail.overageCurrent'), a.currentOverages ? '$' + Number(a.currentOverages).toFixed(4) : '$0') +
      detailItem(t('detail.overageCheckedAt'), checkedAt) +
      '</div>';
  }
  async function toggleOverageSwitch(id, inputEl) {
    const desired = inputEl.checked;
    const labelEl = $('overageSwitchLabel-' + id);
    const oldLabel = labelEl ? labelEl.textContent : '';
    inputEl.disabled = true;
    if (labelEl) labelEl.textContent = t('detail.overageSwitching');
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/overage', {
        method: 'POST',
        body: JSON.stringify({ enabled: desired }),
      });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) {
        throw new Error(d.error || t('accounts.overageSwitchFailed'));
      }
      if (labelEl) {
        labelEl.textContent = d.overageStatus === 'ENABLED' ? t('detail.overageEnabled')
          : d.overageStatus === 'DISABLED' ? t('detail.overageDisabled')
          : t('detail.overageUnknown');
      }
      inputEl.checked = d.overageStatus === 'ENABLED';
      await loadAccounts();
    } catch (e) {
      inputEl.checked = !desired;
      if (labelEl) labelEl.textContent = oldLabel;
      toast(t('accounts.overageSwitchFailed') + ': ' + (e.message || e), 'warning');
    } finally {
      inputEl.disabled = false;
    }
  }
  async function refreshAccountOverage(id) {
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/overage', { method: 'GET' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) {
        throw new Error(d.error || t('accounts.overageSwitchFailed'));
      }
      await loadAccounts();
      showDetail(id);
    } catch (e) {
      toast(t('accounts.overageSwitchFailed') + ': ' + (e.message || e), 'warning');
    }
  }
  async function saveProxyURL(id) {
    const url = $('proxyURLInput').value.trim();
    if (url && !/^(socks5|socks5h|http|https):\/\//.test(url)) {
      toast(t('detail.proxyFormatError'), 'warning'); return;
    }
    await putAccount(id, { proxyURL: url }, t('detail.proxySaved'));
  }
  function closeDetailModal() { closeDialog('detailModal'); }

  // Test flow
  function getTestAccount(id) {
    return accountsData.find(a => a.id === id) || null;
  }
  function getTestModelValue() {
    const choice = $('testModelChoice');
    return (choice && choice.value.trim()) || 'claude-sonnet-4';
  }
  function renderTestLog() {
    const c = $('testModalLog');
    if (!c) return;
    if (!testLogs.length) {
      c.innerHTML = '<div class="test-log-empty">' + escapeHtml(t('accounts.testLog.empty')) + '</div>';
      return;
    }
    c.innerHTML = testLogs.map(log =>
      '<div class="test-log-line ' + escapeAttr(log.type || 'info') + '">' +
      '<span class="test-log-time">' + escapeHtml(log.time) + '</span>' +
      '<span class="test-log-message">' + escapeHtml(log.msg) + '</span>' +
      '</div>'
    ).join('');
    c.scrollTop = c.scrollHeight;
  }
  function addTestLog(msg, type) {
    const time = new Date().toLocaleTimeString();
    testLogs.push({ time, msg, type });
    if (testLogs.length > 100) testLogs.shift();
    renderTestLog();
  }
  function clearTestLog() {
    testLogs = [];
    renderTestLog();
  }
  function renderTestModal() {
    const body = $('testBody');
    if (!body) return;
    const acc = getTestAccount(testModalAccountId);
    const idAttr = escapeAttr(testModalAccountId);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : testModalAccountId;
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    const statusText = testModalLoadingModels
      ? t('accounts.testModelsLoading')
      : testModalModelError
        ? t('accounts.testModelsFallback')
        : t('accounts.testModelsReady', testModalModels.length);
    const modelField = testModalLoadingModels
      ? '<div class="test-model-loading">' + escapeHtml(t('accounts.testModelsLoading')) + '</div>'
      : testModalModels.length
        ? '<select id="testModelChoice">' +
        testModalModels.map(m => '<option value="' + escapeAttr(m) + '">' + escapeHtml(m) + '</option>').join('') +
        '</select>'
        : '<input type="text" id="testModelChoice" placeholder="claude-sonnet-4" value="claude-sonnet-4" />';

    body.innerHTML =
      '<div class="test-modal-account">' +
      '<div class="test-modal-account-main">' +
      '<div class="test-modal-email">' + escapeHtml(email) + '</div>' +
      '<div class="test-modal-meta">' +
      '<span>' + escapeHtml(formatAuthMethod(acc && (acc.provider || acc.authMethod))) + '</span>' +
      '<span>' + escapeHtml(proxy) + '</span>' +
      '</div>' +
      '</div>' +
      '<span class="test-modal-status">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      '<div class="test-modal-grid">' +
      '<div class="form-group test-model-field">' +
      '<label for="testModelChoice">' + escapeHtml(t('accounts.selectModel')) + '</label>' +
      modelField +
      '</div>' +
      '<div class="test-log-card">' +
      '<div class="test-log-header">' +
      '<span class="test-log-title">' + escapeHtml(t('accounts.testLog.title')) + '</span>' +
      '<button class="btn btn-xs btn-outline test-log-clear" id="testLogClear" type="button">' + escapeHtml(t('accounts.testLog.clear')) + '</button>' +
      '</div>' +
      '<div class="test-log-content" id="testModalLog"></div>' +
      '</div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="testModalCancelBtn" type="button">' + escapeHtml(t('common.close')) + '</button>' +
      '<button class="btn btn-primary" id="testRunBtn" data-id="' + idAttr + '" type="button" ' + (testModalLoadingModels ? 'disabled' : '') + '>' + escapeHtml(t('accounts.test')) + '</button>' +
      '</div>';

    if (!testModalLoadingModels) enhanceCustomSelects(body);
    renderTestLog();
  }
  async function testAccount(id) {
    testModalAccountId = id;
    testModalModels = [];
    testModalLoadingModels = true;
    testModalModelError = false;
    testModalRunning = false;
    testLogs = [];
    renderTestModal();
    openDialog('testModal');
    try {
      const res = await api('/accounts/' + id + '/models/cached');
      const d = await res.json();
      testModalModels = Array.isArray(d.models) ? d.models.slice().sort() : [];
    } catch (e) {
      testModalModelError = true;
    } finally {
      testModalLoadingModels = false;
      renderTestModal();
    }
  }
  function closeTestModal() {
    closeAllCustomSelects();
    closeDialog('testModal');
  }
  async function runTestAccount(id, model) {
    if (testModalRunning) return;
    testModalRunning = true;
    const modalBtn = $('testRunBtn');
    if (modalBtn) modalBtn.setAttribute('aria-busy', 'true');
    const acc = accountsData.find(a => a.id === id);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : id;
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    addTestLog(t('accounts.testLog.start', email, model, proxy), 'info');
    try {
      const startTime = Date.now();
      const res = await api('/accounts/' + id + '/test', { method: 'POST', body: JSON.stringify({ model }) });
      const elapsed = ((Date.now() - startTime) / 1000).toFixed(1);
      const d = await res.json();
      if (d.success) {
        addTestLog(t('accounts.testLog.success', email, elapsed, d.reply), 'ok');
      } else {
        addTestLog(t('accounts.testLog.failed', email, elapsed, d.error || t('common.unknownError')), 'err');
      }
    } catch (e) {
      addTestLog(t('accounts.testLog.error', email, e.message), 'err');
    }
    testModalRunning = false;
    if (modalBtn) modalBtn.removeAttribute('aria-busy');
  }

  // Settings
  async function loadSettings() {
    const res = await api('/settings');
    const d = await res.json();
    $('requireApiKey').checked = d.requireApiKey;
    $('allowOverUsage').checked = d.allowOverUsage || false;
    $('maxPayloadBytes').value = String(d.maxPayloadBytes || 2000000);
    if ($('publicBaseURL')) $('publicBaseURL').value = d.publicBaseURL || '';
    if ($('limitNoticeMessage')) $('limitNoticeMessage').value = d.limitNoticeMessage || '';
    // Defaults to true when the server omits it, matching config.GetLimitNoticeAsError.
    if ($('limitNoticeAsError')) $('limitNoticeAsError').checked = d.limitNoticeAsError !== false;
    populateForceModelOptions(d.forceModel || '');
    populateIdentityModelOptions(d.identityModel || '');
    await Promise.all([loadThinkingConfig(), loadEndpointConfig(), loadProxyConfig(), loadProxyPool(), loadPromptFilter(), loadApiKeys()]);
    refreshCustomSelects();
  }
  async function loadThinkingConfig() {
    const res = await api('/thinking');
    const d = await res.json();
    $('thinkingSuffix').value = d.suffix || '-thinking';
    $('openaiThinkingFormat').value = d.openaiFormat || 'reasoning_content';
    $('claudeThinkingFormat').value = d.claudeFormat || 'thinking';
  }
  async function saveThinkingConfig() {
    const res = await api('/thinking', {
      method: 'POST', body: JSON.stringify({
        suffix: $('thinkingSuffix').value || '-thinking',
        openaiFormat: $('openaiThinkingFormat').value,
        claudeFormat: $('claudeThinkingFormat').value
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.thinkingSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function loadEndpointConfig() {
    const res = await api('/endpoint');
    const d = await res.json();
    $('preferredEndpoint').value = d.preferredEndpoint || 'auto';
    $('endpointFallback').checked = d.endpointFallback !== false;
  }
  async function saveEndpointConfig() {
    const res = await api('/endpoint', {
      method: 'POST', body: JSON.stringify({
        preferredEndpoint: $('preferredEndpoint').value,
        endpointFallback: $('endpointFallback').checked
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.endpointSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function loadProxyConfig() {
    const res = await api('/proxy');
    const d = await res.json();
    const requireProxyEl = document.getElementById('requireProxyToggle');
    if (requireProxyEl) requireProxyEl.checked = !!d.requireProxy;
    window.__requireProxy = !!d.requireProxy;
    window.__globalProxyURL = d.proxyURL || '';
    renderAppliedProxies();
    renderAccounts();
    const url = d.proxyURL || '';
    if (!url) {
      $('proxyType').value = 'none';
      $('proxyFields').classList.add('hidden');
      return;
    }
    try {
      const u = new URL(url);
      const scheme = u.protocol.replace(':', '');
      $('proxyType').value = scheme.startsWith('socks5') ? 'socks5' : 'http';
      $('proxyHost').value = u.hostname;
      $('proxyPort').value = u.port;
      $('proxyUsername').value = decodeURIComponent(u.username);
      $('proxyPassword').value = decodeURIComponent(u.password);
      $('proxyFields').classList.remove('hidden');
    } catch (e) {
      $('proxyType').value = 'none';
      $('proxyFields').classList.add('hidden');
    }
  }
  function onProxyTypeChange() {
    const type = $('proxyType').value;
    $('proxyFields').classList.toggle('hidden', type === 'none');
  }
  async function saveProxyConfig() {
    const type = $('proxyType').value;
    let url = '';
    if (type !== 'none') {
      const host = $('proxyHost').value.trim();
      const port = $('proxyPort').value.trim();
      if (!host || !port) { toast(t('settings.proxyHostRequired'), 'warning'); return; }
      const u = $('proxyUsername').value.trim();
      const p = $('proxyPassword').value.trim();
      const auth = u ? (p ? encodeURIComponent(u) + ':' + encodeURIComponent(p) + '@' : encodeURIComponent(u) + '@') : '';
      url = type + '://' + auth + host + ':' + port;
    }
    const requireProxyEl = document.getElementById('requireProxyToggle');
    window.__requireProxy = requireProxyEl ? requireProxyEl.checked : false;
    const res = await api('/proxy', { method: 'POST', body: JSON.stringify({ proxyURL: url, requireProxy: requireProxyEl ? requireProxyEl.checked : false }) });
    const d = await res.json();
    if (d.success) toast(t('settings.proxySaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function savePublicBaseURL() {
    const url = $('publicBaseURL').value.trim();
    try {
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ publicBaseURL: url }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      toast(t('settings.publicBaseURLSaved'), 'success');
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  async function saveLimitNotice() {
    const msg = $('limitNoticeMessage').value.trim();
    const asError = $('limitNoticeAsError') ? !!$('limitNoticeAsError').checked : true;
    try {
      const res = await api('/settings', {
        method: 'POST',
        body: JSON.stringify({ limitNoticeMessage: msg, limitNoticeAsError: asError }),
      });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      toast(t('settings.limitNoticeSaved'), 'success');
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  // populateForceModelOptions fills the Force-Model <select>. First option ("") = off
  // (each request keeps its own model); the rest force every request to that model.
  function populateForceModelOptions(selected) {
    const sel = $('forceModel');
    if (!sel) return;
    const opts = KIRO_MODEL_OPTIONS.slice();
    if (selected && !opts.includes(selected)) opts.unshift(selected);
    let html = '<option value="">' + escapeHtml(t('settings.forceModelOff')) + '</option>';
    html += opts.map(m => '<option value="' + escapeAttr(m) + '"' + (m === selected ? ' selected' : '') + '>' + escapeHtml(m) + '</option>').join('');
    sel.innerHTML = html;
    sel.value = selected || '';
  }
  async function saveForceModel() {
    const sel = $('forceModel');
    const model = sel ? sel.value.trim() : '';
    try {
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ forceModel: model }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      toast(t('settings.forceModelSaved'), 'success');
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  // populateIdentityModelOptions fills the Identity-Model <select>. First option ("") = off
  // (assistant self-identifies from its own training); the rest inject a "You are <model>"
  // line into the system prompt so the assistant self-reports as that model.
  function populateIdentityModelOptions(selected) {
    const sel = $('identityModel');
    if (!sel) return;
    const opts = KIRO_MODEL_OPTIONS.slice();
    if (selected && !opts.includes(selected)) opts.unshift(selected);
    let html = '<option value="">' + escapeHtml(t('settings.identityModelOff')) + '</option>';
    html += opts.map(m => '<option value="' + escapeAttr(m) + '"' + (m === selected ? ' selected' : '') + '>' + escapeHtml(m) + '</option>').join('');
    sel.innerHTML = html;
    sel.value = selected || '';
    updateIdentityModelWarning();
  }
  // Show the "please be a dev with a conscience" warning only when an identity
  // model is actually selected (i.e. the feature is on).
  function updateIdentityModelWarning() {
    const sel = $('identityModel');
    const warn = $('identityModelWarn');
    if (!sel || !warn) return;
    warn.classList.toggle('hidden', !sel.value.trim());
  }
  async function saveIdentityModel() {
    const sel = $('identityModel');
    const model = sel ? sel.value.trim() : '';
    try {
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ identityModel: model }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      toast(t('settings.identityModelSaved'), 'success');
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  async function importProxies() {
    const raw = $('proxyImportList').value.trim();
    if (!raw) { toast(t('proxyImport.listRequired'), 'warning'); return; }
    const autoTest = $('proxyImportAutoTest').checked;
    const dryRun = $('proxyImportDryRun').checked;
    const target = $('proxyImportTarget').value;
    const btn = $('proxyImportBtn');
    btn.disabled = true;
    const dismiss = toast(t('proxyImport.processing'), 'info', { duration: 0 });
    try {
      const res = await api('/proxy/import', {
        method: 'POST',
        body: JSON.stringify({ proxies: raw, autoTest, dryRun, target })
      });
      const d = await res.json();
      dismiss();
      if (!d.success) {
        toast(t('common.failed') + ': ' + (d.error || ''), 'error');
        return;
      }
      renderProxyImportResults(d);
      toast(t('proxyImport.summary', d.assigned || 0, d.reachable || 0, d.total || 0), d.reachable < d.total ? 'warning' : 'success');
      if (target === 'pool') loadProxyPool();
      loadAccounts();
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    } finally {
      btn.disabled = false;
    }
  }
  function renderProxyImportResults(d) {
    const box = $('proxyImportResults');
    const results = d.results || [];
    if (!results.length) {
      box.innerHTML = '<p class="help-block">' + escapeHtml(t('proxyImport.noResults')) + '</p>';
      return;
    }
    const ok = results.filter(r => !r.error && !(r.tested && !r.testPassed)).length;
    const summary = '<div class="proxy-import-summary">' +
      escapeHtml(t('proxyImport.summary', d.assigned || 0, d.reachable || 0, d.total || 0)) +
      ' · ' + escapeHtml(t('proxyImport.okCount', ok, results.length)) + '</div>';
    const rows = results.map(r => {
      let status, cls;
      if (r.error) { status = '✗ ' + r.error; cls = 'error-text'; }
      else if (r.tested && !r.testPassed) { status = '⚠ ' + t('proxyImport.testFailed'); cls = 'warning-text'; }
      else if (r.tested && r.testPassed) { status = '✓ ' + t('proxyImport.testOk'); cls = 'success-text'; }
      else if (r.assigned) { status = '✓ ' + t('proxyImport.assigned'); cls = 'success-text'; }
      else if (r.reachable) { status = '✓ ' + t('proxyImport.reachable'); cls = 'success-text'; }
      else { status = '✗'; cls = 'error-text'; }
      const target = r.assignedEmail ? ' → ' + escapeHtml(r.assignedEmail) : '';
      const scheme = r.scheme ? '[' + escapeHtml(r.scheme) + '] ' : '';
      const label = escapeHtml(r.maskedUrl || r.raw);
      return '<div class="proxy-import-row">' +
        '<span class="font-mono text-xs proxy-import-url" title="' + escapeAttr((r.scheme ? r.scheme + ' ' : '') + (r.maskedUrl || r.raw)) + '">' + scheme + label + target + '</span>' +
        '<span class="' + cls + ' proxy-import-status">' + escapeHtml(status) + '</span>' +
        '</div>';
    }).join('');
    box.innerHTML = summary + rows;
  }
  async function loadProxyPool() {
    const box = $('proxyPoolList');
    if (!box) return;
    try {
      const res = await api('/proxy/pool');
      const d = await res.json();
      renderProxyPool(d.pool || []);
    } catch (e) {
      box.innerHTML = '<p class="help-block error-text">' + escapeHtml(t('common.failed')) + '</p>';
    }
  }
  function renderProxyPool(pool) {
    const box = $('proxyPoolList');
    if (!box) return;
    if (!pool.length) {
      box.innerHTML = '<p class="help-block">' + escapeHtml(t('settings.proxyPoolEmpty')) + '</p>';
      return;
    }
    box.innerHTML = pool.map(p => {
      const masked = maskProxyForDisplay(p.url) || '•••';
      let dotCls, stateLabel;
      if (p.disabledPermanent) { dotCls = 'disabled'; stateLabel = t('settings.proxyPoolDisabled'); }
      else if (p.healthy) { dotCls = 'healthy'; stateLabel = t('settings.proxyPoolHealthy'); }
      else { dotCls = 'unhealthy'; stateLabel = t('settings.proxyPoolUnhealthy'); }
      const fail = (p.failCount || 0) > 0 ? '<span class="applied-proxy-count">' + escapeHtml(t('settings.proxyPoolFailCount', p.failCount)) + '</span>' : '';
      const toggleLabel = p.disabledPermanent ? t('common.enable') : t('common.disable');
      return '<div class="proxy-pool-row">' +
        '<span class="proxy-health-dot ' + dotCls + '" title="' + escapeAttr(stateLabel) + '"></span>' +
        '<span class="font-mono text-xs" title="' + escapeAttr(masked) + '">' + escapeHtml(masked) + '</span>' +
        fail +
        '<span class="proxy-pool-actions">' +
        '<button class="btn btn-sm" data-pool-action="toggle" data-url="' + escapeAttr(p.url) + '" data-disabled="' + (p.disabledPermanent ? '0' : '1') + '">' + escapeHtml(toggleLabel) + '</button>' +
        '<button class="btn btn-sm btn-danger" data-pool-action="remove" data-url="' + escapeAttr(p.url) + '">' + escapeHtml(t('settings.proxyPoolRemove')) + '</button>' +
        '</span>' +
        '</div>';
    }).join('');
  }
  async function removeProxyFromPool(url) {
    try {
      const res = await api('/proxy/pool', { method: 'DELETE', body: JSON.stringify({ url }) });
      const d = await res.json().catch(() => ({}));
      if (res.ok && d.success !== false) loadProxyPool();
      else toast(t('common.failed') + ': ' + (d.error || ''), 'error');
    } catch (e) {
      toast(t('common.failed'), 'error');
    }
  }
  async function toggleProxyPool(url, disabled) {
    try {
      const res = await api('/proxy/pool/toggle', { method: 'POST', body: JSON.stringify({ url, disabled }) });
      const d = await res.json().catch(() => ({}));
      if (res.ok && d.success !== false) loadProxyPool();
      else toast(t('common.failed') + ': ' + (d.error || ''), 'error');
    } catch (e) {
      toast(t('common.failed'), 'error');
    }
  }
  async function saveRequireApiKey() {
    try {
      const requireApiKey = $('requireApiKey').checked;
      if (requireApiKey) {
        const hasEnabledKey = Array.isArray(apiKeysCache) && apiKeysCache.some(k => k && k.enabled);
        if (!hasEnabledKey) {
          if (!confirm(t('apiKeys.requireWithoutEnabledKeyWarning'))) {
            $('requireApiKey').checked = false;
            return;
          }
        }
      }
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ requireApiKey }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      toast(t('detail.saved'), 'success');
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  async function saveOverUsageConfig() {
    const allowOverUsage = $('allowOverUsage').checked;
    const maxPayloadBytes = parseInt($('maxPayloadBytes').value, 10);
    await api('/settings', { method: 'POST', body: JSON.stringify({ allowOverUsage, maxPayloadBytes }) });
    toast(t('settings.overUsageSaved'), 'success');
  }
  async function changePassword() {
    const np = $('newPassword').value;
    if (!np) return toast(t('settings.passwordRequired'), 'warning');
    try {
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ password: np }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      // The current session token stays valid after a password change (sessions are
      // independent of the password), so no re-login is needed.
      toast(t('settings.passwordChanged'), 'success');
      $('newPassword').value = '';
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  async function resetStats() {
    const ok = await confirmAction(t('settings.confirmReset'), {
      title: t('settings.statistics'),
      confirmText: t('settings.resetStats'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/stats/reset', { method: 'POST' });
      if (!res.ok) throw new Error(t('common.failed'));
      loadStats();
      toastPrimary(t('settings.statsReset'));
    } catch (e) {
      toastError((e && e.message) || t('common.failed'));
    }
  }
  // Multi API Key management
  let apiKeysCache = [];
  let apiKeyEditingId = '';
  let apiKeyModalSubmitting = false;
  const selectedApiKeyIds = new Set();

  async function loadApiKeys() {
    const list = $('apiKeysList');
    if (!list) return;
    try {
      const res = await api('/api-keys');
      if (!res.ok) throw new Error('http ' + res.status);
      const d = await res.json();
      apiKeysCache = Array.isArray(d.apiKeys) ? d.apiKeys : [];
      renderApiKeys();
    } catch (e) {
      apiKeysCache = [];
      list.innerHTML = '<div class="muted-text" style="padding:0.5rem 0;">' + escapeHtml(t('apiKeys.loadFailed')) + '</div>';
    }
  }

  function formatNumber(n) {
    if (n == null || isNaN(n)) return '0';
    if (Math.abs(n) >= 1 && Math.floor(n) === n) return Number(n).toLocaleString('en-US');
    return Number(n).toLocaleString('en-US', { maximumFractionDigits: 4 });
  }

  function usageBar(used, limit) {
    if (!limit || limit <= 0) return '';
    const ratio = Math.max(0, Math.min(1, used / limit));
    const pct = (ratio * 100).toFixed(1);
    let color = '#3b82f6';
    if (ratio >= 0.95) color = '#ef4444';
    else if (ratio >= 0.8) color = '#f59e0b';
    return '<div style="height:6px;background:rgba(127,127,127,0.2);border-radius:3px;overflow:hidden;margin-top:4px;">' +
      '<div style="height:100%;width:' + pct + '%;background:' + color + ';transition:width 0.3s;"></div>' +
      '</div>';
  }

  function usageLine(label, used, limit, options) {
    options = options || {};
    const fmt = options.fmt || formatNumber;
    if (!limit || limit <= 0) {
      return '<div class="text-xs muted-text">' + escapeHtml(label) + ': ' + escapeHtml(fmt(used)) + ' / ' + escapeHtml(t('apiKeys.unlimited')) + '</div>';
    }
    return '<div class="text-xs muted-text">' + escapeHtml(label) + ': ' + escapeHtml(fmt(used)) + ' / ' + escapeHtml(fmt(limit)) + '</div>' + usageBar(used, limit);
  }

  function renderApiKeyStats() {
    const el = $('apiKeyStats');
    if (!el) return;
    const now = Date.now() / 1000;
    const total = apiKeysCache.length;
    const disabled = apiKeysCache.filter(k => !k.enabled).length;
    const expired = apiKeysCache.filter(k => normalizeUnixSeconds(k.expiresAt) && normalizeUnixSeconds(k.expiresAt) <= now).length;
    const active = apiKeysCache.filter(k => k.enabled && (!normalizeUnixSeconds(k.expiresAt) || normalizeUnixSeconds(k.expiresAt) > now)).length;
    el.innerHTML =
      '<div class="stat-card"><div class="stat-card-title">' + escapeHtml(t('apiKeys.statTotal')) + '</div><div class="stat-value">' + escapeHtml(formatNumber(total)) + '</div></div>' +
      '<div class="stat-card"><div class="stat-card-title">' + escapeHtml(t('apiKeys.statActive')) + '</div><div class="stat-value">' + escapeHtml(formatNumber(active)) + '</div></div>' +
      '<div class="stat-card"><div class="stat-card-title">' + escapeHtml(t('apiKeys.statExpired')) + '</div><div class="stat-value stat-value--danger">' + escapeHtml(formatNumber(expired)) + '</div></div>' +
      '<div class="stat-card"><div class="stat-card-title">' + escapeHtml(t('apiKeys.statDisabled')) + '</div><div class="stat-value">' + escapeHtml(formatNumber(disabled)) + '</div></div>';
  }

  function renderApiKeyBulkBar() {
    const bar = $('apiKeyBulkBar');
    const count = $('apiKeyBulkCount');
    if (!bar || !count) return;
    const n = selectedApiKeyIds.size;
    bar.classList.toggle('hidden', n === 0);
    count.textContent = t('apiKeys.selected', n);
    // Keep the select-all checkbox in sync: checked when every key is selected,
    // indeterminate when only some are.
    const cb = $('apiKeySelectAll');
    if (cb) {
      const total = apiKeysCache.length;
      cb.checked = total > 0 && n === total;
      cb.indeterminate = n > 0 && n < total;
    }
  }

  // toggleApiKeySelectAll selects or clears every created API key at once.
  function toggleApiKeySelectAll(checked) {
    selectedApiKeyIds.clear();
    if (checked) apiKeysCache.forEach(k => { if (k.id) selectedApiKeyIds.add(k.id); });
    renderApiKeys();
  }

  function renderApiKeys() {
    const list = $('apiKeysList');
    if (!list) return;
    const ids = new Set(apiKeysCache.map(k => k.id));
    selectedApiKeyIds.forEach(id => { if (!ids.has(id)) selectedApiKeyIds.delete(id); });
    renderApiKeyStats();
    renderApiKeyBulkBar();
    if (!apiKeysCache.length) {
      list.innerHTML = '<div class="muted-text" style="padding:0.5rem 0;">' + escapeHtml(t('apiKeys.empty')) + '</div>';
      return;
    }
    const html = apiKeysCache.map(item => {
      const rawId = item.id || '';
      const id = escapeAttr(rawId);
      const checked = selectedApiKeyIds.has(rawId) ? ' checked' : '';
      const name = item.name ? escapeHtml(item.name) : '<span class="muted-text">' + escapeHtml(t('apiKeys.unnamed')) + '</span>';
      const masked = escapeHtml(item.keyMasked || '');
      const migrated = item.migrated
        ? '<span class="text-xs" style="background:rgba(59,130,246,0.15);color:#3b82f6;padding:1px 6px;border-radius:4px;">' + escapeHtml(t('apiKeys.migrated')) + '</span>'
        : '';
      const disabled = !item.enabled
        ? '<span class="text-xs" style="background:rgba(239,68,68,0.15);color:#ef4444;padding:1px 6px;border-radius:4px;">' + escapeHtml(t('apiKeys.disabled')) + '</span>'
        : '';
      const expiredBadge = apiKeyExpiryBadge(item.expiresAt);
      const expiryLine = apiKeyExpiryLine(item.expiresAt);
      const tokensLine = usageLine(t('apiKeys.tokens'), item.tokensUsed || 0, item.tokenLimit || 0);
      const creditsLine = usageLine(t('apiKeys.credits'), item.creditsUsed || 0, item.creditLimit || 0);
      const requestsLine = '<div class="text-xs muted-text">' + escapeHtml(t('apiKeys.requests')) + ': ' + escapeHtml(formatNumber(item.requestsCount || 0)) + '</div>';
      // Lifetime grand total — survives "Reset Usage", only cleared by "Reset All".
      const lifetimeLine = '<div class="text-xs muted-text">' + escapeHtml(t('apiKeys.lifetime')) + ': ' +
        escapeHtml(formatNumber(item.lifetimeTokens || 0)) + ' ' + escapeHtml(t('apiKeys.tokens')) + ' · ' +
        escapeHtml(formatNumber(item.lifetimeRequests || 0)) + ' ' + escapeHtml(t('apiKeys.requests')) + '</div>';
      const rpmValue = item.rpmLimit && item.rpmLimit > 0 ? formatNumber(item.rpmLimit) + ' ' + t('apiKeys.rpmUnit') : t('apiKeys.unlimited');
      const rpmLine = '<div class="text-xs muted-text">' + escapeHtml(t('apiKeys.limitRPM')) + ': ' + escapeHtml(rpmValue) + '</div>';
      const hasAllowlist = Array.isArray(item.ipAllowlist) && item.ipAllowlist.length > 0;
      // A non-empty allowlist takes precedence over the IP count limit, so show whichever is active.
      const ipLine = hasAllowlist
        ? '<div class="text-xs muted-text">' + escapeHtml(t('apiKeys.ipAllowlist')) + ': ' + escapeHtml(item.ipAllowlist.join(', ')) + '</div>'
        : '<div class="text-xs muted-text">' + escapeHtml(t('apiKeys.limitIP')) + ': ' + escapeHtml(item.ipLimit && item.ipLimit > 0 ? formatNumber(item.ipLimit) : t('apiKeys.unlimited')) + '</div>';
      // Bound-account line: map ids to a readable email/nickname via the account cache.
      const boundIds = Array.isArray(item.boundAccountIds) ? item.boundAccountIds : [];
      const acctById = {};
      (Array.isArray(accountsData) ? accountsData : []).forEach(a => { acctById[a.id] = a; });
      const boundNames = boundIds.map(bid => {
        const a = acctById[bid];
        return a ? (a.email || a.nickname || bid) : bid;
      });
      const boundValue = boundNames.length ? boundNames.join(', ') : t('apiKeys.boundAccountsNone');
      const boundLine = '<div class="text-xs muted-text">' + escapeHtml(t('apiKeys.boundAccounts')) + ': ' + escapeHtml(boundValue) + '</div>';
      // Model allowlist line: empty = the key accepts the client's requested model.
      const modelList = Array.isArray(item.models) ? item.models.filter(Boolean) : [];
      const modelValue = modelList.length ? modelList.join(', ') : t('apiKeys.modelDefault');
      const modelsLine = '<div class="text-xs muted-text">' + escapeHtml(t('apiKeys.model')) + ': ' + escapeHtml(modelValue) + '</div>';
      // Customer attribution line — only rendered when at least one field is set, so
      // deployments that never use these labels keep the card compact.
      const customerParts = [];
      if (item.ownerRef) customerParts.push(t('apiKeys.ownerRef') + ': ' + item.ownerRef);
      if (item.orderRef) customerParts.push(t('apiKeys.orderRef') + ': ' + item.orderRef);
      if (item.note) customerParts.push(t('apiKeys.note') + ': ' + item.note);
      const customerLine = customerParts.length
        ? '<div class="text-xs muted-text">' + escapeHtml(customerParts.join(' · ')) + '</div>'
        : '';
      return '<div class="card" data-apikey-id="' + id + '" style="margin-top:0.5rem;padding:0.75rem;">' +
        '<div class="flex items-center gap-2" style="flex-wrap:wrap;justify-content:space-between;">' +
          '<div class="flex items-center gap-2" style="flex-wrap:wrap;">' +
            '<input type="checkbox" data-apikey-action="select" data-id="' + id + '"' + checked + ' />' +
            '<span class="font-semibold">' + name + '</span>' +
            migrated +
            disabled +
            expiredBadge +
            '<span class="text-xs muted-text font-mono">' + masked + '</span>' +
          '</div>' +
          '<div class="flex items-center gap-2">' +
            '<label class="switch" title="' + escapeAttr(item.enabled ? t('accounts.disable') : t('accounts.enable')) + '">' +
              '<input type="checkbox" data-apikey-action="toggle" data-id="' + id + '"' + (item.enabled ? ' checked' : '') + ' />' +
              '<span class="slider"></span>' +
            '</label>' +
            '<button class="btn btn-outline btn-sm" type="button" data-apikey-action="portal" data-id="' + id + '" title="' + escapeAttr(t('apiKeys.portalLinkHint')) + '"><i class="fa-solid fa-link"></i></button>' +
            '<button class="btn btn-outline btn-sm" type="button" data-apikey-action="edit" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionEdit')) + '</button>' +
            '<button class="btn btn-outline btn-sm" type="button" data-apikey-action="reset" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionReset')) + '</button>' +
            '<button class="btn btn-outline btn-sm" type="button" data-apikey-action="reset-all" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionResetAll')) + '</button>' +
            '<button class="btn btn-danger btn-sm" type="button" data-apikey-action="delete" data-id="' + id + '">' + escapeHtml(t('apiKeys.actionDelete')) + '</button>' +
          '</div>' +
        '</div>' +
        '<div style="margin-top:0.5rem;display:grid;gap:0.35rem;">' +
          tokensLine +
          creditsLine +
          requestsLine +
          lifetimeLine +
          rpmLine +
          ipLine +
          expiryLine +
          boundLine +
          modelsLine +
          customerLine +
        '</div>' +
      '</div>';
    }).join('');
    list.innerHTML = html;
  }

  // apiKeyBoundSelected is the live set of bound-account ids chosen in the modal. It
  // persists across search-filter re-renders (a checkbox hidden by the filter must not
  // lose its selection), so it — not the DOM — is the source of truth for what gets saved.
  let apiKeyBoundSelected = new Set();

  // initApiKeyBoundAccounts seeds the selection set from the key being edited and paints
  // the (unfiltered) picker. Call once when opening the modal.
  function initApiKeyBoundAccounts(selectedIds) {
    apiKeyBoundSelected = new Set(selectedIds || []);
    const search = $('apiKeyForm_boundSearch');
    if (search) search.value = '';
    renderApiKeyBoundAccounts('');
  }

  // renderBoundAccounts paints one checkbox per account into containerId, checked from
  // selectedSet (not the DOM), filtered by the search term. Disabled/banned accounts are
  // still listed (so an operator can bind ahead of re-enabling) but labelled. Shared by the
  // single-key and bulk-create modals, each of which owns its own selection set.
  function renderBoundAccounts(containerId, selectedSet, filter) {
    const box = $(containerId);
    if (!box) return;
    const accts = Array.isArray(accountsData) ? accountsData : [];
    if (!accts.length) {
      box.innerHTML = '<div class="muted-text text-xs" style="padding:0.25rem 0;">' + escapeHtml(t('apiKeys.boundAccountsEmpty')) + '</div>';
      return;
    }
    const kw = (filter || '').trim().toLowerCase();
    const matched = accts.filter(a => {
      if (!kw) return true;
      return (a.email || '').toLowerCase().includes(kw) ||
        (a.nickname || '').toLowerCase().includes(kw) ||
        (a.id || '').toLowerCase().includes(kw);
    });
    if (!matched.length) {
      box.innerHTML = '<div class="muted-text text-xs" style="padding:0.25rem 0;">' + escapeHtml(t('apiKeys.boundAccountsNoMatch')) + '</div>';
      return;
    }
    box.innerHTML = matched.map(a => {
      const id = escapeHtml(a.id);
      const label = escapeHtml(a.email || a.nickname || a.id);
      const off = !a.enabled || (a.banStatus && a.banStatus !== 'ACTIVE')
        ? ' <span class="muted-text text-xs">(' + escapeHtml(t('apiKeys.disabled')) + ')</span>'
        : '';
      const checked = selectedSet.has(a.id) ? ' checked' : '';
      return '<label class="flex items-center gap-2" style="padding:2px 0;">' +
        '<input type="checkbox" class="apikey-bound-account" value="' + id + '"' + checked + ' />' +
        '<span class="text-sm">' + label + off + '</span>' +
        '</label>';
    }).join('');
  }

  function renderApiKeyBoundAccounts(filter) {
    renderBoundAccounts('apiKeyForm_boundAccounts', apiKeyBoundSelected, filter);
  }

  // onApiKeyBoundToggle keeps apiKeyBoundSelected in sync when a checkbox flips.
  function onApiKeyBoundToggle(id, checked) {
    if (checked) apiKeyBoundSelected.add(id);
    else apiKeyBoundSelected.delete(id);
  }

  // selectedBoundAccountIds returns the chosen ids from the persistent set (not the DOM,
  // which only holds the currently-filtered subset).
  function selectedBoundAccountIds() {
    return Array.from(apiKeyBoundSelected);
  }

  // Bulk-create modal keeps its own selection set, mirroring the single-key modal above.
  let apiKeyBulkBoundSelected = new Set();

  function initApiKeyBulkBoundAccounts(selectedIds) {
    apiKeyBulkBoundSelected = new Set(selectedIds || []);
    const search = $('apiKeyBulk_boundSearch');
    if (search) search.value = '';
    renderApiKeyBulkBoundAccounts('');
  }

  function renderApiKeyBulkBoundAccounts(filter) {
    renderBoundAccounts('apiKeyBulk_boundAccounts', apiKeyBulkBoundSelected, filter);
  }

  // Per-key model allowlist pickers. Each modal owns a Set of ticked model ids; empty =
  // "use the client's requested model". A ticked model passes through; a client model not
  // in the set is remapped upstream to the first ticked entry (see applyModelOverride).
  let apiKeyModelsSelected = new Set();
  let apiKeyBulkModelsSelected = new Set();

  // renderModelPicker paints one checkbox per canonical Kiro model into containerId,
  // checked from selectedSet. Any already-selected model that isn't canonical is listed
  // first so an existing value is never silently dropped.
  function renderModelPicker(containerId, selectedSet) {
    const box = $(containerId);
    if (!box) return;
    const opts = KIRO_MODEL_OPTIONS.slice();
    selectedSet.forEach(m => { if (m && !opts.includes(m)) opts.unshift(m); });
    box.innerHTML = opts.map(m => {
      const val = escapeAttr(m);
      const checked = selectedSet.has(m) ? ' checked' : '';
      return '<label class="flex items-center gap-2" style="padding:2px 0;">' +
        '<input type="checkbox" class="apikey-model-option" value="' + val + '"' + checked + ' />' +
        '<span class="text-sm">' + escapeHtml(m) + '</span>' +
        '</label>';
    }).join('');
  }

  // initApiKeyModels seeds a picker's selection set from the key being edited and paints it.
  function initApiKeyModels(containerId, selectedSet, selectedList) {
    const set = new Set(Array.isArray(selectedList) ? selectedList.filter(Boolean) : []);
    if (containerId === 'apiKeyForm_models') apiKeyModelsSelected = set;
    else apiKeyBulkModelsSelected = set;
    renderModelPicker(containerId, set);
  }

  function openApiKeyModal(entry) {
    apiKeyEditingId = entry ? (entry.id || '') : '';
    const titleEl = $('apiKeyModalTitle');
    titleEl.textContent = t(apiKeyEditingId ? 'apiKeys.modalTitleEdit' : 'apiKeys.modalTitleCreate');
    $('apiKeyForm_name').value = entry ? (entry.name || '') : '';
    const keyEl = $('apiKeyForm_key');
    // Key is always editable. On edit we leave it blank and show the current masked value
    // as a placeholder — typing a new value changes the key, leaving it blank keeps it.
    keyEl.readOnly = false;
    keyEl.value = '';
    if (apiKeyEditingId) {
      keyEl.placeholder = t('apiKeys.formKeyEditPlaceholder', entry.keyMasked || '');
    } else {
      keyEl.placeholder = t('apiKeys.formKeyPlaceholder');
    }
    $('apiKeyForm_enabled').checked = entry ? !!entry.enabled : true;
    $('apiKeyForm_tokenLimit').value = entry ? String(entry.tokenLimit || 0) : '0';
    $('apiKeyForm_creditLimit').value = entry ? String(entry.creditLimit || 0) : '0';
    $('apiKeyForm_expiresAt').value = (entry && entry.expiresAt) ? unixToLocalInput(normalizeUnixSeconds(entry.expiresAt)) : '';
    if ($('apiKeyForm_rpmLimit')) $('apiKeyForm_rpmLimit').value = entry ? String(entry.rpmLimit || 0) : '0';
    if ($('apiKeyForm_ipLimit')) $('apiKeyForm_ipLimit').value = entry ? String(entry.ipLimit || 0) : '0';
    if ($('apiKeyForm_ipAllowlist')) $('apiKeyForm_ipAllowlist').value = (entry && Array.isArray(entry.ipAllowlist)) ? entry.ipAllowlist.join('\n') : '';
    if ($('apiKeyForm_tpmLimit')) $('apiKeyForm_tpmLimit').value = entry ? String(entry.tpmLimit || 0) : '0';
    if ($('apiKeyForm_ownerRef')) $('apiKeyForm_ownerRef').value = entry ? (entry.ownerRef || '') : '';
    if ($('apiKeyForm_orderRef')) $('apiKeyForm_orderRef').value = entry ? (entry.orderRef || '') : '';
    if ($('apiKeyForm_note')) $('apiKeyForm_note').value = entry ? (entry.note || '') : '';
    initApiKeyModels('apiKeyForm_models', apiKeyModelsSelected, entry && Array.isArray(entry.models) ? entry.models : []);
    initApiKeyBoundAccounts(entry && Array.isArray(entry.boundAccountIds) ? entry.boundAccountIds : []);
    apiKeyModalSubmitting = false;
    $('apiKeyModalSaveBtn').disabled = false;
    openDialog('apiKeyModal');
  }

  function closeApiKeyModal() {
    closeDialog('apiKeyModal');
    apiKeyEditingId = '';
    apiKeyModalSubmitting = false;
    $('apiKeyModalSaveBtn').disabled = false;
  }

  function openApiKeyBulkModal() {
    $('apiKeyBulk_count').value = '10';
    $('apiKeyBulk_namePrefix').value = '';
    $('apiKeyBulk_enabled').checked = true;
    $('apiKeyBulk_tokenLimit').value = '0';
    $('apiKeyBulk_creditLimit').value = '0';
    $('apiKeyBulk_expiresAt').value = '';
    if ($('apiKeyBulk_rpmLimit')) $('apiKeyBulk_rpmLimit').value = '0';
    if ($('apiKeyBulk_ipLimit')) $('apiKeyBulk_ipLimit').value = '0';
    if ($('apiKeyBulk_ipAllowlist')) $('apiKeyBulk_ipAllowlist').value = '';
    if ($('apiKeyBulk_tpmLimit')) $('apiKeyBulk_tpmLimit').value = '0';
    initApiKeyModels('apiKeyBulk_models', apiKeyBulkModelsSelected, []);
    initApiKeyBulkBoundAccounts([]);
    $('apiKeyBulkSaveBtn').disabled = false;
    openDialog('apiKeyBulkModal');
  }

  function closeApiKeyBulkModal() {
    closeDialog('apiKeyBulkModal');
    $('apiKeyBulkSaveBtn').disabled = false;
  }

  async function createBulkApiKeys() {
    const btn = $('apiKeyBulkSaveBtn');
    btn.disabled = true;
    try {
      const count = parseInt($('apiKeyBulk_count').value, 10);
      if (isNaN(count) || count < 1 || count > 100) throw new Error(t('apiKeys.bulkCountError'));
      const tokenLimit = parseInt($('apiKeyBulk_tokenLimit').value, 10);
      const creditLimit = parseFloat($('apiKeyBulk_creditLimit').value);
      const rpmLimit = parseInt($('apiKeyBulk_rpmLimit') ? $('apiKeyBulk_rpmLimit').value : '0', 10);
      const ipLimit = parseInt($('apiKeyBulk_ipLimit') ? $('apiKeyBulk_ipLimit').value : '0', 10);
      const tpmLimit = parseInt($('apiKeyBulk_tpmLimit') ? $('apiKeyBulk_tpmLimit').value : '0', 10);
      const ipAllowlist = $('apiKeyBulk_ipAllowlist')
        ? $('apiKeyBulk_ipAllowlist').value.split(/[\r\n,]+/).map(s => s.trim()).filter(Boolean)
        : [];
      const payload = {
        count,
        namePrefix: $('apiKeyBulk_namePrefix').value.trim(),
        enabled: $('apiKeyBulk_enabled').checked,
        tokenLimit: isNaN(tokenLimit) || tokenLimit < 0 ? 0 : tokenLimit,
        creditLimit: isNaN(creditLimit) || creditLimit < 0 ? 0 : creditLimit,
        expiresAt: localInputToUnix($('apiKeyBulk_expiresAt').value),
        rpmLimit: isNaN(rpmLimit) || rpmLimit < 0 ? 0 : rpmLimit,
        ipLimit: isNaN(ipLimit) || ipLimit < 0 ? 0 : ipLimit,
        ipAllowlist: ipAllowlist,
        tpmLimit: isNaN(tpmLimit) || tpmLimit < 0 ? 0 : tpmLimit,
        boundAccountIds: Array.from(apiKeyBulkBoundSelected),
        models: Array.from(apiKeyBulkModelsSelected)
      };
      const res = await api('/api-keys/bulk', { method: 'POST', body: JSON.stringify(payload) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      closeApiKeyBulkModal();
      await loadApiKeys();
      if (Array.isArray(d.keys) && d.keys.length) showNewApiKey(d.keys.join('\n'));
      toast(t('apiKeys.bulkCreated', d.count || count), 'success');
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
      btn.disabled = false;
    }
  }

  async function bulkDeleteApiKeys() {
    const ids = Array.from(selectedApiKeyIds);
    if (!ids.length) return;
    const ok = await confirmAction(t('apiKeys.confirmBulkDelete', ids.length), {
      title: t('apiKeys.bulkDelete'),
      confirmText: t('apiKeys.bulkDelete'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/bulk', { method: 'DELETE', body: JSON.stringify({ ids }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      selectedApiKeyIds.clear();
      toast(t('apiKeys.bulkDeleted', d.deleted || ids.length), 'success');
      await loadApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  async function submitApiKeyModal() {
    if (apiKeyModalSubmitting) return;
    apiKeyModalSubmitting = true;
    const saveBtn = $('apiKeyModalSaveBtn');
    saveBtn.disabled = true;
    try {
      const name = $('apiKeyForm_name').value.trim();
      const enabled = $('apiKeyForm_enabled').checked;
      const tokenLimit = parseInt($('apiKeyForm_tokenLimit').value, 10);
      const creditLimit = parseFloat($('apiKeyForm_creditLimit').value);
      const expiresAt = localInputToUnix($('apiKeyForm_expiresAt').value);
      const rpmLimit = parseInt($('apiKeyForm_rpmLimit') ? $('apiKeyForm_rpmLimit').value : '0', 10);
      const ipLimit = parseInt($('apiKeyForm_ipLimit') ? $('apiKeyForm_ipLimit').value : '0', 10);
      const tpmLimit = parseInt($('apiKeyForm_tpmLimit') ? $('apiKeyForm_tpmLimit').value : '0', 10);
      const ipAllowlist = $('apiKeyForm_ipAllowlist')
        ? $('apiKeyForm_ipAllowlist').value.split(/[\r\n,]+/).map(s => s.trim()).filter(Boolean)
        : [];
      const payload = {
        name: name,
        enabled: enabled,
        tokenLimit: isNaN(tokenLimit) || tokenLimit < 0 ? 0 : tokenLimit,
        creditLimit: isNaN(creditLimit) || creditLimit < 0 ? 0 : creditLimit,
        expiresAt: expiresAt,
        rpmLimit: isNaN(rpmLimit) || rpmLimit < 0 ? 0 : rpmLimit,
        ipLimit: isNaN(ipLimit) || ipLimit < 0 ? 0 : ipLimit,
        ipAllowlist: ipAllowlist,
        tpmLimit: isNaN(tpmLimit) || tpmLimit < 0 ? 0 : tpmLimit,
        ownerRef: $('apiKeyForm_ownerRef') ? $('apiKeyForm_ownerRef').value.trim() : '',
        orderRef: $('apiKeyForm_orderRef') ? $('apiKeyForm_orderRef').value.trim() : '',
        note: $('apiKeyForm_note') ? $('apiKeyForm_note').value.trim() : '',
        boundAccountIds: selectedBoundAccountIds(),
        models: Array.from(apiKeyModelsSelected)
      };
      const keyVal = $('apiKeyForm_key').value.trim();
      let res, d;
      if (apiKeyEditingId) {
        // Only send the key when the operator typed a new value; blank keeps the current one.
        if (keyVal) payload.key = keyVal;
        res = await api('/api-keys/' + encodeURIComponent(apiKeyEditingId), { method: 'PUT', body: JSON.stringify(payload) });
        d = await res.json().catch(() => ({}));
        if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
        toast(t('apiKeys.updated'), 'success');
        closeApiKeyModal();
        await loadApiKeys();
      } else {
        if (keyVal) payload.key = keyVal;
        res = await api('/api-keys', { method: 'POST', body: JSON.stringify(payload) });
        d = await res.json().catch(() => ({}));
        if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
        toast(t('apiKeys.created'), 'success');
        closeApiKeyModal();
        await loadApiKeys();
        if (d.key) showNewApiKey(d.key);
      }
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
      apiKeyModalSubmitting = false;
      saveBtn.disabled = false;
    }
  }

  async function toggleApiKeyEntry(id, enabled) {
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id), { method: 'PUT', body: JSON.stringify({ enabled }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      const item = apiKeysCache.find(x => x.id === id);
      if (item) item.enabled = enabled;
      renderApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
      await loadApiKeys();
    }
  }

  async function deleteApiKeyEntry(id, name) {
    const ok = await confirmAction(t('apiKeys.confirmDelete', name || t('apiKeys.unnamed')), {
      title: t('apiKeys.actionDelete'),
      confirmText: t('apiKeys.actionDelete'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id), { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('apiKeys.deleteSuccess'), 'success');
      await loadApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  async function resetApiKeyUsageEntry(id, name) {
    const ok = await confirmAction(t('apiKeys.confirmReset', name || t('apiKeys.unnamed')), {
      title: t('apiKeys.actionReset'),
      confirmText: t('apiKeys.actionReset')
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id) + '/reset-usage', { method: 'POST' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('apiKeys.usageReset'), 'success');
      await loadApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  // resetAllApiKeyUsageEntry wipes BOTH current-period and lifetime counters (destructive).
  async function resetAllApiKeyUsageEntry(id, name) {
    const ok = await confirmAction(t('apiKeys.confirmResetAll', name || t('apiKeys.unnamed')), {
      title: t('apiKeys.actionResetAll'),
      confirmText: t('apiKeys.actionResetAll'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id) + '/reset-all', { method: 'POST' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('apiKeys.usageReset'), 'success');
      await loadApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  // Copies the self-service portal URL so the seller can hand it to a customer
  // alongside their key. Prefers the configured public base URL, else this origin.
  async function copyPortalLink() {
    const base = (($('publicBaseURL') && $('publicBaseURL').value.trim()) || location.origin).replace(/\/+$/, '');
    try {
      await copyText(base + '/check');
      toast(t('apiKeys.portalLinkCopied'), 'success');
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  function showNewApiKey(plaintext) {
    $('apiKeyShowValue').value = plaintext || '';
    openDialog('apiKeyShowModal');
    setTimeout(() => {
      const el = $('apiKeyShowValue');
      if (el) { try { el.select(); } catch (_) { } }
    }, 0);
  }

  function closeShowApiKeyModal() {
    closeDialog('apiKeyShowModal');
    $('apiKeyShowValue').value = '';
  }

  async function copyNewApiKey() {
    const val = $('apiKeyShowValue').value;
    if (!val) return;
    try {
      await copyText(val);
      toast(t('apiKeys.copySuccess'), 'success');
    } catch (e) {
      toast(t('common.failed'), 'error');
    }
  }

  function bindApiKeyEvents() {
    const list = $('apiKeysList');
    if (list) {
      list.addEventListener('click', e => {
        const btn = e.target.closest('[data-apikey-action]');
        if (!btn) return;
        const action = btn.dataset.apikeyAction;
        const id = btn.dataset.id;
        if (!id) return;
        const entry = apiKeysCache.find(x => x.id === id);
        const name = entry ? entry.name : '';
        if (action === 'edit') openApiKeyModal(entry);
        else if (action === 'delete') deleteApiKeyEntry(id, name);
        else if (action === 'reset') resetApiKeyUsageEntry(id, name);
        else if (action === 'reset-all') resetAllApiKeyUsageEntry(id, name);
        else if (action === 'portal') copyPortalLink();
      });
      list.addEventListener('change', e => {
        const select = e.target.closest('input[data-apikey-action="select"]');
        if (select) {
          const id = select.dataset.id;
          if (id) {
            if (select.checked) selectedApiKeyIds.add(id);
            else selectedApiKeyIds.delete(id);
            renderApiKeyBulkBar();
          }
          return;
        }
        const cb = e.target.closest('input[data-apikey-action="toggle"]');
        if (!cb) return;
        const id = cb.dataset.id;
        if (!id) return;
        toggleApiKeyEntry(id, cb.checked);
      });
    }
    const exportBtn = $('exportApiKeysBtn');
    if (exportBtn) exportBtn.addEventListener('click', showApiKeyExportModal);
    const addBtn = $('addApiKeyBtn');
    if (addBtn) addBtn.addEventListener('click', () => openApiKeyModal(null));
    const bulkAddBtn = $('bulkAddApiKeyBtn');
    if (bulkAddBtn) bulkAddBtn.addEventListener('click', openApiKeyBulkModal);
    const bulkDeleteBtn = $('bulkDeleteApiKeyBtn');
    if (bulkDeleteBtn) bulkDeleteBtn.addEventListener('click', bulkDeleteApiKeys);
    const selectAllApiKeys = $('apiKeySelectAll');
    if (selectAllApiKeys) selectAllApiKeys.addEventListener('change', e => toggleApiKeySelectAll(e.target.checked));
    const saveBtn = $('apiKeyModalSaveBtn');
    if (saveBtn) saveBtn.addEventListener('click', submitApiKeyModal);
    const cancelBtn = $('apiKeyModalCancelBtn');
    if (cancelBtn) cancelBtn.addEventListener('click', closeApiKeyModal);
    // Bound-account picker: live search, select-all/clear, and keep the selection set in sync.
    const boundSearch = $('apiKeyForm_boundSearch');
    if (boundSearch) boundSearch.addEventListener('input', () => renderApiKeyBoundAccounts(boundSearch.value));
    const boundBox = $('apiKeyForm_boundAccounts');
    if (boundBox) boundBox.addEventListener('change', e => {
      const cb = e.target.closest('input.apikey-bound-account');
      if (cb) onApiKeyBoundToggle(cb.value, cb.checked);
    });
    const modelBox = $('apiKeyForm_models');
    if (modelBox) modelBox.addEventListener('change', e => {
      const cb = e.target.closest('input.apikey-model-option');
      if (cb) {
        if (cb.checked) apiKeyModelsSelected.add(cb.value);
        else apiKeyModelsSelected.delete(cb.value);
      }
    });
    const boundSelectAll = $('apiKeyForm_boundSelectAll');
    if (boundSelectAll) boundSelectAll.addEventListener('click', () => {
      (Array.isArray(accountsData) ? accountsData : []).forEach(a => apiKeyBoundSelected.add(a.id));
      renderApiKeyBoundAccounts(boundSearch ? boundSearch.value : '');
    });
    const boundClear = $('apiKeyForm_boundClear');
    if (boundClear) boundClear.addEventListener('click', () => {
      apiKeyBoundSelected.clear();
      renderApiKeyBoundAccounts(boundSearch ? boundSearch.value : '');
    });
    const closeBtn = $('apiKeyModalClose');
    if (closeBtn) closeBtn.addEventListener('click', closeApiKeyModal);
    const showCloseBtn = $('apiKeyShowCloseBtn');
    if (showCloseBtn) showCloseBtn.addEventListener('click', closeShowApiKeyModal);
    const showCloseX = $('apiKeyShowClose');
    if (showCloseX) showCloseX.addEventListener('click', closeShowApiKeyModal);
    const copyBtn = $('apiKeyShowCopyBtn');
    if (copyBtn) copyBtn.addEventListener('click', copyNewApiKey);
    const bulkSaveBtn = $('apiKeyBulkSaveBtn');
    if (bulkSaveBtn) bulkSaveBtn.addEventListener('click', createBulkApiKeys);
    const bulkCancelBtn = $('apiKeyBulkCancelBtn');
    if (bulkCancelBtn) bulkCancelBtn.addEventListener('click', closeApiKeyBulkModal);
    const bulkCloseBtn = $('apiKeyBulkModalClose');
    if (bulkCloseBtn) bulkCloseBtn.addEventListener('click', closeApiKeyBulkModal);
    // Bulk bound-account picker: mirrors the single-key picker, backed by its own selection set.
    const bulkBoundSearch = $('apiKeyBulk_boundSearch');
    if (bulkBoundSearch) bulkBoundSearch.addEventListener('input', () => renderApiKeyBulkBoundAccounts(bulkBoundSearch.value));
    const bulkBoundBox = $('apiKeyBulk_boundAccounts');
    if (bulkBoundBox) bulkBoundBox.addEventListener('change', e => {
      const cb = e.target.closest('input.apikey-bound-account');
      if (cb) {
        if (cb.checked) apiKeyBulkBoundSelected.add(cb.value);
        else apiKeyBulkBoundSelected.delete(cb.value);
      }
    });
    const bulkModelBox = $('apiKeyBulk_models');
    if (bulkModelBox) bulkModelBox.addEventListener('change', e => {
      const cb = e.target.closest('input.apikey-model-option');
      if (cb) {
        if (cb.checked) apiKeyBulkModelsSelected.add(cb.value);
        else apiKeyBulkModelsSelected.delete(cb.value);
      }
    });
    const bulkBoundSelectAll = $('apiKeyBulk_boundSelectAll');
    if (bulkBoundSelectAll) bulkBoundSelectAll.addEventListener('click', () => {
      (Array.isArray(accountsData) ? accountsData : []).forEach(a => apiKeyBulkBoundSelected.add(a.id));
      renderApiKeyBulkBoundAccounts(bulkBoundSearch ? bulkBoundSearch.value : '');
    });
    const bulkBoundClear = $('apiKeyBulk_boundClear');
    if (bulkBoundClear) bulkBoundClear.addEventListener('click', () => {
      apiKeyBulkBoundSelected.clear();
      renderApiKeyBulkBoundAccounts(bulkBoundSearch ? bulkBoundSearch.value : '');
    });
    bindDialogBackdropClose('apiKeyModal', closeApiKeyModal);
    bindDialogBackdropClose('apiKeyBulkModal', closeApiKeyBulkModal);
    bindDialogBackdropClose('apiKeyShowModal', closeShowApiKeyModal);
  }

  // Prompt filter rules
  async function loadPromptFilter() {
    const res = await api('/prompt-filter');
    const d = await res.json();
    $('filterClaudeCode').checked = !!d.filterClaudeCode;
    $('filterEnvNoise').checked = !!d.filterEnvNoise;
    $('filterStripBoundaries').checked = !!d.filterStripBoundaries;
    promptRules = d.rules || [];
    renderPromptRules();
  }
  async function savePromptFilter() {
    const res = await api('/prompt-filter', {
      method: 'POST', body: JSON.stringify({
        filterClaudeCode: $('filterClaudeCode').checked,
        filterEnvNoise: $('filterEnvNoise').checked,
        filterStripBoundaries: $('filterStripBoundaries').checked,
        rules: promptRules
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.promptFilterSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  function renderPromptRules() {
    const c = $('promptFilterRules');
    if (!c) return;
    if (!promptRules.length) {
      c.innerHTML = '<small class="text-xs muted-text">' + escapeHtml(t('promptFilter.noRules')) + '</small>';
      return;
    }
    c.innerHTML = promptRules.map((r, i) => {
      const isContains = r.type === 'lines-containing';
      const typeLabel = isContains ? t('promptFilter.typeContains') : t('promptFilter.typeRegex');
      const matchPh = isContains ? t('promptFilter.matchPlaceholderContains') : t('promptFilter.matchPlaceholderRegex');
      const replaceRow = !isContains
        ? '<div class="rule-field"><label>' + escapeHtml(t('promptFilter.replace')) + '</label>' +
        '<input value="' + escapeAttr(r.replace || '') + '" data-rule-idx="' + i + '" data-rule-field="replace" placeholder="' + escapeAttr(t('promptFilter.emptyRemove')) + '" />' +
        '</div>'
        : '';
      return '<div class="rule-card' + (r.enabled ? '' : ' disabled') + '">' +
        '<div class="rule-header">' +
        '<label class="switch"><input type="checkbox" ' + (r.enabled ? 'checked' : '') + ' data-rule-toggle="' + i + '" /><span class="slider"></span></label>' +
        '<div class="rule-meta">' +
        '<input class="rule-name-input" value="' + escapeAttr(r.name || '') + '" data-rule-idx="' + i + '" data-rule-field="name" placeholder="' + escapeAttr(t('promptFilter.unnamed')) + '" />' +
        '<span class="rule-type">' + escapeHtml(typeLabel) + '</span>' +
        '</div>' +
        '<button class="rule-remove" data-rule-remove="' + i + '" type="button" aria-label="' + escapeAttr(t('common.remove')) + '">&times;</button>' +
        '</div>' +
        '<div class="rule-body">' +
        '<div class="rule-field"><label>' + escapeHtml(t('promptFilter.match')) + '</label>' +
        '<input value="' + escapeAttr(r.match || '') + '" data-rule-idx="' + i + '" data-rule-field="match" placeholder="' + escapeAttr(matchPh) + '" />' +
        '</div>' +
        replaceRow +
        '</div>' +
        '</div>';
    }).join('');
  }
  function addPromptRule(type) {
    promptRules.push({ id: 'rule-' + Date.now(), name: '', type, match: '', replace: '', enabled: true });
    renderPromptRules();
  }

  // Add-account modal templates
  var METHOD_ICONS = {
    builderid: 'fa-solid fa-id-card',
    iam: 'fa-solid fa-key',
    sso: 'fa-solid fa-shield-halved',
    local: 'fa-solid fa-folder-open',
    credentials: 'fa-solid fa-code',
    cookie: 'fa-solid fa-cookie-bite',
    kiro: 'fa-solid fa-building'
  };
  function methodCard(type, title, desc) {
    var icon = METHOD_ICONS[type] || 'fa-solid fa-circle-plus';
    return '<button type="button" class="method-card" data-method="' + escapeAttr(type) + '">' +
      '<span class="method-icon"><i class="' + icon + '" aria-hidden="true"></i></span>' +
      '<span class="method-body">' +
      '<span class="method-title">' + escapeHtml(title) + '</span>' +
      '<span class="method-desc">' + escapeHtml(desc) + '</span>' +
      '</span>' +
      '<span class="method-arrow" aria-hidden="true"><i class="fa-solid fa-chevron-right"></i></span>' +
      '</button>';
  }
  function showModal(type) {
    const modal = $('addModal');
    const title = $('modalTitle');
    const body = $('modalBody');
    if (type === 'add') modalAdd(title, body);
    else if (type === 'builderid') modalBuilderId(title, body);
    else if (type === 'iam') modalIam(title, body);
    else if (type === 'sso') modalSso(title, body);
    else if (type === 'local') modalLocal(title, body);
    else if (type === 'localdetect') modalLocalDetect(title, body);
    else if (type === 'credentials') modalCredentials(title, body);
    else if (type === 'cookie') modalCookie(title, body);
    else if (type === 'apikey') modalApiKey(title, body);
    else if (type === 'apikeybatch') modalApiKeyBatch(title, body);
    else if (type === 'kiro') modalKiro(title, body);
    if (!modal.classList.contains('active')) openDialog('addModal');
    enhanceCustomSelects(body);
  }
  function closeModal() {
    closeDialog('addModal');
    iamSession = '';
    kiroSsoSession = '';
    if (builderIdPollTimer) { clearTimeout(builderIdPollTimer); builderIdPollTimer = null; }
    if (kiroSsoPollTimer) { clearTimeout(kiroSsoPollTimer); kiroSsoPollTimer = null; }
    builderIdSession = '';
  }
  function modalAdd(title, body) {
    title.textContent = t('modal.addAccount');
    body.innerHTML =
      '<div class="method-list">' +
      methodCard('builderid', t('modal.builderIdTitle'), t('modal.builderIdDesc')) +
      methodCard('iam', t('modal.iamTitle'), t('modal.iamDesc')) +
      methodCard('kiro', t('modal.kiroTitle'), t('modal.kiroDesc')) +
      methodCard('sso', t('modal.ssoTitle'), t('modal.ssoDesc')) +
      methodCard('localdetect', t('modal.localDetectTitle'), t('modal.localDetectDesc')) +
      methodCard('local', t('modal.localTitle'), t('modal.localDesc')) +
      methodCard('credentials', t('modal.credentialsTitle'), t('modal.credentialsDesc')) +
      methodCard('cookie', t('modal.cookieTitle'), t('modal.cookieDesc')) +
      methodCard('apikey', t('modal.apikeyTitle'), t('modal.apikeyDesc')) +
      methodCard('apikeybatch', t('modal.apikeyBatchTitle'), t('modal.apikeyBatchDesc')) +
      '</div>' +
      '<div class="modal-footer"><button class="btn btn-secondary" data-close-add="1" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>';
  }
  function modalBuilderId(title, body) {
    title.textContent = t('modal.builderIdTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.builderIdDesc')) + '</p>' +
      '<div id="builderIdStep1">' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="builderIdRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="startBuilderIdBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '</div>' +
      '</div>' +
      '<div id="builderIdStep2" class="hidden">' +
      '<div class="message message-info message-center"><p class="builder-code" id="builderIdUserCode"></p><p class="text-xs mt-2">' + escapeHtml(t('builderid.verifyCode')) + '</p></div>' +
      '<div class="form-group mt-4"><label>' + escapeHtml(t('builderid.verifyUrl')) + '</label>' +
      '<div class="endpoint"><span id="builderIdVerifyUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="builderIdOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="builderIdCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p id="builderIdStatus" class="text-center text-sm mt-4 muted-text">' + escapeHtml(t('builderid.waiting')) + '</p>' +
      '<div class="modal-footer"><button class="btn btn-secondary" id="builderIdCancelBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>' +
      '</div>';
    $('startBuilderIdBtn').addEventListener('click', startBuilderIdLogin);
  }
  function modalIam(title, body) {
    title.textContent = t('modal.iamTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.iamDesc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.startUrl')) + '</label><input type="text" id="iamStartUrl" placeholder="https://xxx.awsapps.com/start" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="iamRegion" value="us-east-1" /></div>' +
      '<div id="iamStep2" class="hidden">' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.loginUrl')) + '</label>' +
      '<div class="endpoint"><span id="iamAuthUrl" class="font-mono text-xs"></span></div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-sm btn-outline flex-1" id="iamOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-sm btn-outline flex-1" id="iamCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '</div>' +
      '<p class="text-sm mt-3 success-text">' + escapeHtml(t('iam.completeLogin')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.callbackUrl')) + '</label><input type="text" id="iamCallback" placeholder="http://127.0.0.1:xxx/?code=..." /></div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="iamBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '</div>';
    $('iamBtn').addEventListener('click', startIamSso);
  }
  function modalSso(title, body) {
    title.textContent = t('modal.ssoTitle');
    body.innerHTML =
      '<div class="help-block">' +
      '<b>' + escapeHtml(t('sso.howToGet')) + '</b>' +
      '<ol class="steps-list">' +
      '<li>' + escapeHtml(t('sso.step1')) + ' <code class="code-inline">view.awsapps.com/start</code></li>' +
      '<li>' + escapeHtml(t('sso.step2')) + '</li>' +
      '<li>' + escapeHtml(t('sso.step3')) + ' <code class="code-inline">x-amz-sso_authn</code></li>' +
      '</ol>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('sso.tokenLabel')) + ' <small>' + escapeHtml(t('sso.tokenHint')) + '</small></label>' +
      '<textarea id="ssoToken" placeholder="' + escapeAttr(t('sso.tokenPlaceholder')) + '"></textarea></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('detail.region')) + '</label><input type="text" id="ssoRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importSsoBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importSsoBtn').addEventListener('click', importSsoToken);
  }

  function modalLocal(title, body) {
    title.textContent = t('modal.localTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.localDesc')) + '</p>' +
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('local.fileLocation')) + '</b></p>' +
      '<p>' + escapeHtml(t('local.windows')) + ': <code class="code-inline">%USERPROFILE%\\.aws\\sso\\cache\\</code></p>' +
      '<p>' + escapeHtml(t('local.macosLinux')) + ': <code class="code-inline">~/.aws/sso/cache/</code></p>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('local.loginChannel')) + '</label>' +
      '<select id="localProvider">' +
      '<option value="BuilderId">' + escapeHtml(t('local.providerBuilderId')) + '</option>' +
      '<option value="Enterprise">' + escapeHtml(t('local.providerEnterprise')) + '</option>' +
      '<option value="Google">' + escapeHtml(t('local.providerGoogle')) + '</option>' +
      '<option value="Github">' + escapeHtml(t('local.providerGithub')) + '</option>' +
      '</select>' +
      '</div>' +
      '<div class="form-group">' +
      '<label>' + escapeHtml(t('local.tokenFile')) + ' <small>' + escapeHtml(t('local.tokenRequired')) + '</small></label>' +
      '<div class="input-row">' +
      '<textarea id="localTokenJson" placeholder="' + escapeAttr(t('local.pasteOrUpload')) + '" class="font-mono"></textarea>' +
      '<label class="btn btn-outline btn-sm">' + escapeHtml(t('local.upload')) +
      '<input type="file" accept=".json" id="localTokenFile" class="file-input-hidden" />' +
      '</label>' +
      '</div>' +
      '</div>' +
      '<div id="localClientGroup" class="form-group">' +
      '<label>' + escapeHtml(t('local.clientFile')) + ' <small>' + escapeHtml(t('local.clientRequired')) + '</small></label>' +
      '<div class="input-row">' +
      '<textarea id="localClientJson" placeholder="' + escapeAttr(t('local.pasteOrUpload')) + '" class="font-mono"></textarea>' +
      '<label class="btn btn-outline btn-sm">' + escapeHtml(t('local.upload')) +
      '<input type="file" accept=".json" id="localClientFile" class="file-input-hidden" />' +
      '</label>' +
      '</div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importLocalBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('localProvider').addEventListener('change', updateLocalFields);
    $('localTokenFile').addEventListener('change', e => loadLocalFile(e.target, 'localTokenJson'));
    $('localClientFile').addEventListener('change', e => loadLocalFile(e.target, 'localClientJson'));
    $('importLocalBtn').addEventListener('click', importLocalKiro);
  }
  // Auto-detect Kiro credentials from the local SSO cache (same machine only).
  function modalLocalDetect(title, body) {
    title.textContent = t('modal.localDetectTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.localDetectDesc')) + '</p>' +
      '<div id="localDetectStatus" class="help-block">' + escapeHtml(t('localDetect.scanning')) + '</div>' +
      '<div id="localDetectList"></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="localDetectImportBtn" type="button" disabled>' + escapeHtml(t('localDetect.importSelected')) + '</button>' +
      '</div>';
    $('localDetectImportBtn').addEventListener('click', importLocalDetected);
    scanLocalCache();
  }
  async function scanLocalCache() {
    const statusEl = $('localDetectStatus');
    const listEl = $('localDetectList');
    const importBtn = $('localDetectImportBtn');
    try {
      const res = await api('/auth/local-cache/scan', { method: 'GET' });
      const d = await res.json();
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      if (!d.available || !d.accounts || d.accounts.length === 0) {
        statusEl.textContent = t('localDetect.notFound');
        listEl.innerHTML = '';
        importBtn.disabled = true;
        return;
      }
      statusEl.innerHTML = escapeHtml(t('localDetect.found', d.count)) +
        ' <code class="code-inline">' + escapeHtml(d.cacheDir || '') + '</code>';
      listEl.innerHTML = d.accounts.map(a => {
        const label = (a.loginHint || a.provider || a.authMethod || a.fingerprint);
        const meta = formatAuthMethod(a.provider || a.authMethod) + ' · ' + a.region +
          (a.hasClient ? '' : ' · ' + t('localDetect.noClient'));
        const disabled = a.importable ? '' : 'disabled';
        const reason = a.importable ? '' : ' <small class="muted-text">(' + escapeHtml(a.reason || '') + ')</small>';
        return '<label class="export-row' + (a.importable ? '' : ' opacity-50') + '">' +
          '<input type="checkbox" ' + (a.importable ? 'checked' : '') + ' ' + disabled +
          ' data-detect-fp="' + escapeAttr(a.fingerprint) + '" />' +
          '<div class="export-row-text">' +
          '<div class="export-row-email">' + escapeHtml(label) + reason + '</div>' +
          '<div class="export-row-meta">' + escapeHtml(meta) + '</div>' +
          '</div></label>';
      }).join('');
      importBtn.disabled = false;
    } catch (e) {
      statusEl.textContent = (e && e.message) || t('common.failed');
      importBtn.disabled = true;
    }
  }
  async function importLocalDetected() {
    const fps = qsa('[data-detect-fp]:checked', $('localDetectList')).map(cb => cb.dataset.detectFp);
    if (fps.length === 0) return toastWarning(t('localDetect.selectAtLeastOne'));
    const importBtn = $('localDetectImportBtn');
    importBtn.disabled = true;
    try {
      const res = await api('/auth/local-cache/import', {
        method: 'POST', body: JSON.stringify({ fingerprints: fps })
      });
      const d = await res.json();
      if (d.success) {
        closeModal(); loadAccounts(); loadStats();
        toastPrimary(t('localDetect.importSuccess', d.imported || 0));
        (d.results || []).forEach(r => { if (r.success && r.accountId) autoRefreshNewAccount(r.accountId); });
      } else {
        const firstErr = (d.results || []).find(r => r.error);
        toastError(t('common.failed') + (firstErr ? ': ' + firstErr.error : ''));
        importBtn.disabled = false;
      }
    } catch (e) {
      toastError((e && e.message) || t('common.failed'));
      importBtn.disabled = false;
    }
  }
  function modalCredentials(title, body) {
    title.textContent = t('modal.credentialsTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.credentialsDesc')) + '</p>' +
      '<p class="help-block">' + escapeHtml(t('credentials.batchHint')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('credentials.label')) + '</label>' +
      '<textarea id="credJson" class="font-mono" placeholder=\'[{"refreshToken":"xxx","provider":"BuilderID"}]&#10;or&#10;email----password----refreshToken----clientId----clientSecret\'></textarea>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importCredBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importCredBtn').addEventListener('click', importCredentials);
  }
  function modalCookie(title, body) {
    title.textContent = t('modal.cookieTitle');
    body.innerHTML =
      '<div class="help-block">' +
      '<p><b>' + escapeHtml(t('cookie.howToGet')) + '</b></p>' +
      '<ol class="steps-list">' +
      '<li>' + escapeHtml(t('cookie.step1')) + ' <a href="' + escapeAttr(t('cookie.link')) + '" target="_blank">' + escapeHtml(t('cookie.link')) + '</a></li>' +
      '<li>' + escapeHtml(t('cookie.step2')) + '</li>' +
      '<li>' + escapeHtml(t('cookie.step3')) + '</li>' +
      '</ol>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('cookie.provider')) + '</label>' +
      '<select id="cookieProvider">' +
      '<option value="Google">' + escapeHtml(t('cookie.google')) + '</option>' +
      '<option value="Github">' + escapeHtml(t('cookie.github')) + '</option>' +
      '</select>' +
      '</div>' +
      '<div class="form-group"><label>' + escapeHtml(t('cookie.refreshToken')) + '</label>' +
      '<textarea id="cookieRefreshToken" class="font-mono" placeholder="' + escapeAttr(t('cookie.refreshTokenPlaceholder')) + '"></textarea>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importCookieBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importCookieBtn').addEventListener('click', importFromCookie);
  }
  function modalApiKey(title, body) {
    title.textContent = t('apikey.title');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('apikey.desc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikey.keyLabel')) + '</label>' +
      '<input type="password" id="apikeyInput" placeholder="' + escapeAttr(t('apikey.keyPlaceholder')) + '" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikey.nickname')) + '</label>' +
      '<input type="text" id="apikeyNickname" placeholder="' + escapeAttr(t('apikey.nicknamePlaceholder')) + '" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikey.authRegion')) + '</label>' +
      '<input type="text" id="apikeyAuthRegion" value="us-east-1" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikey.apiRegion')) + '</label>' +
      '<input type="text" id="apikeyApiRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importApikeyBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importApikeyBtn').addEventListener('click', importApiKey);
  }
  function modalApiKeyBatch(title, body) {
    title.textContent = t('apikeyBatch.title');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('apikeyBatch.desc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikeyBatch.listLabel')) + '</label>' +
      '<textarea id="apikeyBatchList" class="font-mono" rows="8" placeholder="' + escapeAttr(t('apikeyBatch.listPlaceholder')) + '"></textarea>' +
      '<p class="help-block">' + escapeHtml(t('apikeyBatch.listHint')) + '</p></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('apikey.apiRegion')) + '</label>' +
      '<input type="text" id="apikeyBatchRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="importApikeyBatchBtn" type="button">' + escapeHtml(t('apikeyBatch.import')) + '</button>' +
      '</div>' +
      '<div id="apikeyBatchResults" class="mt-3"></div>';
    $('importApikeyBatchBtn').addEventListener('click', importApiKeysBatch);
  }
  function modalKiro(title, body) {
    title.textContent = t('modal.kiroTitle');
    body.innerHTML =
      '<p class="help-block">' + escapeHtml(t('modal.kiroDesc')) + '</p>' +
      '<div id="kiroStep1">' +
      '<div class="form-group"><label>' + escapeHtml(t('kiro.emailLabel')) + '</label>' +
      '<input type="email" id="kiroEmail" placeholder="' + escapeAttr(t('kiro.emailPlaceholder')) + '" /></div>' +
      '<p class="text-xs muted-text">' + escapeHtml(t('kiro.emailHint')) + '</p>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-modal-goto="add" type="button">' + escapeHtml(t('common.back')) + '</button>' +
      '<button class="btn btn-primary" id="startKiroBtn" type="button">' + escapeHtml(t('kiro.startLogin')) + '</button>' +
      '</div>' +
      '</div>' +
      '<div id="kiroStep2" class="hidden">' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.loginUrl')) + '</label>' +
      '<textarea id="kiroAuthUrl" readonly rows="3" class="w-full font-mono text-xs p-2"' +
      ' style="word-break:break-all; resize:none; border:1px solid var(--border); border-radius:var(--radius); background:var(--surface); color:var(--text);"></textarea>' +
      '</div>' +
      '<div class="flex gap-2 mt-2">' +
      '<button class="btn btn-primary btn-sm" id="kiroOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button>' +
      '<button class="btn btn-outline btn-sm" id="kiroCopyBtn" type="button">' + escapeHtml(t('common.copy')) + '</button>' +
      '</div>' +
      '<p id="kiroStatus" class="text-center text-sm mt-4 muted-text">' + escapeHtml(t('builderid.waiting')) + '</p>' +
      '<div class="modal-footer"><button class="btn btn-secondary" id="kiroCancelBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button></div>' +
      '</div>';
    $('startKiroBtn').addEventListener('click', startKiroSso);
  }

  // Kiro SSO state
  var kiroSsoSession = '';
  var kiroSsoPollTimer = null;

  async function startKiroSso() {
    try {
      var email = $('kiroEmail').value.trim();
      var res = await api('/auth/kiro-sso/start', {
        method: 'POST',
        body: JSON.stringify({ loginHint: email || undefined })
      });
      var d = await res.json();
      if (d.sessionId) {
        kiroSsoSession = d.sessionId;
        $('kiroStep1').classList.add('hidden');
        $('kiroStep2').classList.remove('hidden');
        $('kiroAuthUrl').textContent = d.authorizeUrl;
        $('kiroOpenBtn').addEventListener('click', function() {
          window.open($('kiroAuthUrl').textContent, '_blank');
        });
        $('kiroCopyBtn').addEventListener('click', async function() {
          await copyText($('kiroAuthUrl').textContent);
          toast(t('common.copied'), 'primary');
        });
        $('kiroCancelBtn').addEventListener('click', cancelKiroSso);
        pollKiroSso(2);
      } else {
        toastError(t('common.failed') + ': ' + (d.error || ''));
      }
    } catch (e) {
      toastError(t('login.connectError'));
    }
  }

  function pollKiroSso(interval) {
    kiroSsoPollTimer = setTimeout(async function() {
      try {
        var res = await api('/auth/kiro-sso/poll', {
          method: 'POST',
          body: JSON.stringify({ sessionId: kiroSsoSession })
        });
        var d = await res.json();
        if (d.status === 'completed') {
          closeModal(); loadAccounts(); loadStats();
          toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
          autoRefreshNewAccount(d.account?.id);
        } else if (d.status === 'pending') {
          $('kiroStatus').textContent = t('builderid.waiting');
          pollKiroSso(d.interval || interval);
        } else {
          cancelKiroSso();
          toastError(t('common.failed') + ': ' + (d.error || t('kiro.authFailed')));
        }
      } catch (e) {
        cancelKiroSso();
        toastError(t('login.connectError'));
      }
    }, interval * 1000);
  }

  function cancelKiroSso() {
    if (kiroSsoPollTimer) { clearTimeout(kiroSsoPollTimer); kiroSsoPollTimer = null; }
    if (kiroSsoSession) {
      api('/auth/kiro-sso/cancel', {
        method: 'POST',
        body: JSON.stringify({ sessionId: kiroSsoSession })
      }).catch(function() {});
    }
    kiroSsoSession = '';
    showModal('add');
  }

  function updateLocalFields() {
    const p = $('localProvider').value;
    $('localClientGroup').classList.toggle('hidden', p === 'Google' || p === 'Github');
  }
  function loadLocalFile(input, targetId) {
    const file = input.files[0];
    if (!file) return;
    const r = new FileReader();
    r.onload = e => { $(targetId).value = e.target.result; };
    r.readAsText(file);
  }

  // Import handlers
  async function importLocalKiro() {
    const provider = $('localProvider').value;
    const tokenJson = $('localTokenJson').value.trim();
    const clientJson = $('localClientJson').value.trim();
    const isSocial = provider === 'Google' || provider === 'Github';
    if (!tokenJson) return toastWarning(t('local.tokenMissing'));
    let tokenData, clientData;
    try { tokenData = JSON.parse(tokenJson); } catch { return toastWarning(t('local.tokenInvalid')); }
    if (!tokenData.refreshToken) return toastWarning(t('local.refreshTokenMissing'));
    if (!isSocial) {
      if (!clientJson) return toastWarning(t('local.clientMissing'));
      try { clientData = JSON.parse(clientJson); } catch { return toastWarning(t('local.clientInvalid')); }
      if (!clientData.clientId || !clientData.clientSecret) return toastWarning(t('local.clientSecretMissing'));
    }
    const authMethod = clientData ? 'idc' : 'social';
    const payload = {
      refreshToken: tokenData.refreshToken,
      accessToken: tokenData.accessToken || '',
      clientId: clientData?.clientId || '',
      clientSecret: clientData?.clientSecret || '',
      region: tokenData.region || '',
      authMethod, provider
    };
    const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('local.importSuccess') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function importCredentials() {
    const raw = $('credJson').value.trim();
    if (!raw) { toastWarning(t('credentials.jsonError')); return; }
    let items;
    let skipped = 0;
    try {
      const json = JSON.parse(raw);
      const rawItems = (json.accounts && Array.isArray(json.accounts))
        ? json.accounts
        : (Array.isArray(json) ? json : [json]);
      items = rawItems.map(normalizeCredItem);
    } catch {
      const parsed = parseLineCredentials(raw);
      items = parsed.items;
      skipped = parsed.skipped;
      if (items.length === 0 && skipped === 0) {
        toastWarning(t('credentials.jsonError'));
        return;
      }
      if (items.length === 0) {
        toastWarning(t('credentials.lineParseAllSkipped', skipped));
        return;
      }
    }
    let ok = 0, fail = 0, newIds = [];
    for (const item of items) {
      if (!item.refreshToken) { fail++; continue; }
      let authMethod = item.authMethod || '';
      if (item.clientId && item.clientSecret) authMethod = 'idc';
      else if (authMethod === "external_idp" || authMethod === "externalidp") authMethod = "external_idp"; else if (!authMethod || authMethod === "social") authMethod = "social";
      else if (authMethod.toLowerCase() === "idc") authMethod = "idc"; else authMethod = "social";
      let provider = item.provider || '';
      if (!provider && authMethod === 'social') provider = 'Google';
      if (!provider && authMethod === 'idc') provider = 'BuilderId';
      const payload = {
        refreshToken: item.refreshToken,
        accessToken: item.accessToken || '',
        clientId: item.clientId || '',
        clientSecret: item.clientSecret || '',
        authMethod, provider,
        issuerUrl: item.issuerUrl || "",
        idpClientId: item.idpClientId || "",
        scopes: item.scopes || "",
        loginHint: item.loginHint || "",
        idpTokenEndpoint: item.idpTokenEndpoint || "",
        region: item.region || 'us-east-1'
      };
      try {
        const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
        const d = await res.json();
        if (d.success) { ok++; if (d.account?.id) newIds.push(d.account.id); }
        else fail++;
      } catch { fail++; }
    }
    closeModal(); loadAccounts(); loadStats();
    let msg = t('sso.importSuccess', ok);
    if (fail > 0) msg += t('sso.importPartial', fail);
    if (skipped > 0) msg += t('credentials.lineParseSkipped', skipped);
    toastPrimary(msg, { duration: 5200 });
    newIds.forEach(autoRefreshNewAccount);
  }
  // Normalize one credential object into the /auth/credentials payload shape.
  // Accepts camelCase (existing exports), the {credentials:{...}} wrapper, and
  // flat snake_case (cliproxyapi kiro export: access_token, auth_method,
  // issuer_url, token_endpoint, ...).
  function normalizeCredItem(a) {
    const c = a.credentials || {};
    const pick = (...keys) => {
      for (const src of [c, a]) {
        for (const k of keys) {
          if (src[k] !== undefined && src[k] !== null && src[k] !== '') return src[k];
        }
      }
      return undefined;
    };
    const authMethod = pick('authMethod', 'auth_method', 'type');
    const isExternalIdp = String(authMethod || '').toLowerCase().replace('_', '') === 'externalidp';
    const clientId = pick('clientId', 'client_id');
    return {
      refreshToken: pick('refreshToken', 'refresh_token'),
      accessToken: pick('accessToken', 'access_token'),
      // cliproxyapi external_idp puts the IdP client id in client_id; route it
      // to idpClientId so token refresh has the right client (else 401).
      clientId: isExternalIdp ? '' : clientId,
      clientSecret: pick('clientSecret', 'client_secret'),
      region: pick('region'),
      authMethod: authMethod === 'kiro' ? '' : authMethod,
      provider: pick('provider', 'idp'),
      issuerUrl: pick('issuerUrl', 'issuer_url'),
      idpClientId: pick('idpClientId', 'idp_client_id') || (isExternalIdp ? clientId : undefined),
      scopes: pick('scopes'),
      loginHint: pick('loginHint', 'login_hint'),
      idpTokenEndpoint: pick('idpTokenEndpoint', 'idp_token_endpoint', 'token_endpoint'),
      expiresAt: pick('expiresAt', 'expires_at'),
    };
  }
  function parseLineCredentials(text) {
    const items = [];
    let skipped = 0;
    for (const line of text.split(/\r?\n/)) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      let parts;
      if (trimmed.includes('----')) {
        parts = trimmed.split('----').map(s => s.trim());
      } else if (trimmed.includes('\t')) {
        parts = trimmed.split(/\t+/).map(s => s.trim());
      } else {
        parts = trimmed.split(/\s+/).map(s => s.trim());
      }
      if (parts.length < 5) { skipped++; continue; }
      const refreshToken = parts[2];
      if (!refreshToken) { skipped++; continue; }
      items.push({
        refreshToken,
        clientId: parts[3],
        clientSecret: parts[4],
      });
    }
    return { items, skipped };
  }
  async function importFromCookie() {
    const refreshToken = $('cookieRefreshToken').value.trim();
    if (!refreshToken) return toastWarning(t('cookie.refreshTokenMissing'));
    const provider = $('cookieProvider').value;
    const payload = { refreshToken, accessToken: '', clientId: '', clientSecret: '', authMethod: 'social', provider };
    const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('cookie.importSuccess') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function importApiKey() {
    const key = $('apikeyInput').value.trim();
    if (!key) return toastWarning(t('apikey.keyRequired'));
    const payload = {
      authMethod: 'api_key',
      kiroApiKey: key,
      nickname: $('apikeyNickname').value.trim() || '',
      region: $('apikeyApiRegion').value.trim() || 'us-east-1',
      authRegion: $('apikeyAuthRegion').value.trim() || 'us-east-1',
      apiRegion: $('apikeyApiRegion').value.trim() || 'us-east-1',
      enabled: true
    };
    const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('apikey.success') + ': ' + (d.account?.email || d.account?.id));
      autoRefreshNewAccount(d.account?.id);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function importApiKeysBatch() {
    const raw = $('apikeyBatchList').value.trim();
    if (!raw) { toastWarning(t('apikeyBatch.listRequired')); return; }
    const region = $('apikeyBatchRegion').value.trim() || 'us-east-1';
    const btn = $('importApikeyBatchBtn');
    btn.disabled = true;
    const dismiss = toast(t('apikeyBatch.processing'), 'info', { duration: 0 });
    try {
      const res = await api('/auth/apikeys-batch', {
        method: 'POST',
        body: JSON.stringify({ keys: raw, region, authRegion: region, apiRegion: region })
      });
      const d = await res.json();
      dismiss();
      if (!d.success) {
        toast(t('common.failed') + ': ' + (d.error || ''), 'error');
        return;
      }
      renderApiKeyBatchResults(d);
      toast(t('apikeyBatch.summary', d.imported || 0, d.skipped || 0, d.total || 0),
        (d.imported > 0) ? 'success' : 'warning');
      loadAccounts(); loadStats();
    } catch (e) {
      dismiss();
      toast(t('common.failed'), 'error');
    } finally {
      btn.disabled = false;
    }
  }
  function renderApiKeyBatchResults(d) {
    const box = $('apikeyBatchResults');
    if (!box) return;
    const rows = (d.results || []).map(r => {
      let status, cls;
      if (r.skipped) { status = '⊘ ' + t('apikeyBatch.skipped'); cls = 'warning-text'; }
      else if (r.error && !r.imported) { status = '✗ ' + escapeHtml(r.error); cls = 'error-text'; }
      else if (r.imported) {
        const credit = r.infoOk
          ? (formatCredit(r.usageCurrent) + ' / ' + formatCredit(r.usageLimit))
          : t('apikeyBatch.infoUnavailable');
        const email = r.email ? ' ' + escapeHtml(r.email) : '';
        status = '✓ ' + escapeHtml(credit) + email;
        cls = 'success-text';
      } else { status = '✗'; cls = 'error-text'; }
      return '<div class="test-log-line"><span class="font-mono text-xs">' + escapeHtml(r.maskedKey || '') +
        '</span> <span class="' + cls + '">' + status + '</span></div>';
    }).join('');
    box.innerHTML = rows || '<p class="help-block">' + escapeHtml(t('apikeyBatch.noResults')) + '</p>';
  }
  function formatCredit(n) {
    if (typeof n !== 'number' || !isFinite(n)) return '0';
    return Number.isInteger(n) ? String(n) : n.toFixed(1);
  }
  async function importSsoToken() {
    const res = await api('/auth/sso-token', {
      method: 'POST', body: JSON.stringify({
        bearerToken: $('ssoToken').value,
        region: $('ssoRegion').value
      })
    });
    const d = await res.json();
    if (d.success) {
      closeModal(); loadAccounts(); loadStats();
      const count = d.accounts?.length || 0;
      const errs = d.errors?.length || 0;
      let msg = t('sso.importSuccess', count);
      if (errs > 0) msg += t('sso.importPartial', errs);
      toastPrimary(msg, { duration: 5200 });
      if (d.accounts) d.accounts.forEach(a => autoRefreshNewAccount(a.id));
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  async function startBuilderIdLogin() {
    const region = $('builderIdRegion').value || 'us-east-1';
    const res = await api('/auth/builderid/start', { method: 'POST', body: JSON.stringify({ region }) });
    const d = await res.json();
    if (d.sessionId) {
      builderIdSession = d.sessionId;
      $('builderIdUserCode').textContent = d.userCode;
      $('builderIdVerifyUrl').textContent = d.verificationUri;
      $('builderIdStep1').classList.add('hidden');
      $('builderIdStep2').classList.remove('hidden');
      $('builderIdOpenBtn').addEventListener('click', () => window.open($('builderIdVerifyUrl').textContent, '_blank'));
      $('builderIdCopyBtn').addEventListener('click', async () => {
        await copyText($('builderIdVerifyUrl').textContent);
        toast(t('common.copied'), 'primary');
      });
      $('builderIdCancelBtn').addEventListener('click', cancelBuilderIdLogin);
      pollBuilderIdAuth(d.interval || 5);
    } else toastError(t('common.failed') + ': ' + (d.error || ''));
  }
  function pollBuilderIdAuth(interval) {
    builderIdPollTimer = setTimeout(async () => {
      try {
        const res = await api('/auth/builderid/poll', { method: 'POST', body: JSON.stringify({ sessionId: builderIdSession }) });
        const d = await res.json();
        if (d.completed) {
          closeModal(); loadAccounts(); loadStats();
          toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
          autoRefreshNewAccount(d.account?.id);
        } else if (d.success && !d.completed) {
          $('builderIdStatus').textContent = t('builderid.waiting');
          pollBuilderIdAuth(d.interval || interval);
        } else {
          toastError(t('common.failed') + ': ' + (d.error || ''));
          cancelBuilderIdLogin();
        }
      } catch (e) {
        // Server restarted / network dropped mid-poll: surface it and reset the
        // modal instead of leaving the poll loop dead and the UI stuck on
        // "waiting" with no way out.
        toastError(t('login.connectError'));
        cancelBuilderIdLogin();
      }
    }, interval * 1000);
  }
  function cancelBuilderIdLogin() {
    if (builderIdPollTimer) { clearTimeout(builderIdPollTimer); builderIdPollTimer = null; }
    builderIdSession = '';
    showModal('add');
  }
  async function startIamSso() {
    // Wrap the whole flow: a dropped connection mid-request must surface an
    // error and re-enable the button, not throw uncaught and leave it stuck.
    try {
      if (iamSession) {
        const res = await api('/auth/iam-sso/complete', {
          method: 'POST', body: JSON.stringify({
            sessionId: iamSession, callbackUrl: $('iamCallback').value
          })
        });
        const d = await res.json();
        if (d.success) {
          closeModal(); loadAccounts(); loadStats();
          toastPrimary(t('builderid.success') + ': ' + (d.account?.email || d.account?.id));
          autoRefreshNewAccount(d.account?.id);
        } else toastError(t('common.failed') + ': ' + (d.error || ''));
      } else {
        const res = await api('/auth/iam-sso/start', {
          method: 'POST', body: JSON.stringify({
            startUrl: $('iamStartUrl').value, region: $('iamRegion').value
          })
        });
        const d = await res.json();
        if (d.authorizeUrl) {
          iamSession = d.sessionId;
          $('iamAuthUrl').textContent = d.authorizeUrl;
          $('iamStep2').classList.remove('hidden');
          $('iamBtn').textContent = t('iam.complete');
          $('iamOpenBtn').addEventListener('click', () => window.open($('iamAuthUrl').textContent, '_blank'));
          $('iamCopyBtn').addEventListener('click', async () => {
            await copyText($('iamAuthUrl').textContent);
            toast(t('common.copied'), 'primary');
          });
        } else toastError(t('common.failed') + ': ' + (d.error || ''));
      }
    } catch (e) {
      toastError(t('login.connectError'));
    }
  }
  async function autoRefreshNewAccount(id) {
    if (!id) return;
    try { await api('/accounts/' + id + '/refresh', { method: 'POST' }); } catch (e) { }
    loadAccounts();
  }

  // Export modal
  function showExportModal() {
    if (!accountsData.length) return toastWarning(t('accounts.empty'));
    exportSelectedIds = new Set(accountsData.map(a => a.id));
    renderExportModal();
    openDialog('exportModal');
  }
  function closeExportModal() { closeDialog('exportModal'); }
  function renderExportModal() {
    const body = $('exportBody');
    const all = exportSelectedIds.size === accountsData.length;
    body.innerHTML =
      '<div class="flex items-center justify-between mb-3">' +
      '<span class="text-sm muted-text">' + escapeHtml(t('export.selected', exportSelectedIds.size)) + '</span>' +
      '<button class="btn btn-sm btn-outline" id="exportToggleAllBtn" type="button">' + escapeHtml(all ? t('export.deselectAll') : t('export.selectAll')) + '</button>' +
      '</div>' +
      '<div class="export-list">' +
      accountsData.map(a => {
        const checked = exportSelectedIds.has(a.id);
        return '<label class="export-row' + (checked ? ' selected' : '') + '">' +
          '<input type="checkbox" ' + (checked ? 'checked' : '') + ' data-export-toggle="' + escapeAttr(a.id) + '" />' +
          '<div class="export-row-text">' +
          '<div class="export-row-email">' + escapeHtml(getDisplayEmail(a.email, a.id)) + '</div>' +
          '<div class="export-row-meta">' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + ' · ' + escapeHtml(formatSubscriptionLabel(a.subscriptionType)) + '</div>' +
          '</div>' +
          '</label>';
      }).join('') +
      '</div>' +
      '<div id="exportJsonPreview" class="hidden mb-3"><textarea id="exportJsonText" readonly class="font-mono"></textarea></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="exportCloseBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-outline" id="exportShowJsonBtn" type="button">' + escapeHtml(t('export.showJson')) + '</button>' +
      '<button class="btn btn-outline" id="exportCopyJsonBtn" type="button">' + escapeHtml(t('export.copyJson')) + '</button>' +
      '<button class="btn btn-primary" id="exportDownloadBtn" type="button">' + escapeHtml(t('export.downloadJson')) + '</button>' +
      '</div>';
    $('exportToggleAllBtn').addEventListener('click', () => {
      if (exportSelectedIds.size === accountsData.length) exportSelectedIds.clear();
      else exportSelectedIds = new Set(accountsData.map(a => a.id));
      renderExportModal();
    });
    $('exportCloseBtn').addEventListener('click', closeExportModal);
    $('exportShowJsonBtn').addEventListener('click', exportShowJson);
    $('exportCopyJsonBtn').addEventListener('click', exportCopyJson);
    $('exportDownloadBtn').addEventListener('click', exportDownloadJson);
    qsa('[data-export-toggle]', body).forEach(cb => cb.addEventListener('change', e => {
      const id = e.target.dataset.exportToggle;
      if (exportSelectedIds.has(id)) exportSelectedIds.delete(id);
      else exportSelectedIds.add(id);
      renderExportModal();
    }));
  }
  async function getExportData() {
    if (exportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return null; }
    const res = await api('/export', { method: 'POST', body: JSON.stringify({ ids: Array.from(exportSelectedIds) }) });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      toastError(t('common.failed') + ': ' + (err.error || t('common.unknownError')));
      return null;
    }
    return res.json();
  }
  async function exportShowJson() {
    const data = await getExportData();
    if (!data) return;
    $('exportJsonPreview').classList.remove('hidden');
    $('exportJsonText').value = JSON.stringify(data, null, 2);
  }
  async function exportCopyJson() {
    if (exportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return; }
    const jsonPromise = getExportData().then(data => {
      if (!data) throw new Error('no-data');
      const filtered = (data.accounts || []).map(a => {
        const c = a.credentials || {};
        const { clientId, clientSecret, accessToken, refreshToken,
          authMethod, provider, issuerUrl, idpClientId, scopes, loginHint, region, expiresAt } = c;
        return { clientId, clientSecret, accessToken, refreshToken,
          authMethod, provider, issuerUrl, idpClientId, scopes, loginHint, region, expiresAt };
      });
      return JSON.stringify(filtered, null, 2);
    });
    try {
      await copyText(jsonPromise);
      toast(t('export.copied'), 'primary');
    } catch (e) {
      if (e && e.message !== 'no-data') toastError(t('common.failed'));
    }
  }
  async function exportDownloadJson() {
    const data = await getExportData();
    if (!data) return;
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'kiro-accounts-' + new Date().toISOString().slice(0, 10) + '.json';
    a.click();
    URL.revokeObjectURL(url);
  }

  // API key usage export modal (masked report, not re-importable)
  function showApiKeyExportModal() {
    if (!apiKeysCache.length) return toastWarning(t('apiKeys.export.empty'));
    apiKeyExportSelectedIds = new Set(apiKeysCache.map(k => k.id));
    renderApiKeyExportModal();
    openDialog('exportModal');
  }
  function renderApiKeyExportModal() {
    const body = $('exportBody');
    const all = apiKeyExportSelectedIds.size === apiKeysCache.length;
    body.innerHTML =
      '<div class="flex items-center justify-between mb-3">' +
      '<span class="text-sm muted-text">' + escapeHtml(t('export.selected', apiKeyExportSelectedIds.size)) + '</span>' +
      '<button class="btn btn-sm btn-outline" id="apiKeyExportToggleAllBtn" type="button">' + escapeHtml(all ? t('export.deselectAll') : t('export.selectAll')) + '</button>' +
      '</div>' +
      '<div class="export-list">' +
      apiKeysCache.map(k => {
        const checked = apiKeyExportSelectedIds.has(k.id);
        const label = k.name || k.keyMasked || k.id;
        const meta = (k.keyMasked || '') + ' · ' + t('apiKeys.export.reqMeta', k.requestsCount || 0) + (k.enabled ? '' : ' · ' + t('apiKeys.export.disabled'));
        return '<label class="export-row' + (checked ? ' selected' : '') + '">' +
          '<input type="checkbox" ' + (checked ? 'checked' : '') + ' data-apikey-export-toggle="' + escapeAttr(k.id) + '" />' +
          '<div class="export-row-text">' +
          '<div class="export-row-email">' + escapeHtml(label) + '</div>' +
          '<div class="export-row-meta">' + escapeHtml(meta) + '</div>' +
          '</div>' +
          '</label>';
      }).join('') +
      '</div>' +
      '<div id="apiKeyExportJsonPreview" class="hidden mb-3"><textarea id="apiKeyExportJsonText" readonly class="font-mono"></textarea></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="apiKeyExportCloseBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-outline" id="apiKeyExportShowJsonBtn" type="button">' + escapeHtml(t('export.showJson')) + '</button>' +
      '<button class="btn btn-outline" id="apiKeyExportCopyJsonBtn" type="button">' + escapeHtml(t('export.copyJson')) + '</button>' +
      '<button class="btn btn-outline" id="apiKeyExportCsvBtn" type="button">' + escapeHtml(t('apiKeys.export.downloadCsv')) + '</button>' +
      '<button class="btn btn-primary" id="apiKeyExportJsonBtn" type="button">' + escapeHtml(t('export.downloadJson')) + '</button>' +
      '</div>';
    $('apiKeyExportToggleAllBtn').addEventListener('click', () => {
      if (apiKeyExportSelectedIds.size === apiKeysCache.length) apiKeyExportSelectedIds.clear();
      else apiKeyExportSelectedIds = new Set(apiKeysCache.map(k => k.id));
      renderApiKeyExportModal();
    });
    $('apiKeyExportCloseBtn').addEventListener('click', () => closeDialog('exportModal'));
    $('apiKeyExportShowJsonBtn').addEventListener('click', apiKeyExportShowJson);
    $('apiKeyExportCopyJsonBtn').addEventListener('click', apiKeyExportCopyJson);
    $('apiKeyExportCsvBtn').addEventListener('click', apiKeyExportDownloadCsv);
    $('apiKeyExportJsonBtn').addEventListener('click', apiKeyExportDownloadJson);
    qsa('[data-apikey-export-toggle]', body).forEach(cb => cb.addEventListener('change', e => {
      const id = e.target.dataset.apikeyExportToggle;
      if (apiKeyExportSelectedIds.has(id)) apiKeyExportSelectedIds.delete(id);
      else apiKeyExportSelectedIds.add(id);
      renderApiKeyExportModal();
    }));
  }
  async function getApiKeyExportData() {
    if (apiKeyExportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return null; }
    const res = await api('/api-keys/export', { method: 'POST', body: JSON.stringify({ ids: Array.from(apiKeyExportSelectedIds) }) });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      toastError(t('common.failed') + ': ' + (err.error || t('common.unknownError')));
      return null;
    }
    return res.json();
  }
  async function apiKeyExportShowJson() {
    const data = await getApiKeyExportData();
    if (!data) return;
    $('apiKeyExportJsonPreview').classList.remove('hidden');
    $('apiKeyExportJsonText').value = JSON.stringify(data, null, 2);
  }
  async function apiKeyExportCopyJson() {
    if (apiKeyExportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return; }
    const jsonPromise = getApiKeyExportData().then(data => {
      if (!data) throw new Error('no-data');
      return JSON.stringify(data, null, 2);
    });
    try {
      await copyText(jsonPromise);
      toast(t('export.copied'), 'primary');
    } catch (e) {
      if (e && e.message !== 'no-data') toastError(t('common.failed'));
    }
  }
  async function apiKeyExportDownloadJson() {
    const data = await getApiKeyExportData();
    if (!data) return;
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'kiro-apikeys-' + new Date().toISOString().slice(0, 10) + '.json';
    a.click();
    URL.revokeObjectURL(url);
  }
  function csvCell(v) {
    const s = String(v == null ? '' : v);
    if (/[",\n]/.test(s)) return '"' + s.replace(/"/g, '""') + '"';
    return s;
  }
  function buildApiKeyCsv(data) {
    const cols = ['id', 'name', 'keyMasked', 'enabled', 'requestsCount', 'tokensUsed',
      'creditsUsed', 'tokenLimit', 'creditLimit', 'tokenPercentUsed', 'creditPercentUsed',
      'overToken', 'overCredit', 'expired', 'expiresAt', 'createdAt', 'lastUsedAt'];
    const rows = [cols.join(',')];
    (data.apiKeys || []).forEach(k => {
      rows.push(cols.map(c => csvCell(k[c])).join(','));
    });
    return rows.join('\r\n');
  }
  async function apiKeyExportDownloadCsv() {
    const data = await getApiKeyExportData();
    if (!data) return;
    const blob = new Blob([buildApiKeyCsv(data)], { type: 'text/csv;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'kiro-apikeys-' + new Date().toISOString().slice(0, 10) + '.csv';
    a.click();
    URL.revokeObjectURL(url);
  }

  // Version and update
  function renderVersionBadge() {
    const badge = $('versionBadge');
    if (badge && currentVersion) badge.textContent = currentVersion.replace(/^v/i, '');
  }
  async function loadVersion() {
    try {
      const res = await api('/version');
      const d = await res.json();
      currentVersion = d.version || '';
      renderVersionBadge();
    } catch (e) { }
  }
  function compareVersions(a, b) {
    const pa = a.split('.').map(Number);
    const pb = b.split('.').map(Number);
    for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
      const na = pa[i] || 0, nb = pb[i] || 0;
      if (na > nb) return 1;
      if (na < nb) return -1;
    }
    return 0;
  }
  function setUpdateButtonLoading(loading) {
    const btn = $('checkUpdateBtn');
    if (!btn) return;
    btn.disabled = loading;
    if (loading) btn.setAttribute('aria-busy', 'true');
    else btn.removeAttribute('aria-busy');
    const label = btn.querySelector('[data-update-label]');
    const icon = btn.querySelector('i');
    if (label) label.textContent = t(loading ? 'update.checking' : 'update.check');
    if (icon) icon.classList.toggle('fa-spin', loading);
  }
  async function checkUpdate(manual) {
    if (manual) setUpdateButtonLoading(true);
    try {
      if (!currentVersion) await loadVersion();
      const current = currentVersion.replace(/^v/i, '');
      if (!current) throw new Error('Current version missing');
      const res = await fetch('https://raw.githubusercontent.com/Quorinex/Kiro-Go/main/version.json?t=' + Date.now());
      if (!res.ok) throw new Error('Fetch failed');
      const d = await res.json();
      const latest = (d.version || '').replace(/^v/i, '');
      if (!latest) throw new Error('Latest version missing');
      if (latest && latest !== current && compareVersions(latest, current) > 0) {
        if (manual) showUpdateModal(latest, d.download, d.changelog);
        else showUpdateToast('available', current, latest);
      } else if (manual) {
        showUpdateToast('current', current, latest || current);
      }
    } catch (e) {
      if (manual) showUpdateToast('error', '', '');
    } finally {
      if (manual) setUpdateButtonLoading(false);
    }
  }
  function showUpdateToast(status, current, latest) {
    if (status === 'available') {
      toast(t('update.availableToast') + (latest ? ': ' + latest : ''), 'warning', {
        icon: 'fa-solid fa-arrow-up',
        duration: 5200,
        onClick: function () { checkUpdate(true); }
      });
      return;
    }
    if (status === 'current') {
      toast(t('update.noUpdatesToast'), 'success', {
        icon: 'fa-solid fa-circle-check',
        duration: 3600
      });
      return;
    }
    toast(t('update.checkFailed'), 'error', {
      icon: 'fa-solid fa-triangle-exclamation',
      duration: 4200
    });
  }
  function showUpdateModal(version, url, changelog) {
    const current = currentVersion.replace(/^v/i, '');
    $('updateBody').innerHTML =
      '<div class="update-shell">' +
      '<div class="update-hero">' +
      '<div class="update-result-icon update-result-info"><i class="fa-solid fa-arrow-up"></i></div>' +
      '<div>' +
      '<h3 class="update-hero-title">' + escapeHtml(t('update.newVersion')) + '</h3>' +
      '<p class="update-hero-copy">' + escapeHtml(t('update.newVersionMessage')) + '</p>' +
      '</div>' +
      '</div>' +
      '<div class="update-version-grid">' +
      '<div class="update-version-card update-version-card-current"><p class="update-version-label">' + escapeHtml(t('update.current')) + '</p><p class="update-version-value update-version-value-current">' + escapeHtml(current) + '</p></div>' +
      '<div class="update-version-card update-version-card-latest"><p class="update-version-label">' + escapeHtml(t('update.latest')) + '</p><p class="update-version-value update-version-value-success">' + escapeHtml(version) + '</p></div>' +
      '</div>' +
      (changelog ? '<div class="update-notes"><p class="update-notes-title">' + escapeHtml(t('update.changelog')) + '</p><p class="update-notes-body">' + escapeHtml(changelog) + '</p></div>' : '') +
      '<div class="update-actions"><a href="' + escapeAttr(url) + '" target="_blank" rel="noopener" class="btn btn-primary">' + escapeHtml(t('update.goDownload')) + '</a></div>' +
      '</div>';
    openDialog('updateModal');
  }
  function showUpdateStatusModal(status, title, message, latest) {
    const current = currentVersion.replace(/^v/i, '');
    const isError = status === 'error';
    $('updateBody').innerHTML =
      '<div class="update-shell">' +
      '<div class="text-center mb-5">' +
      '<div class="update-result-icon update-status-icon update-result-' + (isError ? 'error' : 'success') + '">' +
      '<i class="fa-solid ' + (isError ? 'fa-triangle-exclamation' : 'fa-circle-check') + '"></i>' +
      '</div>' +
      '<p class="text-base font-semibold ' + (isError ? 'danger-text' : 'success-text') + '">' + escapeHtml(title) + '</p>' +
      '<p class="text-sm mt-2 muted-text">' + escapeHtml(message) + '</p>' +
      '</div>' +
      '<div class="update-version-grid">' +
      '<div class="update-version-card update-version-card-current"><p class="update-version-label">' + escapeHtml(t('update.current')) + '</p><p class="update-version-value update-version-value-current">' + escapeHtml(current || '-') + '</p></div>' +
      '<div class="update-version-card' + (!isError ? ' update-version-card-latest' : '') + '"><p class="update-version-label">' + escapeHtml(t('update.latest')) + '</p><p class="update-version-value' + (!isError ? ' update-version-value-success' : '') + '">' + escapeHtml(latest || '-') + '</p></div>' +
      '</div>' +
      '</div>';
    openDialog('updateModal');
  }
  function closeUpdateModal() { closeDialog('updateModal'); }

  // Console (realtime logs)
  let consoleSource = null;
  let consolePaused = false;
  let consoleAutoscroll = true;
  let consoleFilter = 'all';
  let consoleQueue = [];
  let consoleRafScheduled = false;
  const CONSOLE_MAX_LINES = 2000;
  const consoleLevelRank = { debug: 0, info: 1, warn: 2, error: 3 };
  // Older server builds prefixed each line with "LEVEL  YYYY/MM/DD HH:MM:SS ";
  // strip it so the web console doesn't render the level/timestamp twice.
  const consolePrefixRe = /^(?:DEBUG|INFO|WARN|ERROR)\s+\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2}\s+/;

  function consoleSetStatus(state) {
    const el = $('consoleStatus');
    const txt = $('consoleStatusText');
    if (!el || !txt) return;
    el.dataset.state = state;
    txt.textContent = t('console.' + state);
  }

  function consoleBuildLine(entry) {
    const level = entry.level || 'info';
    const line = document.createElement('div');
    line.className = 'console-line console-' + level;
    line.dataset.level = level;
    const ts = new Date(entry.ts || Date.now());
    const hh = String(ts.getHours()).padStart(2, '0');
    const mm = String(ts.getMinutes()).padStart(2, '0');
    const ss = String(ts.getSeconds()).padStart(2, '0');

    const tsEl = document.createElement('span');
    tsEl.className = 'console-ts';
    tsEl.textContent = hh + ':' + mm + ':' + ss;
    const badgeEl = document.createElement('span');
    badgeEl.className = 'console-badge';
    badgeEl.textContent = level.toUpperCase();
    const textEl = document.createElement('span');
    textEl.className = 'console-text';
    textEl.textContent = String(entry.text || '').replace(consolePrefixRe, '');

    line.appendChild(tsEl);
    line.appendChild(badgeEl);
    line.appendChild(textEl);
    if (consoleFilter !== 'all' && consoleLevelRank[level] < consoleLevelRank[consoleFilter]) {
      line.classList.add('console-hidden');
    }
    return line;
  }

  // Coalesce bursts: queued entries are flushed to the DOM once per animation
  // frame via a single DocumentFragment, so render cost stays bounded (~60fps)
  // no matter how fast log lines arrive.
  function consoleFlushQueue() {
    consoleRafScheduled = false;
    const out = $('consoleOutput');
    if (!out) { consoleQueue.length = 0; return; }
    if (!consoleQueue.length) return;

    const batch = consoleQueue;
    consoleQueue = [];
    const frag = document.createDocumentFragment();
    for (let i = 0; i < batch.length; i++) frag.appendChild(consoleBuildLine(batch[i]));
    out.appendChild(frag);
    while (out.childElementCount > CONSOLE_MAX_LINES) out.removeChild(out.firstChild);
    if (consoleAutoscroll) out.scrollTop = out.scrollHeight;
  }

  function consoleAppend(entry) {
    consoleQueue.push(entry);
    if (consoleQueue.length > CONSOLE_MAX_LINES) {
      consoleQueue.splice(0, consoleQueue.length - CONSOLE_MAX_LINES);
    }
    if (!consoleRafScheduled) {
      consoleRafScheduled = true;
      requestAnimationFrame(consoleFlushQueue);
    }
  }

  function consoleApplyFilter() {
    qsa('.console-line', $('consoleOutput')).forEach(line => {
      const hide = consoleFilter !== 'all' && consoleLevelRank[line.dataset.level] < consoleLevelRank[consoleFilter];
      line.classList.toggle('console-hidden', hide);
    });
  }

  function openConsole() {
    if (consoleSource) return;
    const out = $('consoleOutput');
    if (out) out.innerHTML = '';
    consoleQueue.length = 0;
    consoleSetStatus('connecting');
    // Sync the active level selector.
    api('/logs/level').then(r => r.ok ? r.json() : null).then(d => {
      if (d && d.level) $('consoleLevel').value = d.level;
      refreshCustomSelects($('tabConsole'));
    }).catch(() => { });
    // EventSource authenticates via the HttpOnly admin_session cookie (sent
    // automatically for same-origin requests; EventSource cannot set headers).
    const src = new EventSource('/admin/api/logs/stream');
    consoleSource = src;
    src.onopen = () => consoleSetStatus('connected');
    src.onmessage = ev => {
      if (consolePaused) return;
      let entry;
      try { entry = JSON.parse(ev.data); } catch (e) { return; }
      consoleAppend(entry);
    };
    src.onerror = () => {
      consoleSetStatus('connecting');
      // EventSource auto-reconnects; status flips back on the next onopen.
    };
  }

  function closeConsole() {
    if (consoleSource) {
      consoleSource.close();
      consoleSource = null;
    }
  }

  function bindConsoleEvents() {
    const levelSel = $('consoleLevel');
    if (levelSel) levelSel.addEventListener('change', async () => {
      try {
        const res = await api('/logs/level', { method: 'POST', body: JSON.stringify({ level: levelSel.value }) });
        if (res.ok) toastPrimary(t('console.levelChanged', levelSel.value));
        else toastError(t('common.failed'));
      } catch (e) { toastError(t('common.failed')); }
    });
    const filterSel = $('consoleFilter');
    if (filterSel) filterSel.addEventListener('change', () => {
      consoleFilter = filterSel.value;
      consoleApplyFilter();
    });
    const pauseBtn = $('consolePauseBtn');
    if (pauseBtn) pauseBtn.addEventListener('click', () => {
      consolePaused = !consolePaused;
      pauseBtn.dataset.active = String(consolePaused);
      pauseBtn.textContent = t(consolePaused ? 'console.resume' : 'console.pause');
    });
    const autoBtn = $('consoleAutoscrollBtn');
    if (autoBtn) autoBtn.addEventListener('click', () => {
      consoleAutoscroll = !consoleAutoscroll;
      autoBtn.dataset.active = String(consoleAutoscroll);
    });
    const clearBtn = $('consoleClearBtn');
    if (clearBtn) clearBtn.addEventListener('click', () => {
      consoleQueue.length = 0;
      const out = $('consoleOutput');
      if (out) out.innerHTML = '';
    });
  }

  // API Log + Usage check
  function formatLogTime(unixSec) {
    if (!unixSec) return '';
    const d = new Date(unixSec * 1000);
    const pad = n => String(n).padStart(2, '0');
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
      ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
  }

  function normalizeUnixSeconds(v) {
    const n = Number(v);
    if (!isFinite(n) || n <= 0) return 0;
    return n > 1e12 ? Math.floor(n / 1000) : Math.floor(n);
  }

  // Convert a Unix seconds timestamp to a value for <input type="date"> (local yyyy-mm-dd).
  function unixToLocalInput(unixSec) {
    unixSec = normalizeUnixSeconds(unixSec);
    if (!unixSec) return '';
    const d = new Date(unixSec * 1000);
    const pad = n => String(n).padStart(2, '0');
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate());
  }

  // Convert a <input type="date"> value (yyyy-mm-dd) to Unix seconds at end of that
  // day in local time. Empty input → 0 (never expires).
  function localInputToUnix(val) {
    if (!val) return 0;
    const parts = val.split('-');
    if (parts.length !== 3) return 0;
    const d = new Date(+parts[0], +parts[1] - 1, +parts[2], 23, 59, 59);
    const ms = d.getTime();
    return isNaN(ms) ? 0 : Math.floor(ms / 1000);
  }

  // Small colored badge shown next to a key name: red when expired, amber when
  // expiring within 3 days, nothing otherwise (or when never-expires).
  function apiKeyExpiryBadge(expiresAt) {
    expiresAt = normalizeUnixSeconds(expiresAt);
    if (!expiresAt) return '';
    const now = Math.floor(Date.now() / 1000);
    if (expiresAt <= now) {
      return '<span class="text-xs" style="background:rgba(239,68,68,0.15);color:#ef4444;padding:1px 6px;border-radius:4px;">' + escapeHtml(t('apiKeys.expired')) + '</span>';
    }
    if (expiresAt - now <= 3 * 86400) {
      return '<span class="text-xs" style="background:rgba(245,158,11,0.15);color:#f59e0b;padding:1px 6px;border-radius:4px;">' + escapeHtml(t('apiKeys.expiringSoon')) + '</span>';
    }
    return '';
  }

  // Full "Expires: <date>" line for the key/usage cards. Empty when never expires.
  function apiKeyExpiryLine(expiresAt) {
    expiresAt = normalizeUnixSeconds(expiresAt);
    const val = expiresAt ? formatLogTime(expiresAt) : t('apiKeys.neverExpires');
    return '<div class="text-xs muted-text">' + escapeHtml(t('apiKeys.expiry')) + ': ' + escapeHtml(val) + '</div>';
  }

  let apiLogCache = [];
  let apiLogFilter = 'all';
  let apiLogSearch = '';
  let apiLogSource = null;
  let apiLogRenderScheduled = false;
  // Cap the retained rows so a busy proxy cannot grow the table without bound.
  const APILOG_MAX_ROWS = 500;

  async function loadApiLog() {
    const body = $('apiLogBody');
    if (!body) return;
    const keyId = $('apiLogKeyFilter') ? $('apiLogKeyFilter').value : '';
    let entries = [];
    try {
      const qs = keyId ? ('?apiKeyId=' + encodeURIComponent(keyId)) : '';
      const res = await api('/request-logs' + qs);
      const d = await res.json().catch(() => ({}));
      entries = Array.isArray(d.logs) ? d.logs : [];
    } catch (e) {
      entries = [];
    }
    apiLogCache = entries;
    renderApiLog(entries);
  }

  // Applies status filter + free-text search client-side (the /request-logs feed is unfiltered).
  function filterApiLog(entries) {
    let out = entries;
    if (apiLogFilter === 'success') out = out.filter(e => e.status !== 'error');
    else if (apiLogFilter === 'error') out = out.filter(e => e.status === 'error');
    if (apiLogSearch) {
      const kw = apiLogSearch.toLowerCase();
      out = out.filter(e =>
        (e.model || '').toLowerCase().includes(kw) ||
        (e.endpoint || '').toLowerCase().includes(kw) ||
        (e.apiKeyName || '').toLowerCase().includes(kw) ||
        (e.apiKeyMasked || '').toLowerCase().includes(kw) ||
        (e.accountEmail || '').toLowerCase().includes(kw) ||
        (e.clientIp || '').toLowerCase().includes(kw) ||
        (e.error || '').toLowerCase().includes(kw));
    }
    return out;
  }

  function renderApiLogMetrics(entries) {
    const box = $('metricsSummary');
    if (!box) return;
    const total = entries.length;
    let errorCount = 0, durSum = 0, durCount = 0;
    for (const e of entries) {
      if (e.status === 'error') errorCount++;
      if (e.durationMs) { durSum += e.durationMs; durCount++; }
    }
    const avg = durCount ? Math.round(durSum / durCount) : 0;
    box.innerHTML =
      '<span>' + escapeHtml(t('metrics.logCount')) + ': <strong>' + escapeHtml(String(total)) + '</strong></span>' +
      '<span>' + escapeHtml(t('metrics.avgLatency')) + ': <strong>' + escapeHtml(String(avg)) + 'ms</strong></span>' +
      '<span>' + escapeHtml(t('metrics.quotaErrors')) + ': <strong>' + escapeHtml(String(errorCount)) + '</strong></span>';
  }

  function renderApiLog(allEntries) {
    const body = $('apiLogBody');
    const empty = $('apiLogEmpty');
    const summary = $('apiLogSummary');
    if (!body) return;
    const entries = filterApiLog(allEntries);
    renderApiLogMetrics(entries);
    if (!entries.length) {
      body.innerHTML = '';
      if (empty) empty.classList.remove('hidden');
      if (summary) summary.innerHTML = '';
      return;
    }
    if (empty) empty.classList.add('hidden');

    let sumInput = 0, sumOutput = 0, sumCredits = 0, errorCount = 0;
    const rows = entries.map(e => {
      sumInput += e.inputTokens || 0;
      sumOutput += e.outputTokens || 0;
      sumCredits += e.credits || 0;
      const isError = e.status === 'error';
      if (isError) errorCount++;
      const keyLabel = e.apiKeyMasked
        ? (e.apiKeyName ? escapeHtml(e.apiKeyName) + ' ' : '') + '<span class="font-mono text-xs">' + escapeHtml(e.apiKeyMasked) + '</span>'
        : '<span class="muted-text">' + escapeHtml(t('apilog.noKey')) + '</span>';
      const accountLabel = (e.accountEmail || e.accountId)
        ? '<span class="text-xs">' + escapeHtml(getDisplayEmail(e.accountEmail, e.accountId)) + '</span>'
        : '<span class="muted-text">-</span>';
      const statusBadge = isError
        ? '<span class="text-xs" style="background:rgba(239,68,68,0.15);color:#ef4444;padding:1px 6px;border-radius:4px;">' + escapeHtml(t('apilog.statusError')) + '</span>'
        : '<span class="text-xs" style="background:rgba(34,197,94,0.15);color:#22c55e;padding:1px 6px;border-radius:4px;">' + escapeHtml(t('apilog.statusOk')) + '</span>';
      const dash = '<span class="muted-text">-</span>';
      const detailCell = isError
        ? '<span class="text-xs" style="color:#ef4444;">' + (e.statusCode ? escapeHtml('HTTP ' + e.statusCode + ' ') : '') + escapeHtml(e.error || '') + '</span>'
        : dash;
      const durationCell = e.durationMs ? escapeHtml(formatNumber(e.durationMs)) + 'ms' : dash;
      const numOrDash = v => (isError && !v) ? dash : escapeHtml(formatNumber(v || 0));
      // Prompt-cache traffic. Only the Claude paths track it, so a request with
      // no cache activity shows a dash rather than a misleading "0 / 0".
      const cacheCell = (e.cacheReadTokens || e.cacheWriteTokens)
        ? '<span style="color:#38bdf8;" title="' + escapeAttr(t('apilog.cacheRead')) + '">&#8635; ' +
            escapeHtml(formatNumber(e.cacheReadTokens || 0)) + '</span> / ' +
          '<span style="color:#a78bfa;" title="' + escapeAttr(t('apilog.cacheWrite')) + '">&#43; ' +
            escapeHtml(formatNumber(e.cacheWriteTokens || 0)) + '</span>'
        : dash;
      // Client IP cell doubles as a one-click ban entry point for abusive callers.
      const ipCell = e.clientIp
        ? '<span class="text-xs font-mono">' + escapeHtml(e.clientIp) + '</span>' +
          ' <button class="btn btn-outline btn-xs" type="button" data-log-action="ban-ip" data-ip="' +
          escapeAttr(e.clientIp) + '">' + escapeHtml(t('requestLogs.banIp')) + '</button>'
        : dash;
      return '<tr>' +
        '<td class="text-xs font-mono">' + escapeHtml(formatLogTime(e.time)) + '</td>' +
        '<td>' + statusBadge + '</td>' +
        '<td class="text-xs">' + escapeHtml(e.endpoint || '') + '</td>' +
        '<td>' + keyLabel + '</td>' +
        '<td>' + ipCell + '</td>' +
        '<td class="text-xs">' + escapeHtml(e.model || '') + '</td>' +
        '<td>' + (isError ? dash : accountLabel) + '</td>' +
        '<td class="num text-xs"><span style="color:#22c55e;">&#9660; ' + numOrDash(e.inputTokens) + '</span> / <span style="color:#f59e0b;">&#9650; ' + numOrDash(e.outputTokens) + '</span></td>' +
        '<td class="num text-xs">' + cacheCell + '</td>' +
        '<td class="num">' + numOrDash(e.totalTokens) + '</td>' +
        '<td class="num">' + numOrDash(e.credits) + '</td>' +
        '<td class="num text-xs">' + durationCell + '</td>' +
        '<td>' + detailCell + '</td>' +
        '</tr>';
    }).join('');
    body.innerHTML = rows;

    if (summary) {
      const errChip = errorCount
        ? '<span class="apilog-chip" style="color:#ef4444;">' + escapeHtml(t('apilog.totalErrors', errorCount)) + '</span>'
        : '';
      summary.innerHTML =
        '<span class="apilog-chip">' + escapeHtml(t('apilog.totalRequests', entries.length)) + '</span>' +
        errChip +
        '<span class="apilog-chip">' + escapeHtml(t('apilog.totalTokens', formatNumber(sumInput + sumOutput))) + '</span>' +
        '<span class="apilog-chip">' + escapeHtml(t('apilog.totalCredits', formatNumber(sumCredits))) + '</span>';
    }
  }

  async function clearApiLog() {
    if (!confirm(t('logs.clearConfirm'))) return;
    try {
      await api('/request-logs', { method: 'DELETE' });
    } catch (e) { /* ignore */ }
    apiLogCache = [];
    renderApiLog([]);
    toast(t('logs.cleared'), 'success');
  }

  function exportApiLog(format) {
    const entries = filterApiLog(apiLogCache);
    if (!entries.length) { toast(t('logs.exportFailed'), 'error'); return; }
    let blob;
    if (format === 'csv') {
      const cols = ['time', 'status', 'endpoint', 'apiKeyName', 'apiKeyMasked', 'clientIp', 'model', 'accountEmail', 'inputTokens', 'outputTokens', 'cacheReadTokens', 'cacheWriteTokens', 'totalTokens', 'credits', 'durationMs', 'statusCode', 'error'];
      const esc = v => {
        const s = String(v == null ? '' : v);
        return /[",\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
      };
      const lines = [cols.join(',')];
      for (const e of entries) lines.push(cols.map(c => esc(e[c])).join(','));
      blob = new Blob([lines.join('\n')], { type: 'text/csv' });
    } else {
      blob = new Blob([JSON.stringify(entries, null, 2)], { type: 'application/json' });
    }
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'kiro-go-request-logs.' + (format === 'csv' ? 'csv' : 'json');
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
    toast(t('logs.exported'), 'success');
  }

  // Re-render through renderApiLog (never straight into the DOM) so the status
  // filter, search box and summary chips stay authoritative. Coalesced on an
  // animation frame so a burst of requests costs one render, not one per event.
  function apiLogScheduleRender() {
    if (apiLogRenderScheduled) return;
    apiLogRenderScheduled = true;
    requestAnimationFrame(() => {
      apiLogRenderScheduled = false;
      renderApiLog(apiLogCache);
    });
  }

  function apiLogPrepend(entry) {
    // The stream carries every key; the dropdown filters server-side on the REST
    // feed, so honour it here instead of letting other keys leak into the view.
    const keySel = $('apiLogKeyFilter');
    const keyId = keySel ? keySel.value : '';
    if (keyId && entry.apiKeyId !== keyId) return;
    apiLogCache.unshift(entry);
    if (apiLogCache.length > APILOG_MAX_ROWS) apiLogCache.length = APILOG_MAX_ROWS;
    apiLogScheduleRender();
  }

  // Live SSE feed replacing the old 5s poll. EventSource authenticates via the
  // HttpOnly admin_session cookie (sent automatically same-origin; EventSource
  // cannot set headers).
  function openApiLogStream() {
    if (apiLogSource) return;
    const src = new EventSource('/admin/api/request-logs/stream');
    apiLogSource = src;
    src.onmessage = ev => {
      let msg;
      try { msg = JSON.parse(ev.data); } catch (e) { return; }
      if (!msg || msg.type !== 'requestlog' || !msg.data) return;
      apiLogPrepend(msg.data);
    };
    // EventSource reconnects on its own after a drop; nothing to do here.
  }

  function closeApiLogStream() {
    if (apiLogSource) {
      apiLogSource.close();
      apiLogSource = null;
    }
  }

  function toggleApiLogLive() {
    const box = $('logsAutoRefresh');
    if (box && box.checked) openApiLogStream();
    else closeApiLogStream();
  }

  function populateApiLogKeyFilter() {
    const sel = $('apiLogKeyFilter');
    if (!sel) return;
    const prev = sel.value;
    const opts = ['<option value="">' + escapeHtml(t('apilog.allKeys')) + '</option>'];
    (apiKeysCache || []).forEach(k => {
      const label = k.name || k.keyMasked || k.id;
      opts.push('<option value="' + escapeAttr(k.id) + '">' + escapeHtml(label) + '</option>');
    });
    sel.innerHTML = opts.join('');
    sel.value = prev;
    refreshCustomSelects($('tabApilog'));
  }

  // ---------------------------------------------------------------------------
  // IP bans
  // ---------------------------------------------------------------------------

  async function loadIPBans() {
    const body = $('ipBanBody');
    if (!body) return;
    try {
      const res = await api('/ip-bans');
      const d = await res.json().catch(() => ({}));
      renderIPBans(d || {});
    } catch (e) {
      renderIPBans({});
    }
  }

  function renderIPBans(data) {
    const body = $('ipBanBody');
    const empty = $('ipBanEmpty');
    if (!body) return;

    const enabledEl = $('ipBanEnabled');
    if (enabledEl) enabledEl.checked = !!data.enabled;
    const thresholdEl = $('ipBanThreshold');
    if (thresholdEl && data.threshold) thresholdEl.value = String(data.threshold);

    const bans = Array.isArray(data.bans) ? data.bans : [];
    if (!bans.length) {
      body.innerHTML = '';
      if (empty) empty.classList.remove('hidden');
    } else {
      if (empty) empty.classList.add('hidden');
      body.innerHTML = bans.map(b => {
        const ip = escapeAttr(b.ip || '');
        return '<tr>' +
          '<td class="text-xs font-mono">' + escapeHtml(b.ip || '') + '</td>' +
          '<td class="text-xs">' + escapeHtml(b.reason || '') + '</td>' +
          '<td class="num">' + escapeHtml(formatNumber(b.fails || 0)) + '</td>' +
          '<td class="text-xs font-mono">' + escapeHtml(formatLogTime(b.bannedUnix)) + '</td>' +
          '<td class="text-xs">' + escapeHtml(b.note || '') + '</td>' +
          '<td><button class="btn btn-outline btn-sm" type="button" data-ipban-action="unban" data-ip="' + ip + '">' +
            escapeHtml(t('ipBans.unban')) + '</button></td>' +
          '</tr>';
      }).join('');
    }

    const near = $('ipBanNearList');
    if (near) {
      const list = Array.isArray(data.nearThreshold) ? data.nearThreshold : [];
      near.innerHTML = list.length
        ? list.map(n => '<span class="apilog-chip">' + escapeHtml(n.ip || '') + ': ' +
            escapeHtml(String(n.count || 0)) + '/' + escapeHtml(String(n.threshold || 0)) + '</span>').join('')
        : '<span class="muted-text text-xs">' + escapeHtml(t('ipBans.noData')) + '</span>';
    }
  }

  async function addIPBan(ip, note) {
    ip = (ip || '').trim();
    if (!ip) return;
    try {
      const res = await api('/ip-bans', { method: 'POST', body: JSON.stringify({ ip, note: note || '' }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(d.error || t('common.failed'));
      toast(t('ipBans.banned', ip), 'success');
      const ipEl = $('ipBanAddIp');
      const noteEl = $('ipBanAddNote');
      if (ipEl) ipEl.value = '';
      if (noteEl) noteEl.value = '';
      await loadIPBans();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  async function removeIPBan(ip) {
    try {
      const res = await api('/ip-bans/' + encodeURIComponent(ip), { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(d.error || t('common.failed'));
      toast(t('ipBans.unbanned', ip), 'success');
      await loadIPBans();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  async function clearIPBans() {
    const ok = await confirmAction(t('ipBans.confirmClear'), {
      title: t('ipBans.clearAll'),
      confirmText: t('ipBans.clearAll'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/ip-bans', { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(d.error || t('common.failed'));
      toast(t('ipBans.cleared', d.removed || 0), 'success');
      await loadIPBans();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  async function saveIPBanSettings() {
    const enabledEl = $('ipBanEnabled');
    const thresholdEl = $('ipBanThreshold');
    const threshold = parseInt(thresholdEl ? thresholdEl.value : '', 10);
    if (isNaN(threshold) || threshold < 1) {
      toast(t('ipBans.thresholdError'), 'error');
      return;
    }
    try {
      const res = await api('/ip-bans/settings', {
        method: 'PATCH',
        body: JSON.stringify({ enabled: !!(enabledEl && enabledEl.checked), threshold })
      });
      const d = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(d.error || t('common.saveFailed'));
      toast(t('common.saved'), 'success');
      await loadIPBans();
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }

  // ---------------------------------------------------------------------------
  // Anomaly dashboard
  // ---------------------------------------------------------------------------

  async function loadAnomalies() {
    const body = $('anomalyBody');
    if (!body) return;
    try {
      const res = await api('/anomalies');
      const d = await res.json().catch(() => ({}));
      renderAnomalies(d || {});
    } catch (e) {
      renderAnomalies({});
    }
  }

  // Score colour bands mirror the backend's weighting: 60+ is worth interrupting an
  // operator over, 30+ is worth a look, anything lower is informational.
  function anomalyScoreColor(score) {
    if (score >= 60) return '#ef4444';
    if (score >= 30) return '#f59e0b';
    return '#3b82f6';
  }

  function renderAnomalies(data) {
    const body = $('anomalyBody');
    const empty = $('anomalyEmpty');
    if (!body) return;
    const rows = Array.isArray(data.anomalies) ? data.anomalies : [];
    if (!rows.length) {
      body.innerHTML = '';
      if (empty) empty.classList.remove('hidden');
      return;
    }
    if (empty) empty.classList.add('hidden');
    body.innerHTML = rows.map(a => {
      const keyId = escapeAttr(a.keyId || '');
      const label = a.keyName || a.keyId || '';
      const owner = a.ownerRef
        ? '<div class="text-xs muted-text">' + escapeHtml(a.ownerRef) + '</div>'
        : '';
      const reasons = Array.isArray(a.reasons) && a.reasons.length
        ? a.reasons.map(r => '<div class="text-xs">' + escapeHtml(r) + '</div>').join('')
        : '<span class="muted-text">-</span>';
      const topIP = (Array.isArray(a.topIPs) && a.topIPs.length) ? (a.topIPs[0].ip || '') : '';
      const banBtn = topIP
        ? '<button class="btn btn-outline btn-sm" type="button" data-anomaly-action="ban-ip" data-ip="' +
            escapeAttr(topIP) + '" title="' + escapeAttr(topIP) + '">' + escapeHtml(t('anomaly.banTopIp')) + '</button>'
        : '';
      const disableBtn = a.enabled
        ? '<button class="btn btn-outline btn-sm" type="button" data-anomaly-action="disable" data-id="' + keyId + '">' +
            escapeHtml(t('anomaly.disableKey')) + '</button>'
        : '';
      return '<tr>' +
        '<td><div class="text-xs font-semibold">' + escapeHtml(label) + '</div>' + owner + '</td>' +
        '<td class="num"><span style="color:' + anomalyScoreColor(a.score || 0) + ';font-weight:600;">' +
          escapeHtml(formatNumber(a.score || 0)) + '</span></td>' +
        '<td>' + reasons + '</td>' +
        '<td class="num">' + escapeHtml(formatNumber(a.distinctIPs || 0)) + '</td>' +
        '<td class="num">' + escapeHtml((a.errorRate || 0).toFixed(1)) + '%</td>' +
        '<td class="num">' + escapeHtml(formatNumber(a.burstRpm || 0)) + '</td>' +
        '<td><div class="flex items-center gap-2" style="flex-wrap:wrap;">' + disableBtn + banBtn +
          '<button class="btn btn-outline btn-sm" type="button" data-anomaly-action="reset" data-id="' + keyId + '">' +
          escapeHtml(t('anomaly.resetUsage')) + '</button></div></td>' +
        '</tr>';
    }).join('');
  }

  async function anomalyDisableKey(id) {
    const ok = await confirmAction(t('anomaly.confirmDisable'), {
      title: t('anomaly.disableKey'),
      confirmText: t('anomaly.disableKey'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id), { method: 'PUT', body: JSON.stringify({ enabled: false }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      toast(t('common.saved'), 'success');
      await loadAnomalies();
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }

  async function anomalyResetUsage(id) {
    const ok = await confirmAction(t('anomaly.confirmReset'), {
      title: t('anomaly.resetUsage'),
      confirmText: t('anomaly.resetUsage')
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id) + '/reset-usage', { method: 'POST' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('apiKeys.usageReset'), 'success');
      await loadAnomalies();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  // ---------------------------------------------------------------------------
  // Audit log
  // ---------------------------------------------------------------------------

  let auditActionFilter = '';

  async function loadAuditLog(action) {
    const body = $('auditBody');
    if (!body) return;
    action = action || '';
    try {
      const qs = action ? ('?action=' + encodeURIComponent(action)) : '';
      const res = await api('/audit-log' + qs);
      const d = await res.json().catch(() => ({}));
      renderAuditLog(d || {});
    } catch (e) {
      renderAuditLog({});
    }
  }

  function renderAuditLog(data) {
    const body = $('auditBody');
    const empty = $('auditEmpty');
    if (!body) return;
    const entries = Array.isArray(data.entries) ? data.entries : [];
    if (!entries.length) {
      body.innerHTML = '';
      if (empty) empty.classList.remove('hidden');
      return;
    }
    if (empty) empty.classList.add('hidden');
    const dash = '<span class="muted-text">-</span>';
    body.innerHTML = entries.map(e => '<tr>' +
      '<td class="text-xs font-mono">' + escapeHtml(formatLogTime(e.timeUnix)) + '</td>' +
      '<td class="text-xs">' + escapeHtml(e.actor || '') + '</td>' +
      '<td class="text-xs font-mono">' + escapeHtml(e.action || '') + '</td>' +
      '<td class="text-xs">' + (e.target ? escapeHtml(e.target) : dash) + '</td>' +
      '<td class="text-xs">' + (e.detail ? escapeHtml(e.detail) : dash) + '</td>' +
      '<td class="text-xs font-mono">' + (e.ip ? escapeHtml(e.ip) : dash) + '</td>' +
      '</tr>').join('');
  }

  // ---------------------------------------------------------------------------
  // Maintenance mode
  // ---------------------------------------------------------------------------

  function renderMaintenanceState(enabled, message) {
    const badge = $('maintenanceBadge');
    if (badge) badge.classList.toggle('hidden', !enabled);
    const msg = $('maintenanceMessage');
    if (msg && typeof message === 'string' && message && document.activeElement !== msg) {
      msg.value = message;
    }
  }

  async function setMaintenanceMode(enabled) {
    const msgEl = $('maintenanceMessage');
    const message = msgEl ? msgEl.value.trim() : '';
    try {
      const res = await api('/maintenance', { method: 'POST', body: JSON.stringify({ enabled, message }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(d.error || t('common.saveFailed'));
      renderMaintenanceState(enabled, message);
      toast(t(enabled ? 'maintenance.enabled' : 'maintenance.disabled'), enabled ? 'error' : 'success');
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }

  function bindSecurityEvents() {
    const ipBanAdd = $('ipBanAddBtn');
    if (ipBanAdd) ipBanAdd.addEventListener('click', () => {
      addIPBan($('ipBanAddIp').value, $('ipBanAddNote') ? $('ipBanAddNote').value : '');
    });
    const ipBanAddIp = $('ipBanAddIp');
    if (ipBanAddIp) ipBanAddIp.addEventListener('keypress', e => {
      if (e.key === 'Enter') addIPBan(ipBanAddIp.value, $('ipBanAddNote') ? $('ipBanAddNote').value : '');
    });
    const ipBanRefresh = $('ipBanRefreshBtn');
    if (ipBanRefresh) ipBanRefresh.addEventListener('click', loadIPBans);
    const ipBanClear = $('ipBanClearBtn');
    if (ipBanClear) ipBanClear.addEventListener('click', clearIPBans);
    const ipBanSave = $('ipBanSettingsSaveBtn');
    if (ipBanSave) ipBanSave.addEventListener('click', saveIPBanSettings);
    const ipBanEnabled = $('ipBanEnabled');
    if (ipBanEnabled) ipBanEnabled.addEventListener('change', saveIPBanSettings);
    const ipBanBody = $('ipBanBody');
    if (ipBanBody) ipBanBody.addEventListener('click', e => {
      const btn = e.target.closest('[data-ipban-action="unban"]');
      if (btn) removeIPBan(btn.dataset.ip);
    });

    const anomalyRefresh = $('anomalyRefreshBtn');
    if (anomalyRefresh) anomalyRefresh.addEventListener('click', loadAnomalies);
    const anomalyBody = $('anomalyBody');
    if (anomalyBody) anomalyBody.addEventListener('click', e => {
      const btn = e.target.closest('[data-anomaly-action]');
      if (!btn) return;
      const action = btn.dataset.anomalyAction;
      if (action === 'disable') anomalyDisableKey(btn.dataset.id);
      else if (action === 'reset') anomalyResetUsage(btn.dataset.id);
      else if (action === 'ban-ip') addIPBan(btn.dataset.ip, 'anomaly dashboard');
    });

    const auditRefresh = $('auditRefreshBtn');
    if (auditRefresh) auditRefresh.addEventListener('click', () => loadAuditLog(auditActionFilter));
    const auditFilter = $('auditActionFilter');
    if (auditFilter) auditFilter.addEventListener('change', () => {
      auditActionFilter = auditFilter.value;
      loadAuditLog(auditActionFilter);
    });

    const maintEnable = $('maintenanceEnableBtn');
    if (maintEnable) maintEnable.addEventListener('click', () => setMaintenanceMode(true));
    const maintDisable = $('maintenanceDisableBtn');
    if (maintDisable) maintDisable.addEventListener('click', () => setMaintenanceMode(false));

    const apiLogBody = $('apiLogBody');
    if (apiLogBody) apiLogBody.addEventListener('click', e => {
      const btn = e.target.closest('[data-log-action="ban-ip"]');
      if (btn) addIPBan(btn.dataset.ip, 'request log');
    });

    window.addEventListener('popstate', () => {
      const tab = new URLSearchParams(location.search).get('tab');
      if (isKnownTab(tab)) switchTab(tab, false);
    });
  }

  // Tabs
  let tabPollTimer = null;
  const TAB_POLL_MS = 5000;
  function stopTabPolling() {
    if (tabPollTimer) { clearInterval(tabPollTimer); tabPollTimer = null; }
  }
  function startTabPolling(fn) {
    stopTabPolling();
    tabPollTimer = setInterval(fn, TAB_POLL_MS);
  }
  // refreshApiKeysIfIdle silently reloads the API-key list for the live-updating card
  // stats, but skips while any modal is open so it can't clobber an in-progress edit.
  function refreshApiKeysIfIdle() {
    if (document.body.classList.contains('modal-open')) return;
    loadApiKeys();
  }

  // ---------------------------------------------------------------------------
  // External providers (OpenAI-/Anthropic-compatible fallback upstreams)
  // ---------------------------------------------------------------------------

  let providersData = [];

  async function loadProviders() {
    try {
      const res = await api('/providers');
      const d = await res.json().catch(() => ({}));
      providersData = Array.isArray(d.providers) ? d.providers : [];
    } catch (e) {
      providersData = [];
    }
    renderProviders();
  }

  function renderProviders() {
    const body = $('providerBody');
    const empty = $('providerEmpty');
    if (!body) return;

    if (!providersData.length) {
      body.innerHTML = '';
      if (empty) empty.classList.remove('hidden');
    } else {
      if (empty) empty.classList.add('hidden');
      const sorted = providersData.slice().sort((a, b) => (a.priority || 0) - (b.priority || 0));
      body.innerHTML = sorted.map(p => {
        const id = escapeAttr(p.id || '');
        const models = (p.models || []).map(m => m.alias).join(', ');
        const statusKey = p.enabled ? 'providers.statusEnabled' : 'providers.statusDisabled';
        const statusCls = p.enabled ? 'badge-success' : 'badge-muted';
        return '<tr>' +
          '<td class="num">' + escapeHtml(String(p.priority || 0)) + '</td>' +
          '<td>' + escapeHtml(p.name || '') + '</td>' +
          '<td class="text-xs font-mono">' + escapeHtml(p.protocol || '') +
            (p.supportsResponses ? ' <span class="muted-text">+responses</span>' : '') + '</td>' +
          '<td class="text-xs font-mono">' + escapeHtml(p.baseUrl || '') + '</td>' +
          '<td class="text-xs">' + escapeHtml(models) + '</td>' +
          '<td class="num">' + escapeHtml(formatNumber(p.requestCount || 0)) + '</td>' +
          '<td class="num">' + escapeHtml(formatNumber(p.totalCredits || 0)) + '</td>' +
          '<td><span class="badge ' + statusCls + '">' + escapeHtml(t(statusKey)) + '</span></td>' +
          '<td>' +
            '<button class="btn btn-outline btn-sm" type="button" data-provider-action="test" data-id="' + id + '">' +
              escapeHtml(t('providers.test')) + '</button> ' +
            '<button class="btn btn-outline btn-sm" type="button" data-provider-action="edit" data-id="' + id + '">' +
              escapeHtml(t('providers.edit')) + '</button> ' +
            '<button class="btn btn-danger btn-sm" type="button" data-provider-action="delete" data-id="' + id + '">' +
              escapeHtml(t('common.remove')) + '</button>' +
          '</td>' +
          '</tr>';
      }).join('');
    }

    renderProviderChain();
  }

  // renderProviderChain shows the effective routing order so an operator can see
  // at a glance what "priority" actually produced, instead of inferring it from
  // two separate tables.
  function renderProviderChain() {
    const el = $('providerChain');
    if (!el) return;

    const steps = [];
    (accountsData || []).filter(a => a.enabled).forEach(a => {
      steps.push({ tier: a.priority || 0, kind: 'account', label: a.nickname || a.email || a.id });
    });
    providersData.filter(p => p.enabled).forEach(p => {
      steps.push({ tier: p.priority || 0, kind: 'provider', label: p.name });
    });

    if (!steps.length) {
      el.innerHTML = '<span class="muted-text">' + escapeHtml(t('providers.chainEmpty')) + '</span>';
      return;
    }

    // Within a tier accounts are tried before providers, matching nextUpstream.
    steps.sort((a, b) => (a.tier - b.tier) || (a.kind === b.kind ? 0 : a.kind === 'account' ? -1 : 1));
    el.innerHTML = steps.map(s => {
      const icon = s.kind === 'provider' ? 'fa-cloud-arrow-up' : 'fa-user';
      return '<span class="badge">' + escapeHtml(String(s.tier)) + ' · ' +
        '<i class="fa-solid ' + icon + '"></i> ' + escapeHtml(s.label) + '</span>';
    }).join(' <span class="muted-text">&rarr;</span> ');
  }

  function providerPricingRow(prefix, pricing, labelKey) {
    const p = pricing || {};
    return '<div class="form-group">' +
      '<label>' + escapeHtml(t(labelKey)) + '</label>' +
      '<div class="input-row">' +
        '<input type="number" step="any" min="0" id="' + prefix + 'Input" value="' + escapeAttr(p.input || 0) +
          '" placeholder="' + escapeAttr(t('providers.priceInput')) + '" />' +
        '<input type="number" step="any" min="0" id="' + prefix + 'Output" value="' + escapeAttr(p.output || 0) +
          '" placeholder="' + escapeAttr(t('providers.priceOutput')) + '" />' +
        '<input type="number" step="any" min="0" id="' + prefix + 'CacheWrite" value="' + escapeAttr(p.cacheWrite || 0) +
          '" placeholder="' + escapeAttr(t('providers.priceCacheWrite')) + '" />' +
        '<input type="number" step="any" min="0" id="' + prefix + 'CacheRead" value="' + escapeAttr(p.cacheRead || 0) +
          '" placeholder="' + escapeAttr(t('providers.priceCacheRead')) + '" />' +
      '</div>' +
      '<span class="muted-text">' + escapeHtml(t('providers.priceHint')) + '</span>' +
      '</div>';
  }

  function openProviderModal(provider) {
    const editing = !!provider;
    const p = provider || {
      enabled: true, protocol: 'anthropic', priority: 0, weight: 1,
      models: [], pricing: {}, headers: {}
    };

    $('providerModalTitle').textContent = t(editing ? 'providers.editTitle' : 'providers.addTitle');
    const modelRows = (p.models || []).map(m => {
      const alias = m.alias || '';
      const name = (m.name && m.name !== alias) ? m.name : alias;
      const base = name ? (alias + ' = ' + name) : alias;
      if (m.pricing) {
        const pr = m.pricing;
        return base + ' @ ' + [pr.input || 0, pr.output || 0, pr.cacheWrite || 0, pr.cacheRead || 0].join('/');
      }
      return base;
    }).join('\n');
    const headerRows = Object.keys(p.headers || {}).map(k => k + ': ' + p.headers[k]).join('\n');

    $('providerModalBody').innerHTML =
      '<input type="hidden" id="pfId" value="' + escapeAttr(p.id || '') + '" />' +
      '<div class="form-group"><label>' + escapeHtml(t('providers.fieldName')) + '</label>' +
        '<input type="text" id="pfName" value="' + escapeAttr(p.name || '') + '" autocomplete="off" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('providers.fieldProtocol')) + '</label>' +
        '<select id="pfProtocol">' +
          '<option value="anthropic"' + (p.protocol === 'anthropic' ? ' selected' : '') + '>anthropic (/v1/messages)</option>' +
          '<option value="openai"' + (p.protocol === 'openai' ? ' selected' : '') + '>openai (/v1/chat/completions)</option>' +
        '</select>' +
        '<span class="muted-text">' + escapeHtml(t('providers.protocolHint')) + '</span></div>' +
      '<div class="form-group" id="pfResponsesRow"><label class="flex items-center gap-2">' +
        '<span class="switch"><input type="checkbox" id="pfSupportsResponses"' + (p.supportsResponses ? ' checked' : '') +
          ' /><span class="slider"></span></span>' +
        '<span>' + escapeHtml(t('providers.fieldSupportsResponses')) + '</span></label></div>' +
      '<div class="form-group"><label class="flex items-center gap-2">' +
        '<span class="switch"><input type="checkbox" id="pfAllowBridge"' + (p.allowProtocolBridge ? ' checked' : '') +
          ' /><span class="slider"></span></span>' +
        '<span>' + escapeHtml(t('providers.fieldAllowBridge')) + '</span></label>' +
        '<span class="muted-text" style="display:block;margin-top:4px">' + escapeHtml(t('providers.allowBridgeHint')) + '</span></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('providers.fieldBaseUrl')) + '</label>' +
        '<input type="text" id="pfBaseUrl" value="' + escapeAttr(p.baseUrl || '') +
          '" placeholder="https://api.example.com/anthropic" autocomplete="off" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('providers.fieldApiKey')) + '</label>' +
        '<input type="password" id="pfApiKey" value="" autocomplete="new-password" placeholder="' +
          escapeAttr(editing && p.hasApiKey ? t('providers.apiKeyKeep') : '') + '" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('providers.fieldModels')) + '</label>' +
        '<textarea id="pfModels" rows="4" placeholder="claude-sonnet-4.5 = glm-4.6">' + escapeHtml(modelRows) + '</textarea>' +
        '<span class="muted-text">' + escapeHtml(t('providers.modelsHint')) + '</span></div>' +
      providerPricingRow('pfPrice', p.pricing, 'providers.fieldPricing') +
      '<div class="form-group"><label>' + escapeHtml(t('providers.fieldPriority')) + '</label>' +
        '<div class="input-row">' +
          '<input type="number" id="pfPriority" min="0" step="1" value="' + escapeAttr(p.priority || 0) + '" />' +
          '<input type="number" id="pfWeight" min="1" step="1" value="' + escapeAttr(p.weight || 1) + '" />' +
        '</div>' +
        '<span class="muted-text">' + escapeHtml(t('providers.priorityHint')) + '</span></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('providers.fieldProxy')) + '</label>' +
        '<input type="text" id="pfProxy" value="' + escapeAttr(p.proxyUrl || '') +
          '" placeholder="socks5://host:port" autocomplete="off" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('providers.fieldHeaders')) + '</label>' +
        '<textarea id="pfHeaders" rows="2" placeholder="X-Custom: value">' + escapeHtml(headerRows) + '</textarea></div>' +
      '<div class="form-group"><label class="flex items-center gap-2">' +
        '<span class="switch"><input type="checkbox" id="pfEnabled"' + (p.enabled ? ' checked' : '') +
          ' /><span class="slider"></span></span>' +
        '<span>' + escapeHtml(t('providers.fieldEnabled')) + '</span></label></div>' +
      '<div class="modal-actions">' +
        '<button class="btn btn-primary" type="button" id="pfSaveBtn">' + escapeHtml(t('common.save')) + '</button>' +
      '</div>';

    const protoSel = $('pfProtocol');
    const syncConditionalRows = () => {
      const isOpenAI = protoSel.value === 'openai';
      const row = $('pfResponsesRow');
      if (row) row.classList.toggle('hidden', !isOpenAI);
    };
    protoSel.addEventListener('change', syncConditionalRows);
    syncConditionalRows();

    $('pfSaveBtn').addEventListener('click', saveProvider);
    openDialog('providerModal');
  }

  // parseProviderModels reads the "alias = upstream-name" textarea. A line with no
  // "=" maps the alias onto itself, which is the common case when the provider
  // already uses the same model ID.
  // parseProviderModelPricing reads the optional "@ in/out/cacheWrite/cacheRead"
  // suffix. Returns null when the suffix is absent, which tells the backend to bill
  // this model at the provider-level default instead.
  function parseProviderModelPricing(raw) {
    const parts = (raw || '').split('/').map(s => s.trim());
    if (!parts.length || parts.every(s => s === '')) return null;
    const num = i => {
      const n = parseFloat(parts[i]);
      return isNaN(n) ? 0 : Math.max(0, n);
    };
    return { input: num(0), output: num(1), cacheWrite: num(2), cacheRead: num(3) };
  }

  function parseProviderModels(raw) {
    return (raw || '').split('\n').map(line => line.trim()).filter(Boolean).map(line => {
      // Split the pricing suffix off first: "@" cannot appear in a model ID, while
      // "=" must still be found within the name half only.
      let pricing = null;
      const at = line.lastIndexOf('@');
      if (at >= 0) {
        pricing = parseProviderModelPricing(line.slice(at + 1));
        line = line.slice(0, at).trim();
      }
      const idx = line.indexOf('=');
      const alias = idx < 0 ? line : line.slice(0, idx).trim();
      const name = idx < 0 ? line : line.slice(idx + 1).trim();
      const out = { alias, name };
      if (pricing) out.pricing = pricing;
      return out;
    });
  }

  function parseProviderHeaders(raw) {
    const out = {};
    (raw || '').split('\n').map(line => line.trim()).filter(Boolean).forEach(line => {
      const idx = line.indexOf(':');
      if (idx <= 0) return;
      out[line.slice(0, idx).trim()] = line.slice(idx + 1).trim();
    });
    return out;
  }

  function numVal(id) {
    const el = $(id);
    const n = el ? parseFloat(el.value) : 0;
    return isNaN(n) ? 0 : n;
  }

  async function saveProvider() {
    const id = $('pfId').value;
    const payload = {
      name: $('pfName').value.trim(),
      enabled: $('pfEnabled').checked,
      protocol: $('pfProtocol').value,
      baseUrl: $('pfBaseUrl').value.trim(),
      apiKey: $('pfApiKey').value,
      proxyUrl: $('pfProxy').value.trim(),
      priority: Math.max(0, Math.round(numVal('pfPriority'))),
      weight: Math.max(1, Math.round(numVal('pfWeight'))),
      supportsResponses: $('pfSupportsResponses').checked,
      allowProtocolBridge: !!$('pfAllowBridge') && $('pfAllowBridge').checked,
      models: parseProviderModels($('pfModels').value),
      headers: parseProviderHeaders($('pfHeaders').value),
      pricing: {
        input: numVal('pfPriceInput'),
        output: numVal('pfPriceOutput'),
        cacheWrite: numVal('pfPriceCacheWrite'),
        cacheRead: numVal('pfPriceCacheRead')
      }
    };

    try {
      const res = await api(id ? '/providers/' + encodeURIComponent(id) : '/providers', {
        method: id ? 'PUT' : 'POST',
        body: JSON.stringify(payload)
      });
      const d = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(d.error || t('common.saveFailed'));
      closeDialog('providerModal');
      toast(t('common.saved'), 'success');
      loadProviders();
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }

  async function deleteProvider(id) {
    const ok = await confirmAction(t('providers.confirmDelete'), {
      title: t('common.remove'),
      confirmText: t('common.remove'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/providers/' + encodeURIComponent(id), { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(d.error || t('common.failed'));
      toast(t('providers.deleted'), 'danger', { icon: 'fa-solid fa-trash' });
      loadProviders();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  async function testProvider(id) {
    toast(t('providers.testing'), 'info');
    try {
      const res = await api('/providers/' + encodeURIComponent(id) + '/test', {
        method: 'POST',
        body: JSON.stringify({})
      });
      const d = await res.json().catch(() => ({}));
      if (d.ok) {
        toast(t('providers.testOk', d.model || '', d.durationMs || 0), 'success');
      } else {
        toast(t('providers.testFailed', d.error || d.status || t('common.failed')), 'error');
      }
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  function bindProviderEvents() {
    const addBtn = $('providerAddBtn');
    if (addBtn) addBtn.addEventListener('click', () => openProviderModal(null));
    const refreshBtn = $('providerRefreshBtn');
    if (refreshBtn) refreshBtn.addEventListener('click', loadProviders);
    const close = $('providerModalClose');
    if (close) close.addEventListener('click', () => closeDialog('providerModal'));
    bindDialogBackdropClose('providerModal', () => closeDialog('providerModal'));

    const body = $('providerBody');
    if (body) body.addEventListener('click', e => {
      const btn = e.target.closest('[data-provider-action]');
      if (!btn) return;
      const id = btn.dataset.id;
      const action = btn.dataset.providerAction;
      if (action === 'delete') deleteProvider(id);
      else if (action === 'test') testProvider(id);
      else if (action === 'edit') {
        const p = providersData.find(x => x.id === id);
        if (p) openProviderModal(p);
      }
    });
  }

  // isKnownTab guards against a hand-edited ?tab= value pointing at a panel that
  // does not exist — switchTab would otherwise throw on a null content element.
  function isKnownTab(tab) {
    if (!tab) return false;
    return !!document.querySelector('#tabBar .tab[data-tab="' + CSS.escape(tab) + '"]');
  }

  function switchTab(tab, pushUrl) {
    qsa('.tab').forEach(el => el.classList.toggle('active', el.dataset.tab === tab));
    qsa('.tab-content').forEach(c => c.classList.add('hidden'));
    $('tab' + tab.charAt(0).toUpperCase() + tab.slice(1)).classList.remove('hidden');
    if (pushUrl !== false) {
      history.replaceState(null, '', location.pathname + '?tab=' + encodeURIComponent(tab));
    }
    if (tab === 'console') openConsole();
    else closeConsole();
    if (tab !== 'apilog') closeApiLogStream();
    stopTabPolling();
    // The API Log tab is push-driven (SSE) rather than polled: the initial fill
    // is one REST call, everything after that arrives on the stream.
    if (tab === 'apilog') { populateApiLogKeyFilter(); loadApiLog(); toggleApiLogLive(); }
    else if (tab === 'settings') { startTabPolling(refreshApiKeysIfIdle); }
    else if (tab === 'providers') { loadProviders(); }
    else if (tab === 'ipbans') { loadIPBans(); }
    else if (tab === 'anomaly') { loadAnomalies(); }
    else if (tab === 'audit') { loadAuditLog(auditActionFilter); }
  }

  // restoreTabFromUrl activates the tab named in ?tab= on first paint so a deep link
  // (or a reload) lands the operator back where they were.
  function restoreTabFromUrl() {
    const urlTab = new URLSearchParams(location.search).get('tab');
    if (isKnownTab(urlTab)) switchTab(urlTab, false);
  }

  // Event wiring
  function bindLoginEvents() {
    $('loginBtn').addEventListener('click', login);
    $('pwdField').addEventListener('keypress', e => { if (e.key === 'Enter') login(); });

    const pwdToggle = $('pwdToggle');
    if (pwdToggle) {
      pwdToggle.addEventListener('click', () => {
        const f = $('pwdField');
        const willShow = f.type === 'password';
        f.type = willShow ? 'text' : 'password';
        pwdToggle.dataset.shown = String(willShow);
        pwdToggle.setAttribute('aria-label', willShow ? t('login.hidePassword') : t('login.showPassword'));
        pwdToggle.innerHTML = willShow
          ? '<i class="fa-solid fa-eye-slash"></i>'
          : '<i class="fa-solid fa-eye"></i>';
      });
    }
  }

  function bindShellEvents() {
    const checkUpdateBtn = $('checkUpdateBtn');
    if (checkUpdateBtn) checkUpdateBtn.addEventListener('click', () => checkUpdate(true));

    document.body.addEventListener('click', e => {
      if (!e.target.closest('.custom-select') && !e.target.closest('.custom-select-content')) closeAllCustomSelects();
      const lb = e.target.closest('.lang-btn');
      if (lb) setLang(lb.dataset.lang);
      const lt = e.target.closest('.lang-toggle');
      if (lt) toggleLang();
    });
    window.addEventListener('resize', positionOpenCustomSelects);
    window.addEventListener('scroll', positionOpenCustomSelects, true);

    $('loginThemeToggle').addEventListener('click', toggleTheme);
    $('mainThemeToggle').addEventListener('click', toggleTheme);
    $('logoutBtn').addEventListener('click', logout);

    qsa('#tabBar .tab').forEach(tab => tab.addEventListener('click', () => switchTab(tab.dataset.tab)));

    const apiLogRefresh = $('apiLogRefreshBtn');
    if (apiLogRefresh) apiLogRefresh.addEventListener('click', loadApiLog);
    const apiLogKeyFilter = $('apiLogKeyFilter');
    if (apiLogKeyFilter) apiLogKeyFilter.addEventListener('change', loadApiLog);
    const logsFilterSelect = $('logsFilterSelect');
    if (logsFilterSelect) logsFilterSelect.addEventListener('change', () => { apiLogFilter = logsFilterSelect.value; renderApiLog(apiLogCache); });
    const logsSearchInput = $('logsSearchInput');
    if (logsSearchInput) logsSearchInput.addEventListener('input', () => { apiLogSearch = logsSearchInput.value.trim(); renderApiLog(apiLogCache); });
    const logsExportJsonBtn = $('logsExportJsonBtn');
    if (logsExportJsonBtn) logsExportJsonBtn.addEventListener('click', () => exportApiLog('json'));
    const logsExportCsvBtn = $('logsExportCsvBtn');
    if (logsExportCsvBtn) logsExportCsvBtn.addEventListener('click', () => exportApiLog('csv'));
    const logsAutoRefresh = $('logsAutoRefresh');
    if (logsAutoRefresh) logsAutoRefresh.addEventListener('change', toggleApiLogLive);
    const logsClearBtn = $('logsClearBtn');
    if (logsClearBtn) logsClearBtn.addEventListener('click', clearApiLog);
    qsa('[data-copy]').forEach(btn => btn.addEventListener('click', async () => {
      const id = btn.dataset.copy;
      const target = $(id);
      if (!target) return;
      try {
        await copyText(target.dataset.rawValue || target.textContent);
        toast(t('common.copied'), 'primary');
      } catch (e) {
        toast(t('common.failed'), 'error');
      }
    }));
  }

  function bindAccountEvents() {
    $('privacyModeToggle').addEventListener('change', e => {
      privacyModeEnabled = e.target.checked;
      localStorage.setItem('privacyMode', privacyModeEnabled);
      renderAccounts();
    });

    $('exportBtn').addEventListener('click', showExportModal);
    $('refreshAllModelsBtn').addEventListener('click', refreshAllModels);
    $('addAccountBtn').addEventListener('click', () => showModal('add'));

    $('selectAllCheckbox').addEventListener('change', e => toggleSelectAll(e.target.checked));
    qsa('[data-batch]').forEach(b => b.addEventListener('click', () => {
      const a = b.dataset.batch;
      if (a === 'refreshModels') batchRefreshModels();
      else if (a === 'delete') batchDelete();
      else batchAction(a);
    }));

    $('filterSearch').addEventListener('input', onFilterChange);
    $('filterStatusSelect').addEventListener('change', onFilterChange);

    $('accountsList').addEventListener('click', e => {
      const cb = e.target.closest('.account-checkbox');
      if (cb) {
        toggleSelectAccount(cb.dataset.id);
        const card = cb.closest('.account-card');
        if (card) card.classList.toggle('selected', cb.checked);
        return;
      }
      const btn = e.target.closest('button[data-action]');
      if (!btn) return;
      const id = btn.dataset.id;
      const action = btn.dataset.action;
      if (action === 'refresh') refreshAccount(id, btn.closest('.account-card'));
      else if (action === 'detail') showDetail(id);
      else if (action === 'copyJSON') copyAccountJSON(id, btn);
      else if (action === 'toggle') toggleAccount(id, btn.dataset.enabled === 'true');
      else if (action === 'test') testAccount(id);
      else if (action === 'delete') deleteAccount(id);
    });
  }

  function bindSettingsEvents() {
    $('saveRequireApiKeyBtn').addEventListener('click', saveRequireApiKey);
    $('saveOverUsageBtn').addEventListener('click', saveOverUsageConfig);
    $('saveThinkingBtn').addEventListener('click', saveThinkingConfig);
    $('saveEndpointBtn').addEventListener('click', saveEndpointConfig);
    $('changePasswordBtn').addEventListener('click', changePassword);
    $('proxyType').addEventListener('change', onProxyTypeChange);
    $('saveProxyBtn').addEventListener('click', saveProxyConfig);
    const savePbu = $('savePublicBaseURLBtn');
    if (savePbu) savePbu.addEventListener('click', savePublicBaseURL);
    const saveLn = $('saveLimitNoticeBtn');
    if (saveLn) saveLn.addEventListener('click', saveLimitNotice);
    const saveFm = $('saveForceModelBtn');
    if (saveFm) saveFm.addEventListener('click', saveForceModel);
    const saveIm = $('saveIdentityModelBtn');
    if (saveIm) saveIm.addEventListener('click', saveIdentityModel);
    const idSel = $('identityModel');
    if (idSel) idSel.addEventListener('change', updateIdentityModelWarning);
    $('proxyImportBtn').addEventListener('click', importProxies);
    $('proxyPoolList').addEventListener('click', e => {
      const btn = e.target.closest('button[data-pool-action]');
      if (!btn) return;
      const url = btn.dataset.url;
      if (btn.dataset.poolAction === 'remove') removeProxyFromPool(url);
      else if (btn.dataset.poolAction === 'toggle') toggleProxyPool(url, btn.dataset.disabled === '1');
    });
    $('resetStatsBtn').addEventListener('click', resetStats);
    bindApiKeyEvents();
  }

  function bindPromptFilterEvents() {
    $('savePromptFilterBtn').addEventListener('click', savePromptFilter);
    $('addRuleRegexBtn').addEventListener('click', () => addPromptRule('regex'));
    $('addRuleContainsBtn').addEventListener('click', () => addPromptRule('lines-containing'));

    $('promptFilterRules').addEventListener('input', e => {
      const idx = e.target.dataset.ruleIdx;
      const field = e.target.dataset.ruleField;
      if (idx != null && field) promptRules[idx][field] = e.target.value;
    });
    $('promptFilterRules').addEventListener('change', e => {
      if (e.target.dataset.ruleToggle != null) {
        promptRules[e.target.dataset.ruleToggle].enabled = e.target.checked;
        renderPromptRules();
      }
    });
    $('promptFilterRules').addEventListener('click', e => {
      const rm = e.target.closest('[data-rule-remove]');
      if (rm) { promptRules.splice(parseInt(rm.dataset.ruleRemove, 10), 1); renderPromptRules(); }
    });
  }

  function bindModalEvents() {
    $('addModalClose').addEventListener('click', closeModal);
    $('detailModalClose').addEventListener('click', closeDetailModal);
    $('exportModalClose').addEventListener('click', closeExportModal);
    $('testModalClose').addEventListener('click', closeTestModal);
    $('updateModalClose').addEventListener('click', closeUpdateModal);
    [
      ['addModal', closeModal],
      ['detailModal', closeDetailModal],
      ['exportModal', closeExportModal],
      ['testModal', closeTestModal],
      ['updateModal', closeUpdateModal],
      ['confirmModal', () => closeConfirm(false)],
    ].forEach(([id, fn]) => bindDialogBackdropClose(id, fn));

    $('modalBody').addEventListener('click', e => {
      const m = e.target.closest('[data-method]');
      if (m) { showModal(m.dataset.method); return; }
      const g = e.target.closest('[data-modal-goto]');
      if (g) { showModal(g.dataset.modalGoto); return; }
      if (e.target.dataset.closeAdd) closeModal();
    });
  }

  function bindDetailEvents() {
    $('detailBody').addEventListener('click', e => {
      if (e.target.id === 'generateMachineIdBtn') { generateMachineId(); return; }
      const b = e.target.closest('[data-detail-action]');
      if (!b) return;
      const id = b.dataset.id;
      const a = b.dataset.detailAction;
      if (a === 'saveMachineId') saveMachineId(id);
      else if (a === 'saveWeight') saveWeight(id);
      else if (a === 'savePriority') savePriority(id);
      else if (a === 'toggleOverage') toggleOverageSwitch(id, b);
      else if (a === 'refreshOverage') refreshAccountOverage(id);
      else if (a === 'saveProxyURL') saveProxyURL(id);
      else if (a === 'loadModels') loadModels(id);
      else if (a === 'refreshModels') refreshAccountModels(id);
    });
  }

  function bindTestEvents() {
    $('testBody').addEventListener('click', e => {
      if (e.target.id === 'testLogClear') { clearTestLog(); return; }
      if (e.target.id === 'testModalCancelBtn') { closeTestModal(); return; }
      const run = e.target.closest('#testRunBtn');
      if (run) runTestAccount(run.dataset.id, getTestModelValue());
    });
    $('testBody').addEventListener('keydown', e => {
      if (e.key !== 'Enter') return;
      if (!e.target.closest('#testModelChoice')) return;
      const run = $('testRunBtn');
      if (!run || run.disabled) return;
      e.preventDefault();
      runTestAccount(run.dataset.id, getTestModelValue());
    });
  }

  function wireEvents() {
    bindLoginEvents();
    bindShellEvents();
    bindAccountEvents();
    bindSettingsEvents();
    bindPromptFilterEvents();
    bindModalEvents();
    bindDetailEvents();
    bindTestEvents();
    bindConsoleEvents();
    bindSecurityEvents();
    bindProviderEvents();
  }

  // Init
  async function init() {
    initTheme();
    await loadLocale(currentLang);
    if (currentLang !== 'zh') await loadLocale('zh');
    applyTranslations();
    initCustomSelectObserver();
    initPrivacyMode();
    initRememberMe();
    const yr = $('footerYear');
    if (yr) yr.textContent = new Date().getFullYear();
    wireEvents();
    tryAutoLogin();
    setInterval(() => {
      if (!$('mainPage').classList.contains('hidden')) loadStats();
    }, 10000);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
