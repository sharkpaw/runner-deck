// A session can end while the page is open: send the user to the login page.
(function () {
  var realFetch = window.fetch;
  window.fetch = function () {
    return realFetch.apply(this, arguments).then(function (res) {
      if (res.status === 401) location.href = '/login';
      return res;
    });
  };
})();
// Runner Deck management UI.
//
// This file was split out of the <script> block in templates/index.html. Previously CSS, HTML, and JS
// were crammed into one thousand-plus-line Go template, so editors had no syntax highlighting and browsers couldn't cache it.
// Now it no longer goes through the Go template engine, so placeholders like {{name}} can be written as literals,
// instead of being escaped as {{"{{"}}name{{"}}"}} to dodge the template delimiters.
//
// The i18n dictionary is injected into window.__I18N beforehand by a small inline script in the page.
function t(key) { return (window.__I18N && window.__I18N[key]) || key; }

// ===== Toast =====
// Replaces the native alert: it blocks the whole page, and its styling clashes with this dark UI.
const toastWrap = document.getElementById('toastWrap');
function dismissToast(el) { if (el && el.parentNode) el.parentNode.removeChild(el); }
// actions: [{ label, onClick }]. Toasts with actions do not auto-dismiss —
// they wait for the user to click, since auto-dismissing would remove the way out.
function showToast(message, type, actions) {
  const el = document.createElement('div');
  el.className = 'toast ' + (type || '');
  const msg = document.createElement('div');
  msg.className = 'toast-msg';
  // API-returned text embeds the runner names users entered, so it must go through textContent
  msg.textContent = message;
  el.appendChild(msg);
  const bar = document.createElement('div');
  bar.className = 'toast-actions';
  (actions || []).forEach(function(a) {
    const b = document.createElement('button');
    b.type = 'button';
    // Don't apply .btn-view/.btn-del here: those class names are also the row-action behavior selectors
    b.className = 'btn-neutral';
    b.textContent = a.label;
    b.addEventListener('click', function() { dismissToast(el); a.onClick(); });
    bar.appendChild(b);
  });
  const close = document.createElement('button');
  close.type = 'button';
  close.className = 'btn-neutral';
  close.textContent = t('toast.dismiss');
  close.addEventListener('click', function() { dismissToast(el); });
  bar.appendChild(close);
  el.appendChild(bar);
  toastWrap.appendChild(el);
  if (!actions || !actions.length) {
    setTimeout(function() { dismissToast(el); }, type === 'err' ? 8000 : 4000);
  }
  return el;
}

// ===== Confirmation dialog =====
// Replaces the native confirm. Besides blocking the page, it can be permanently disabled by the browser's "prevent this page from creating additional dialogs" option
// — after which confirmed actions like delete and rebuild silently do nothing.
const confirmModal = document.getElementById('confirmModal');
const confirmTextEl = document.getElementById('confirmText');
const confirmOkBtn = document.getElementById('confirmOkBtn');
const confirmCancelBtn = document.getElementById('confirmCancelBtn');
let confirmSettle = null;
function settleConfirm(v) {
  if (!confirmSettle) return;
  const fn = confirmSettle;
  confirmSettle = null;
  confirmModal.classList.remove('show');
  // Return focus to the element that opened it; it may have been replaced by an auto-refresh, so first check that it is still in the document
  if (confirmLastFocused && document.contains(confirmLastFocused)) confirmLastFocused.focus();
  confirmLastFocused = null;
  fn(v);
}
let confirmLastFocused = null;
function confirmDialog(message) {
  // If a new confirm opens before the previous one settles, cancel the old one first so its Promise doesn't hang forever
  settleConfirm(false);
  confirmLastFocused = document.activeElement;
  confirmTextEl.textContent = message;
  confirmModal.classList.add('show');
  confirmOkBtn.focus();
  return new Promise(function(resolve) { confirmSettle = resolve; });
}
// The confirm dialog is also a modal and must trap focus too — it stacks on top of the view dialog, and without trapping, Tab would move into
// the layer underneath. trapTab comes from the modal focus management, so reuse it here.
confirmModal.addEventListener('keydown', function(e) { trapTab(e, confirmModal); });
confirmOkBtn.addEventListener('click', function() { settleConfirm(true); });
confirmCancelBtn.addEventListener('click', function() { settleConfirm(false); });
confirmModal.addEventListener('click', function(e) { if (e.target === confirmModal) settleConfirm(false); });

// ===== Button busy state =====
// Starting a runner can take seconds to tens of seconds; previously clicking caused no visible change, so users inevitably click repeatedly,
// So the same action gets sent twice. While a request is in flight, also disable the buttons in the same row (or the modal footer).
function setRowBusy(btn, busy) {
  if (!btn) return;
  const scope = btn.closest('tr') || btn.closest('.modal-footer');
  const group = scope ? Array.prototype.slice.call(scope.querySelectorAll('button')) : [btn];
  group.forEach(function(b) {
    // Keep the close button always enabled, or users get stuck in the modal when requests are slow
    if (b !== btn && b.classList.contains('modal-close')) return;
    b.disabled = busy;
  });
  if (busy) {
    if (btn.dataset.idleText === undefined) btn.dataset.idleText = btn.textContent;
    btn.textContent = t('btn.working');
  } else if (btn.dataset.idleText !== undefined) {
    btn.textContent = btn.dataset.idleText;
    delete btn.dataset.idleText;
  }
}
document.getElementById('langSelect').addEventListener('change', function() {
  var lang = this.value;
  document.cookie = 'lang=' + encodeURIComponent(lang) + ';path=/;max-age=31536000';
  location.reload();
});
// Parse GitHub config.sh command and fill add-runner form
function sanitizeRunnerName(s) {
  if (!s || typeof s !== 'string') return '';
  return s.replace(/\.\./g, '').replace(/[/\\]/g, '-').trim() || 'runner';
}

function parseGitHubConfigCommand(text) {
  if (!text || typeof text !== 'string') return { ok: false, message: t('parse.please_enter_command') };
  const trimmed = text.trim();
  const urlMatch = trimmed.match(/\-\-url\s+["']?([^\s"']+)["']?/);
  const tokenMatch = trimmed.match(/\-\-token\s+["']?([^\s"']+)["']?/);
  if (!urlMatch) return { ok: false, message: t('parse.no_url') };
  const url = urlMatch[1].trim();
  let parsedUrl;
  try {
    parsedUrl = new URL(url);
  } catch (e) {
    return { ok: false, message: t('parse.invalid_url') };
  }
  const pathParts = parsedUrl.pathname.replace(/^\/+|\/+$/g, '').split('/').filter(Boolean);
  let targetType = 'org';
  let target = '';
  let suggestedName = '';
  if (pathParts.length >= 2) {
    targetType = 'repo';
    target = pathParts[0] + '/' + pathParts[1];
    suggestedName = sanitizeRunnerName(pathParts[1]);
  } else if (pathParts.length === 1) {
    target = pathParts[0];
    suggestedName = sanitizeRunnerName(pathParts[0]);
  } else {
    return { ok: false, message: t('parse.cannot_parse') };
  }
  const token = tokenMatch ? tokenMatch[1].trim() : '';
  return {
    ok: true,
    target_type: targetType,
    target: target,
    registration_token: token,
    suggested_name: suggestedName || 'runner'
  };
}

// Runners of one repository differ only by number, so the name is proposed as <repo>-<next free number>.
// A name the user typed is never replaced.
var autoNamed = false;
var addedNames = {};
function nextRunnerName(base) {
  var taken = Object.assign({}, addedNames);
  document.querySelectorAll('#runnerRowsBody tr[data-name]').forEach(function(row) {
    taken[row.dataset.name] = true;
  });
  var n = 1;
  while (taken[base + '-' + n]) n++;
  return base + '-' + n;
}
function canAutoName() {
  return autoNamed || !document.getElementById('addFormName').value;
}
function setAutoName(base) {
  document.getElementById('addFormName').value = nextRunnerName(base);
  autoNamed = true;
}
document.getElementById('addFormTarget').addEventListener('input', function() {
  var target = this.value.trim();
  document.getElementById('addFormTargetType').value = target.indexOf('/') > 0 ? 'repo' : 'org';
  var base = sanitizeRunnerName(target.split('/').filter(Boolean).pop() || '');
  if (!target || !canAutoName()) return;
  setAutoName(base);
  schedulePrecheck();
});
document.getElementById('addFormName').addEventListener('input', function() {
  autoNamed = false;
});

document.getElementById('parseCommandBtn').addEventListener('click', function() {
  const input = document.getElementById('githubCommandInput');
  const msgEl = document.getElementById('parseMsg');
  const result = parseGitHubConfigCommand(input.value);
  msgEl.textContent = '';
  msgEl.className = 'parse-msg';
  if (!result.ok) {
    msgEl.className = 'parse-msg err';
    msgEl.textContent = result.message;
    return;
  }
  document.getElementById('addFormTargetType').value = result.target_type;
  document.getElementById('addFormTarget').value = result.target;
  document.getElementById('addFormToken').value = result.registration_token || '';
  if (result.suggested_name && canAutoName()) {
    setAutoName(result.suggested_name);
    document.getElementById('addFormPath').value = '';
    // Names auto-filled from the repo name are the most likely to collide with an existing runner, so check once right after filling
    schedulePrecheck();
  }
  msgEl.className = 'parse-msg ok';
  var filled = t('parse.filled').replace('{{target_type}}', result.target_type).replace('{{target}}', result.target).replace('{{token_part}}', result.registration_token ? t('parse.filled_token') : '');
  msgEl.textContent = filled;
});

// ===== Pre-add conflict check =====
// Name collisions take several forms: a same-named entry already in config, a clash with the container name after normalization, an occupied install directory,
// a leftover directory from a previous registration on disk, or a same-named container still attached to the host. Discovering this only at submit time is too late,
// so we ask the server once while typing (/api/runner-precheck) and confirm again before submitting.
var PH_OPEN = '{{', PH_CLOSE = '}}';
function fillVars(str, vars) {
  Object.keys(vars).forEach(function(k) {
    str = str.split(PH_OPEN + k + PH_CLOSE).join(vars[k] == null ? '' : vars[k]);
  });
  return str;
}
var PRECHECK_KEYS = {
  name_taken: 'precheck.name_taken',
  container_name: 'precheck.container_name',
  install_dir: 'precheck.install_dir',
  dir_registered: 'precheck.dir_registered',
  dir_adopt: 'precheck.dir_adopt',
  dir_exists: 'precheck.dir_exists',
  container_exists: 'precheck.container_exists'
};
function conflictText(conflict, resp) {
  var key = PRECHECK_KEYS[conflict.type];
  if (!key) return conflict.message || '';
  return fillVars(t(key), {
    detail: conflict.detail || '',
    dir: (resp && resp.install_dir) || conflict.detail || '',
    container: (resp && resp.container_name) || conflict.detail || '',
    status: conflict.status || ''
  });
}
var nameCheckEl = document.getElementById('nameCheckMsg');
var nameInput = document.getElementById('addFormName');
var pathInput = document.getElementById('addFormPath');
var tokenInput = document.getElementById('addFormToken');
var precheckSeq = 0;

function clearNameCheck() {
  nameCheckEl.className = 'check-msg';
  nameCheckEl.textContent = '';
}
function renderPrecheck(resp) {
  nameCheckEl.textContent = '';
  if (!resp) { clearNameCheck(); return; }
  var worst = 'ok';
  (resp.conflicts || []).forEach(function(c) {
    if (c.level === 'error') worst = 'err';
    else if (c.level === 'warn' && worst !== 'err') worst = 'warn';
  });
  nameCheckEl.className = 'check-msg ' + worst;
  if (!resp.conflicts || resp.conflicts.length === 0) {
    var okLine = document.createElement('span');
    okLine.className = 'check-line';
    okLine.textContent = t('precheck.ok');
    nameCheckEl.appendChild(okLine);
    return;
  }
  resp.conflicts.forEach(function(c) {
    var line = document.createElement('span');
    line.className = 'check-line';
    line.textContent = conflictText(c, resp);
    if (c.fix_command) {
      line.appendChild(document.createTextNode(' '));
      var code = document.createElement('code');
      code.textContent = c.fix_command;
      line.appendChild(code);
    }
    nameCheckEl.appendChild(line);
  });
  if (resp.suggested_name) {
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'btn-suggest';
    btn.textContent = fillVars(t('precheck.use_suggested'), { name: resp.suggested_name });
    btn.addEventListener('click', function() {
      nameInput.value = resp.suggested_name;
      runPrecheck(nameInput.value, pathInput.value.trim(), false).then(renderPrecheck);
    });
    nameCheckEl.appendChild(btn);
  }
}
// A null return means the check didn't complete (e.g. the endpoint is unavailable); don't block submission in that case and let the server catch it.
// guardStale: while checking as the user types, a stale result should be discarded when a later request returns first;
// but explicit actions like clicking a suggested name or submitting must get a result and must not be dropped by subsequent input.
async function runPrecheck(name, path, guardStale) {
  if (!name) return null;
  var seq = ++precheckSeq;
  try {
    // has_token only passes whether a token was filled in, not the token itself: a runner already registered in the directory,
    // re-registering with a token gets rejected by config.sh (error); without a token it's just a takeover (a hint is enough)
    var url = '/api/runner-precheck?name=' + encodeURIComponent(name) +
      (path ? '&path=' + encodeURIComponent(path) : '') +
      (tokenInput && tokenInput.value.trim() ? '&has_token=1' : '');
    var r = await fetch(url);
    if (!r.ok) return null;
    var data = await r.json();
    if (guardStale && seq !== precheckSeq) return null;
    return data;
  } catch (e) {
    return null;
  }
}
var precheckTimer = null;
function schedulePrecheck() {
  if (precheckTimer) clearTimeout(precheckTimer);
  var name = nameInput.value.trim();
  if (!name) { clearNameCheck(); return; }
  precheckTimer = setTimeout(function() {
    nameCheckEl.className = 'check-msg';
    nameCheckEl.textContent = t('precheck.checking');
    runPrecheck(name, pathInput.value.trim(), true).then(function(resp) {
      if (resp) renderPrecheck(resp); else clearNameCheck();
    });
  }, 400);
}
nameInput.addEventListener('input', schedulePrecheck);
pathInput.addEventListener('input', schedulePrecheck);
tokenInput.addEventListener('input', schedulePrecheck);

document.getElementById('addForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  const fd = new FormData(e.target);
  const body = {};
  fd.forEach((v, k) => { if (v) body[k] = v; });
  if (body.labelsStr) {
    body.labels = body.labelsStr.split(',').map(s => s.trim()).filter(Boolean);
    delete body.labelsStr;
  }
  const msgEl = document.getElementById('addMsg');
  const msgWrap = document.getElementById('addMsgWrap');
  const submitBtn = e.target.querySelector('button[type="submit"]');
  msgWrap.style.display = 'none';
  msgEl.innerHTML = t('msg.adding');
  msgEl.className = 'msg';
  msgWrap.style.display = 'block';
  if (submitBtn) submitBtn.disabled = true;
  // The hint on the input may be stale (a same-named runner was just added elsewhere), so confirm again before submitting
  const pre = await runPrecheck(body.name, body.path || '', false);
  if (pre && !pre.available) {
    renderPrecheck(pre);
    msgEl.className = 'msg err';
    msgEl.textContent = t('precheck.blocked');
    msgWrap.style.display = 'block';
    if (submitBtn) submitBtn.disabled = false;
    return;
  }
  try {
    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), 5 * 60 * 1000);
    const r = await fetch('/api/runners', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal: controller.signal
    });
    clearTimeout(timeoutId);
    const data = await r.json().catch(() => ({}));
    if (!r.ok) {
      // 409: the server detected a conflict; render the conflict and suggested names below the name input, and also offer a "Use suggested name" action
      if (r.status === 409 && data.conflicts) {
        renderPrecheck(data);
        msgEl.className = 'msg err';
        msgEl.textContent = t('precheck.blocked');
      } else {
        msgEl.className = 'msg err';
        msgEl.textContent = data.message || r.statusText || t('msg.request_failed');
      }
      if (submitBtn) submitBtn.disabled = false;
      msgWrap.style.display = 'block';
      return;
    }
    msgEl.className = 'msg ok';
    // Both message and install_dir embed the user-entered runner name, so they must be escaped:
    // Name validation only blocks .. / \, so names like `<img src=x onerror=...>` get through
    msgEl.innerHTML = escapeHtml(data.message || '') +
      (data.install_dir ? '<br><span class="path">' + escapeHtml(data.install_dir) + '</span>' : '');
    if (data.output) msgEl.innerHTML += '<pre style="margin-top:8px;font-size:12px">' + escapeHtml(data.output) + '</pre>';
    if (data.queued) {
      msgEl.innerHTML += '<br><span style="font-size:12px;color:var(--muted)">' + t('msg.reload_in_5s') + '</span>';
      setTimeout(function() { location.reload(); }, 5000);
    }
    addedNames[String(new FormData(e.target).get('name'))] = true;
    autoNamed = false;
    e.target.reset();
    clearNameCheck();
    if (submitBtn) submitBtn.disabled = false;
    msgWrap.style.display = 'block';
  } catch (err) {
    if (submitBtn) submitBtn.disabled = false;
    msgEl.className = 'msg err';
    msgEl.textContent = err.name === 'AbortError' ? t('msg.timeout_refresh') : (err.message || t('msg.request_failed'));
    msgWrap.style.display = 'block';
  }
});
document.getElementById('addMsgClose').addEventListener('click', function() {
  document.getElementById('addMsgWrap').style.display = 'none';
  location.reload();
});
// Printing RFC3339 as-is, like "last checked at 2026-09-21T09:00:00Z", forces the reader to mentally convert it
// into "how long ago" to be useful, yet this column's real question is whether the check is stuck.
// Leave relative time to Intl.RelativeTimeFormat, which handles plurals and per-language phrasing,
// so there's no need to maintain six sets of copy for this.
function relativeTime(d, lang) {
  if (typeof Intl === 'undefined' || !Intl.RelativeTimeFormat) return '';
  try {
    const rtf = new Intl.RelativeTimeFormat(lang || 'en', { numeric: 'auto' });
    const diff = (d.getTime() - Date.now()) / 1000;
    const units = [['year', 31536000], ['month', 2592000], ['day', 86400], ['hour', 3600], ['minute', 60]];
    for (let i = 0; i < units.length; i++) {
      if (Math.abs(diff) >= units[i][1]) return rtf.format(Math.round(diff / units[i][1]), units[i][0]);
    }
    return rtf.format(Math.round(diff), 'second');
  } catch (_) {
    return '';
  }
}
// Relative time goes on the main line, exact timestamp in small text below: the former answers "is it stuck?",
// the latter is for matching times against logs. If it can't be parsed, show it as-is; don't lose the information.
function renderTimestamp(el, iso) {
  el.textContent = '';
  if (!iso) { el.textContent = '—'; return; }
  const lang = document.documentElement.lang || 'en';
  const d = new Date(iso);
  const rel = isNaN(d.getTime()) ? '' : relativeTime(d, lang);
  if (!rel) { el.textContent = iso; return; }
  el.appendChild(document.createTextNode(rel));
  const abs = document.createElement('span');
  abs.className = 'ts-abs';
  // Follow the page language, not the browser locale: the UI has already switched to Chinese,
  // showing this line in the system locale would look jarring
  abs.textContent = d.toLocaleString(lang);
  el.appendChild(abs);
}
// On a healthy Runner these 5 lines are all "—", taking up over half of the three-column grid
const PROBE_ROW_IDS = ['vProbeErrorTypeRow', 'vProbeSuggestionRow', 'vProbeCheckCmdRow', 'vProbeFixCmdRow', 'vProbeErrorRow'];
function setProbeRowsVisible(on) {
  PROBE_ROW_IDS.forEach(function(id) {
    const row = document.getElementById(id);
    if (row) row.style.display = on ? '' : 'none';
  });
}
// Values longer than this wrap into a thin column when squeezed into a third of a column; spanning the full row is better
const WIDE_ROW_CHARS = 40;
function setRowWide(rowId, text) {
  const row = document.getElementById(rowId);
  if (row) row.classList.toggle('span-all', (text || '').length > WIDE_ROW_CHARS);
}

function escapeHtml(s) {
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}
function resolveProbeSuggestion(data) {
  if (data && data.probe && data.probe.suggestion) return data.probe.suggestion;
  return t('probe.default_suggestion');
}
function resolveProbeError(data) {
  if (data && data.probe && data.probe.error) return data.probe.error;
  return '';
}
function resolveProbeCheckCommand(data) {
  if (data && data.probe && data.probe.check_command) return data.probe.check_command;
  return 'docker compose ps && docker logs --tail=200 runner-manager';
}
function resolveProbeFixCommand(data) {
  if (data && data.probe && data.probe.fix_command) return data.probe.fix_command;
  return 'docker compose up -d --force-recreate';
}
function resolveProbeType(data) {
  if (data && data.probe && data.probe.type) return data.probe.type;
  return 'unknown';
}
// ===== Keyboard accessibility of the dialog =====
// Previously the dialog had no role/aria-modal, and focus was unconstrained: Tab would run through to the table and
// form behind the dialog, and screen readers would read background content mixed with the dialog; after closing, focus fell back to <body>, so keyboard users had to Tab from
// the top of the page back to where they were.
const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
let lastFocused = null;
function focusablesIn(root) {
  // offsetParent being null means currently not visible (many buttons in the dialog are display:none based on state),
  // counting them would make Tab stop on invisible elements
  return Array.prototype.filter.call(root.querySelectorAll(FOCUSABLE), function(el) {
    return el.offsetParent !== null;
  });
}
function trapTab(e, root) {
  if (e.key !== 'Tab') return;
  const items = focusablesIn(root);
  if (!items.length) return;
  const first = items[0], last = items[items.length - 1];
  if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
  else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
}
const modal = document.getElementById('runnerModal');
const modalTitle = document.getElementById('modalTitle');
const modalView = document.getElementById('modalView');
const modalEditForm = document.getElementById('modalEditForm');
const modalEditBtn = document.getElementById('modalEditBtn');
const modalSaveBtn = document.getElementById('modalSaveBtn');
const modalMsg = document.getElementById('modalMsg');
const revealFixBtn = document.getElementById('vRevealFixCmdBtn');
const copyCheckBtn = document.getElementById('vCopyCheckCmdBtn');
const copyFixBtn = document.getElementById('vCopyFixCmdBtn');
const probeFixCmdEl = document.getElementById('vProbeFixCommand');
const probeCheckCmdEl = document.getElementById('vProbeCheckCommand');
let currentProbeCheckCommand = '';
let currentProbeFixCommand = '';
let probeFixRevealed = false;

async function copyCommandText(text) {
  if (!text) return false;
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch (_) {
    // ignore and fallback below
  }
  // Fallback: try execCommand in environments that don't support or deny the clipboard API
  try {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '-9999px';
    document.body.appendChild(ta);
    ta.select();
    ta.setSelectionRange(0, ta.value.length);
    const ok = document.execCommand && document.execCommand('copy');
    document.body.removeChild(ta);
    return !!ok;
  } catch (_) {
    return false;
  }
}

function openModal(mode, name) {
  // Record this before showing: once the dialog takes focus, there's no way to know where the user was
  if (!modal.classList.contains('show')) lastFocused = document.activeElement;
  modalMsg.style.display = 'none';
  if (mode === 'view') {
    modalTitle.textContent = t('modal.view_title');
    // Don't write 'block': an inline style would override display:grid in the stylesheet, and the three-column layout would be lost.
    // Setting it empty falls back to the stylesheet, so the column count can later be changed in one place in CSS.
    modalView.style.display = '';
    modalEditForm.style.display = 'none';
    modalEditBtn.style.display = 'inline-block';
    modalSaveBtn.style.display = 'none';
    document.getElementById('modalStartStopSpan').style.display = 'none';
    fetch('/api/runners/' + encodeURIComponent(name))
      .then(r => r.ok ? r.json() : Promise.reject(r))
      .then(data => {
        document.getElementById('vName').textContent = data.name || '';
        document.getElementById('vPath').textContent = data.path || data.name || '';
        document.getElementById('vTargetType').textContent = data.target_type || '';
        document.getElementById('vTarget').textContent = data.target || '';
        document.getElementById('vLabels').textContent = Array.isArray(data.labels) ? data.labels.join(', ') : (data.labels || '');
        document.getElementById('vInstallDir').textContent = data.install_dir || '';
        var driftRow = document.getElementById('vDriftRow');
        document.getElementById('vDrift').textContent = data.container_drift || '';
        driftRow.style.display = data.container_drift ? '' : 'none';
        setRowWide('vDriftRow', data.container_drift);
        var jdbRow = document.getElementById('vJobDockerBackendRow');
        var jdbEl = document.getElementById('vJobDockerBackend');
        if (data.job_docker_backend) {
          jdbRow.style.display = '';
          jdbEl.textContent = data.job_docker_backend;
        } else {
          jdbRow.style.display = 'none';
        }
        document.getElementById('vStatus').innerHTML = '<span class="badge ' + escapeHtml(data.status || '') + '">' + escapeHtml(data.status || '') + '</span>';
        document.getElementById('vRunning').innerHTML = data.running ? ' <span class="badge running">' + t('badge.running') + '</span>' : '';
        document.getElementById('vBusy').innerHTML = data.github_busy ? ' <span class="badge busy has-tip" tabindex="0" role="note">' + escapeHtml(t('badge.busy')) + '<span class="tip">' + escapeHtml(t('badge.busy_title')) + '</span></span>' : '';
        const probeError = resolveProbeError(data);
        setProbeRowsVisible(!!probeError);
        document.getElementById('vProbeErrorType').textContent = probeError ? resolveProbeType(data) : '—';
        document.getElementById('vProbeSuggestion').textContent = probeError ? resolveProbeSuggestion(data) : '—';
        currentProbeCheckCommand = probeError ? resolveProbeCheckCommand(data) : '';
        probeCheckCmdEl.textContent = currentProbeCheckCommand || '—';
        copyCheckBtn.style.display = currentProbeCheckCommand ? 'inline-block' : 'none';
        currentProbeFixCommand = probeError ? resolveProbeFixCommand(data) : '';
        probeFixRevealed = false;
        probeFixCmdEl.textContent = probeError ? t('probe.fix_hidden_hint') : '—';
        revealFixBtn.style.display = probeError ? 'inline-block' : 'none';
        copyFixBtn.style.display = 'none';
        document.getElementById('vProbeError').textContent = probeError || '—';
        document.getElementById('vRegistrationMessage').textContent = data.registration_message || '—';
        setRowWide('vProbeSuggestionRow', probeError ? resolveProbeSuggestion(data) : '');
        setRowWide('vProbeCheckCmdRow', currentProbeCheckCommand);
        setRowWide('vProbeFixCmdRow', probeError ? resolveProbeFixCommand(data) : '');
        setRowWide('vProbeErrorRow', probeError);
        setRowWide('vRegistrationMessageRow', data.registration_message);
        renderTimestamp(document.getElementById('vRegistrationCheckedAt'), data.registration_checked_at);
        var gh = data.registered_on_github;
        var ghEl = document.getElementById('vRegisteredOnGitHub');
        var ghLink = document.getElementById('vGitHubLink');
        // The address goes through the .href property instead of being concatenated into innerHTML: escapeHtml handles the HTML context,
        // it can't block a javascript: protocol in href. The address itself is also built on the Go side from the hardcoded
        // https://github.com prefix, leaving neither end any chance to rewrite the protocol.
        ghLink.style.display = 'none';
        ghLink.removeAttribute('href');
        if (gh === true && data.github_url) {
          ghLink.href = data.github_url;
          ghLink.textContent = t('modal.gh_yes');
          ghLink.style.display = '';
          ghEl.textContent = '';
        }
        else if (gh === true) ghEl.innerHTML = '<span class="github-yes">' + t('modal.gh_yes') + '</span>';
        else if (gh === false) ghEl.innerHTML = '<span class="github-no">' + t('modal.gh_no') + '</span>';
        // gh being null: it was checked but no answer was found, which must be distinguished from "never checked", otherwise an expired token looks like it was never configured
        else if (data.github_check_error) ghEl.textContent = t('modal.gh_failed') + ': ' + data.github_check_error;
        else ghEl.textContent = t('modal.gh_unchecked');
        // Busy is also tri-state: if the API has no such field, it means "unknown", not "idle"
        var busyEl = document.getElementById('vGitHubBusy');
        if (data.github_busy === true) busyEl.textContent = t('modal.gh_busy');
        else if (data.github_busy === false) busyEl.textContent = t('modal.gh_idle');
        else busyEl.textContent = t('modal.gh_busy_unknown');
        // Visibility is tri-state too. Say "unknown" when it is unknown: don't report "couldn't check" as "private",
        // precisely in the case where the user most needs a warning (wrong target name, token can't see it), giving a false reassurance.
        var visEl = document.getElementById('vRepoVisibility');
        if (data.github_public === true) visEl.textContent = t('modal.vis_public');
        else if (data.github_public === false) visEl.textContent = t('modal.vis_private');
        else visEl.textContent = t('modal.vis_unknown');
        renderTimestamp(document.getElementById('vGitHubCheckAt'), data.github_check_at);
        const startStopSpan = document.getElementById('modalStartStopSpan');
        const startBtn = document.getElementById('modalStartBtnFooter');
        const stopBtn = document.getElementById('modalStopBtnFooter');
        startBtn.setAttribute('data-name', name);
        stopBtn.setAttribute('data-name', name);
        if (data.status === 'installed') {
          startStopSpan.style.display = 'inline-block';
          startBtn.style.display = data.running ? 'none' : 'inline-block';
          stopBtn.style.display = data.running ? 'inline-block' : 'none';
        } else if (data.status === 'unknown') {
          // When status probing fails, let the user manually try to start/stop to self-heal
          startStopSpan.style.display = 'inline-block';
          startBtn.style.display = 'inline-block';
          stopBtn.style.display = 'inline-block';
        } else {
          startBtn.style.display = 'none';
          stopBtn.style.display = 'none';
        }
      })
      .catch(() => { modalMsg.textContent = t('msg.load_failed'); modalMsg.style.display = 'block'; modalMsg.className = 'msg err'; });
  } else {
    document.getElementById('modalStartStopSpan').style.display = 'none';
    modalTitle.textContent = t('modal.edit_title');
    modalView.style.display = 'none';
    modalEditForm.style.display = '';   // Same as above; the two columns are controlled by the stylesheet
    modalEditBtn.style.display = 'none';
    modalSaveBtn.style.display = 'inline-block';
    fetch('/api/runners/' + encodeURIComponent(name))
      .then(r => r.ok ? r.json() : Promise.reject(r))
      .then(data => {
        document.getElementById('eName').value = data.name || '';
        document.getElementById('eNameDisplay').value = data.name || '';
        document.getElementById('ePath').value = data.path || '';
        document.getElementById('eTargetType').value = data.target_type || 'org';
        document.getElementById('eTarget').value = data.target || '';
        document.getElementById('eCpus').value = data.cpus || '';
        document.getElementById('eMemory').value = data.memory || '';
        document.getElementById('eLabelsStr').value = Array.isArray(data.labels) ? data.labels.join(', ') : (data.labels || '');
      })
      .catch(() => { modalMsg.textContent = t('msg.load_failed'); modalMsg.style.display = 'block'; modalMsg.className = 'msg err'; });
  }
  modal.classList.add('show');
  // Move focus into the dialog; otherwise Tab still starts from the top of the page, and screen readers stay on the background content
  const firstInModal = focusablesIn(modal)[0];
  if (firstInModal) firstInModal.focus();
}

function closeModal() {
  modal.classList.remove('show');
  // Return focus to the button that opened it; the element may have been replaced by auto-refresh, so first check it is still in the document
  if (lastFocused && document.contains(lastFocused)) lastFocused.focus();
  lastFocused = null;
}

document.getElementById('modalClose').addEventListener('click', closeModal);
document.getElementById('modalCancelBtn').addEventListener('click', closeModal);
modal.addEventListener('click', (e) => { if (e.target === modal) closeModal(); });
modal.addEventListener('keydown', (e) => trapTab(e, modal));
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  // The confirm dialog sits on top of the view dialog, so Esc cancels it first; otherwise it would close the dialog underneath too
  if (confirmModal.classList.contains('show')) { settleConfirm(false); return; }
  if (modal.classList.contains('show')) closeModal();
});

document.getElementById('modalEditBtn').addEventListener('click', () => {
  const name = document.getElementById('vName').textContent;
  if (name) openModal('edit', name);
});

document.getElementById('modalSaveBtn').addEventListener('click', async () => {
  const name = document.getElementById('eName').value;
  if (!name) return;
  const labelsStr = document.getElementById('eLabelsStr').value.trim();
  const labels = labelsStr ? labelsStr.split(',').map(s => s.trim()).filter(Boolean) : [];
  const body = {
    name: name,
    path: document.getElementById('ePath').value.trim() || undefined,
    target_type: document.getElementById('eTargetType').value,
    target: document.getElementById('eTarget').value.trim(),
    labels: labels,
    cpus: document.getElementById('eCpus').value.trim(),
    memory: document.getElementById('eMemory').value.trim()
  };
  modalMsg.style.display = 'none';
  try {
    const r = await fetch('/api/runners/' + encodeURIComponent(name), {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    });
    const data = await r.json().catch(() => ({}));
    if (!r.ok) {
      modalMsg.className = 'msg err';
      modalMsg.textContent = data.message || r.statusText;
      modalMsg.style.display = 'block';
      return;
    }
    modalMsg.className = 'msg ok';
    modalMsg.textContent = data.message || t('msg.saved');
    modalMsg.style.display = 'block';
    setTimeout(() => { closeModal(); location.reload(); }, 800);
  } catch (e) {
    modalMsg.className = 'msg err';
    modalMsg.textContent = e.message;
    modalMsg.style.display = 'block';
  }
});

async function removeRunner(name, btn) {
  if (!name || !(await confirmDialog(t('confirm_remove').replace('{{name}}', name)))) return;
  setRowBusy(btn, true);
  try {
    const r = await fetch('/api/runners/' + encodeURIComponent(name), { method: 'DELETE' });
    const data = await r.json().catch(() => ({}));
    if (r.ok) { location.reload(); return; }
    showToast(data.message || r.statusText || t('msg.request_failed'), 'err');
  } catch (e) { showToast(e.message, 'err'); }
  setRowBusy(btn, false);
}
// Inline buttons use event delegation instead of per-button addEventListener: auto-refresh replaces the whole <tbody>,
// so listeners bound directly to the buttons would all stop working after the first refresh.
const runnerRowsBody = document.getElementById('runnerRowsBody');
runnerRowsBody.addEventListener('click', (e) => {
  const btn = e.target.closest('button[data-name]');
  if (!btn || !runnerRowsBody.contains(btn)) return;
  const name = btn.getAttribute('data-name');
  if (!name) return;
  // Pass the button down the chain so the busy state knows which row to grey out
  if (btn.classList.contains('btn-view')) openModal('view', name);
  else if (btn.classList.contains('btn-edit')) openModal('edit', name);
  else if (btn.classList.contains('btn-del')) removeRunner(name, btn);
  else if (btn.classList.contains('btn-start')) runnerAction(name, 'start', btn);
  else if (btn.classList.contains('btn-stop')) runnerAction(name, 'stop', btn);
  // Rebuilding deletes the container and recreates it from the current config; a running Job would be interrupted, so confirm first
  else if (btn.classList.contains('btn-recreate')) {
    confirmDialog(fillVars(t('confirm_recreate'), { name: name })).then(function(ok) {
      if (ok) runnerAction(name, 'recreate', btn);
    });
  }
});

async function runnerAction(name, action, btn) {
  setRowBusy(btn, true);
  try {
    const r = await fetch('/api/runners/' + encodeURIComponent(name) + '/' + action, { method: 'POST' });
    const data = await r.json().catch(() => ({}));
    if (!r.ok) {
      showToast(data.message || r.statusText || t('msg.request_failed'), 'err');
    } else {
      const probeError = resolveProbeError(data);
      if (!probeError) { location.reload(); return; }
      // A probe failure no longer opens three native dialogs in a row (alert → confirm → alert): one toast gives the conclusion,
      // "View details" opens the existing view dialog directly, along with its check command, fix command,
      // and the "show first, copy after confirmation" protections, which are already there, so there's no need to redo them.
      // Don't reload the whole page here, or the toast would vanish immediately; the list is covered by auto-refresh.
      showToast(
        (data.message || t('msg.action_done')) +
        '\n' + t('probe.alert_type') + resolveProbeType(data) +
        '\n' + t('probe.alert_suggestion') + resolveProbeSuggestion(data) +
        '\n' + t('probe.alert_error') + probeError,
        'warn',
        [
          { label: t('toast.view_detail'), onClick: function() { openModal('view', name); } },
          { label: t('toast.reload_list'), onClick: function() { location.reload(); } }
        ]
      );
    }
  } catch (e) { showToast(e.message, 'err'); }
  setRowBusy(btn, false);
}
revealFixBtn.addEventListener('click', async () => {
  if (!currentProbeFixCommand) return;
  if (!(await confirmDialog(t('confirm_reveal_fix')))) return;
  probeFixCmdEl.textContent = currentProbeFixCommand;
  probeFixRevealed = true;
  copyFixBtn.style.display = 'inline-block';
});
copyCheckBtn.addEventListener('click', async () => {
  if (!currentProbeCheckCommand) return;
  const ok = await copyCommandText(currentProbeCheckCommand);
  showToast(ok ? t('msg.check_cmd_copied') : t('msg.copy_failed'), ok ? 'ok' : 'err');
});
copyFixBtn.addEventListener('click', async () => {
  if (!probeFixRevealed || !currentProbeFixCommand) {
    showToast(t('msg.show_fix_first'), 'warn');
    return;
  }
  const ok = await copyCommandText(currentProbeFixCommand);
  showToast(ok ? t('msg.fix_cmd_copied') : t('msg.copy_failed'), ok ? 'ok' : 'err');
});
// ===== List auto-refresh =====
// "Busy" and "GitHub ✓" are already results of a background check roughly every 5 minutes; if the page doesn't refresh,
// you'll never see them change, so these two columns are effectively dead in everyday use.
// It fetches the <tbody> fragment rendered by /api/runner-rows (same template as the first paint),
// rather than building the lines by hand from /api/runners JSON — the GitHubYes/GitHubNo tri-state
// logic rewritten in JS is exactly where an idle runner is most likely to show as busy.
const REFRESH_MS = 15000;
const autoRefreshToggle = document.getElementById('autoRefreshToggle');
const refreshStatusEl = document.getElementById('refreshStatus');
let refreshTimer = null;
let refreshing = false;

function refreshPaused() {
  // no point refreshing while the modal is open (the user is reading its data); don't bother when the tab is hidden
  // server — in container mode every refresh runs docker inspect on each runner.
  // include the confirm dialog too: replacing <tbody> while the user is confirming a delete would pull the row out from under them
  return !autoRefreshToggle.checked || document.hidden ||
    modal.classList.contains('show') || confirmModal.classList.contains('show');
}

async function refreshRows() {
  if (refreshing || refreshPaused()) return;
  refreshing = true;
  try {
    const r = await fetch('/api/runner-rows', { headers: { 'Accept': 'text/html' } });
    if (!r.ok) throw new Error(r.statusText);
    const html = await r.text();
    // re-check: the user may have opened the modal or turned auto-refresh off while the request was in flight
    if (!refreshPaused()) {
      runnerRowsBody.innerHTML = html;
      // the swapped-in rows are a fresh batch and the filter state doesn't carry over — without re-filtering,
      // rows filtered out every 15 seconds would reappear on their own
      applyFilter();
      refreshStatusEl.className = 'refresh-status';
      refreshStatusEl.textContent = fillVars(t('autorefresh.updated'), { time: new Date().toLocaleTimeString() });
    }
  } catch (_) {
    refreshStatusEl.className = 'refresh-status err';
    refreshStatusEl.textContent = t('autorefresh.failed');
  } finally {
    refreshing = false;
  }
}

autoRefreshToggle.addEventListener('change', function() {
  try { localStorage.setItem('runnerDeckAutoRefresh', this.checked ? '1' : '0'); } catch (_) {}
  if (this.checked) { refreshStatusEl.className = 'refresh-status'; refreshStatusEl.textContent = ''; refreshRows(); }
  else { refreshStatusEl.className = 'refresh-status'; refreshStatusEl.textContent = t('autorefresh.paused'); }
});
try {
  if (localStorage.getItem('runnerDeckAutoRefresh') === '0') autoRefreshToggle.checked = false;
} catch (_) {}
if (!autoRefreshToggle.checked) refreshStatusEl.textContent = t('autorefresh.paused');
// refresh immediately when the tab becomes visible again instead of waiting for the next cycle
document.addEventListener('visibilitychange', () => { if (!document.hidden) refreshRows(); });
refreshTimer = setInterval(refreshRows, REFRESH_MS);
// ===== overview stats and filters =====
// This is a fleet manager, but the list used to be flat: with 20+ runners you could only find one with Ctrl+F,
// and there was no way to see how many were running or how many were down. The chips are both the stats and the filter entry points.
const PREDICATES = {
  all: function() { return true; },
  running: function(r) { return r.running; },
  stopped: function(r) { return r.status === 'installed' && !r.running; },
  new: function(r) { return r.status === 'new'; },
  // abnormal: directory missing, status unreadable, or probe failed — each of these needs someone to look at it
  problem: function(r) { return r.status === 'missing' || r.status === 'unknown' || r.probe; },
  drift: function(r) { return r.drift; }
};
const statsEl = document.getElementById('listStats');
const searchEl = document.getElementById('runnerSearch');
const filterCountEl = document.getElementById('filterCount');
const filterClearBtn = document.getElementById('filterClearBtn');
let activeFilter = 'all';

function readRows() {
  return Array.prototype.map.call(document.querySelectorAll('tr[data-name]'), function(tr) {
    return {
      tr: tr,
      name: (tr.dataset.name || '').toLowerCase(),
      target: (tr.dataset.target || '').toLowerCase(),
      status: tr.dataset.status || '',
      running: tr.dataset.running === '1',
      drift: tr.dataset.drift === '1',
      probe: tr.dataset.probe === '1'
    };
  });
}

function applyFilter() {
  const rows = readRows();
  const q = (searchEl.value || '').trim().toLowerCase();
  const pred = PREDICATES[activeFilter] || PREDICATES.all;
  let shown = 0;
  rows.forEach(function(r) {
    const hit = pred(r) && (!q || r.name.indexOf(q) >= 0 || r.target.indexOf(q) >= 0);
    r.tr.style.display = hit ? '' : 'none';
    if (hit) shown++;
  });
  // counts always cover everything and don't follow the current filter: otherwise, after clicking into "needs attention", the other chips
  // would all drop to 0 and you'd lose the global picture
  statsEl.querySelectorAll('.stat-chip').forEach(function(chip) {
    const n = rows.filter(PREDICATES[chip.dataset.filter] || PREDICATES.all).length;
    chip.querySelector('.n').textContent = n;
    chip.classList.toggle('has', n > 0);
    // hide chips whose count is 0 so a small deployment doesn't show a row of empty chips;
    // the currently selected chip stays even at 0, otherwise there'd be no way to exit the filter once applied
    chip.style.display = (n > 0 || chip.dataset.filter === 'all' || chip.dataset.filter === activeFilter) ? '' : 'none';
  });
  const filtering = activeFilter !== 'all' || q !== '';
  filterCountEl.textContent = filtering ? fillVars(t('filter.showing'), { shown: shown, total: rows.length }) : '';
  filterClearBtn.style.display = filtering ? 'inline-block' : 'none';
  // fetch it fresh each time: this row lives in the runnerRows fragment, and auto-refresh replaces the whole thing,
  // so a reference saved at load time would point to a node that has already left the document
  const noMatchRow = document.getElementById('noMatchRow');
  if (noMatchRow) noMatchRow.style.display = (rows.length > 0 && shown === 0) ? '' : 'none';
}

function setActiveChip(name) {
  activeFilter = name;
  statsEl.querySelectorAll('.stat-chip').forEach(function(c) { c.classList.toggle('active', c.dataset.filter === name); });
}
statsEl.addEventListener('click', function(e) {
  const chip = e.target.closest('.stat-chip');
  if (!chip) return;
  setActiveChip(chip.dataset.filter);
  applyFilter();
});
searchEl.addEventListener('input', applyFilter);
filterClearBtn.addEventListener('click', function() {
  searchEl.value = '';
  setActiveChip('all');
  applyFilter();
  searchEl.focus();
});
applyFilter();

document.getElementById('modalStartBtnFooter').addEventListener('click', function() { runnerAction(this.getAttribute('data-name'), 'start', this); });
document.getElementById('modalStopBtnFooter').addEventListener('click', function() { runnerAction(this.getAttribute('data-name'), 'stop', this); });

// ===== Shell: theme, sidebar, hash navigation =====
(function () {
  var themeSelect = document.getElementById('themeSelect');
  var media = window.matchMedia('(prefers-color-scheme: dark)');
  function storedTheme() {
    try { return localStorage.getItem('runnerDeckTheme') || 'system'; } catch (_) { return 'system'; }
  }
  function applyTheme(mode) {
    var dark = mode === 'dark' || (mode !== 'light' && media.matches);
    document.documentElement.dataset.theme = dark ? 'dark' : 'light';
  }
  themeSelect.value = storedTheme();
  themeSelect.addEventListener('change', function () {
    try {
      if (this.value === 'system') localStorage.removeItem('runnerDeckTheme');
      else localStorage.setItem('runnerDeckTheme', this.value);
    } catch (_) {}
    applyTheme(this.value);
  });
  media.addEventListener('change', function () { applyTheme(storedTheme()); });

  var sidebar = document.getElementById('sidebar');
  document.getElementById('sidebarToggle').addEventListener('click', function () { sidebar.classList.toggle('open'); });
  document.getElementById('sidebarScrim').addEventListener('click', function () { sidebar.classList.remove('open'); });

  var POLL_MS = 5000;
  var MAX_SAMPLES = 120;
  var SERIES = ['--s1', '--s2', '--s3', '--s4', '--s5', '--s6'];
  var pollTimer = null;
  var history = {};
  var sampleCount = 0;

  function el(id) { return document.getElementById(id); }
  function fmtBytes(n) {
    if (n === null || n === undefined) return '—';
    var units = ['B', 'KB', 'MB', 'GB', 'TB'];
    var i = 0;
    var v = n;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return (i === 0 || v >= 100 ? Math.round(v) : v.toFixed(1)) + ' ' + units[i];
  }
  function fmtPct(v) { return (v >= 10 ? Math.round(v) : v.toFixed(1)) + '%'; }
  function tfill(key, vars) {
    return t(key).replace(/\{\{(\w+)\}\}/g, function (_, k) { return vars[k]; });
  }
  function meterEl(ratio) {
    var wrap = document.createElement('div');
    wrap.className = 'meter';
    var bar = document.createElement('i');
    var pct = Math.max(0, Math.min(100, ratio * 100));
    bar.style.width = pct + '%';
    if (pct >= 90) bar.className = 'bad';
    else if (pct >= 75) bar.className = 'warn';
    wrap.appendChild(bar);
    return wrap;
  }
  function meterCell(text, sub, ratio) {
    var td = document.createElement('td');
    var label = document.createElement('div');
    label.className = 'meter-text';
    label.textContent = text;
    if (sub) {
      var s = document.createElement('span');
      s.textContent = ' / ' + sub;
      label.appendChild(s);
    }
    td.appendChild(label);
    td.appendChild(meterEl(ratio));
    return td;
  }
  function textCell(text, cls) {
    var td = document.createElement('td');
    td.textContent = text;
    if (cls) td.className = cls;
    return td;
  }

  function renderRunners(data) {
    var body = el('sysRows');
    body.textContent = '';
    if (!data.runners.length) {
      var tr = document.createElement('tr');
      var td = textCell(t('system.no_runners'), 'cell-empty');
      td.colSpan = 6;
      tr.appendChild(td);
      body.appendChild(tr);
      return;
    }
    data.runners.forEach(function (r) {
      var tr = document.createElement('tr');
      tr.appendChild(textCell(r.name));
      tr.appendChild(textCell(r.target, 'row-sub'));
      if (r.has_stats) {
        var cores = parseFloat(r.cpu_limit) || data.host_cpus || 1;
        tr.appendChild(meterCell(fmtPct(r.cpu_pct), tfill('system.cores', { n: cores }), r.cpu_pct / (cores * 100)));
        tr.appendChild(meterCell(fmtBytes(r.mem_bytes), fmtBytes(r.mem_limit_bytes), r.mem_limit_bytes ? r.mem_bytes / r.mem_limit_bytes : 0));
      } else {
        tr.appendChild(textCell('—', 'row-sub'));
        tr.appendChild(textCell('—', 'row-sub'));
      }
      var diskCell = textCell(fmtBytes(r.disk_bytes) + ' ', 'tabular');
      if (r.running && data.container_mode) {
        var cleanBtn = document.createElement('button');
        cleanBtn.type = 'button';
        cleanBtn.className = 'btn-neutral clean-work';
        cleanBtn.dataset.name = r.name;
        cleanBtn.textContent = t('system.clean');
        diskCell.appendChild(cleanBtn);
      }
      tr.appendChild(diskCell);
      var status = document.createElement('td');
      var badge = document.createElement('span');
      badge.className = 'badge ' + (r.running ? 'ok' : 'off');
      badge.textContent = t(r.running ? 'system.status_online' : 'system.status_offline');
      status.appendChild(badge);
      if (r.cpu_override || r.memory_override) {
        var own = document.createElement('span');
        own.className = 'badge off';
        own.textContent = t('system.custom_limit');
        status.appendChild(own);
      }
      if (r.oom_kills > 0) {
        var oom = document.createElement('div');
        oom.className = 'limit-warn bad';
        oom.textContent = tfill('system.warn_oom', { n: r.oom_kills });
        status.appendChild(oom);
      }
      if (r.cpu_throttled_pct >= 10) {
        var cpuWarn = document.createElement('div');
        cpuWarn.className = 'limit-warn';
        cpuWarn.textContent = tfill('system.warn_cpu', { n: Math.round(r.cpu_throttled_pct) });
        status.appendChild(cpuWarn);
      }
      tr.appendChild(status);
      body.appendChild(tr);
    });
  }

  function renderDocker(data) {
    var labels = { 'Images': 'system.docker_images', 'Containers': 'system.docker_containers', 'Local Volumes': 'system.docker_volumes', 'Build Cache': 'system.docker_cache' };
    var body = el('sysDocker');
    body.textContent = '';
    (data.docker || []).forEach(function (row) {
      var tr = document.createElement('tr');
      tr.appendChild(textCell(labels[row.type] ? t(labels[row.type]) : row.type));
      tr.appendChild(textCell(row.count, 'tabular'));
      tr.appendChild(textCell(row.size, 'tabular'));
      tr.appendChild(textCell(row.reclaimable, 'tabular'));
      var action = document.createElement('td');
      var target = row.type === 'Images' ? 'images' : row.type === 'Build Cache' ? 'build-cache' : '';
      if (target) {
        var btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'btn-neutral clean-docker';
        btn.dataset.target = target;
        btn.textContent = t('system.clean');
        action.appendChild(btn);
      }
      tr.appendChild(action);
      body.appendChild(tr);
    });
  }

  // series: [{ name, points: [{ x: 0..1, y }] }]. A null point breaks the line.
  function drawChart(id, series, format, emptyText) {
    var host = el(id);
    host.textContent = '';
    if (!series.length) {
      var empty = document.createElement('div');
      empty.className = 'chart-empty';
      empty.textContent = emptyText;
      host.appendChild(empty);
      return;
    }
    var legend = document.createElement('div');
    legend.className = 'chart-legend';
    series.forEach(function (s, i) {
      var item = document.createElement('span');
      var swatch = document.createElement('i');
      swatch.style.background = 'var(' + SERIES[i % SERIES.length] + ')';
      item.appendChild(swatch);
      item.appendChild(document.createTextNode(s.name));
      legend.appendChild(item);
    });
    host.appendChild(legend);

    var max = 0;
    series.forEach(function (s) { s.points.forEach(function (p) { if (p && p.y > max) max = p.y; }); });
    max = max > 0 ? max * 1.15 : 1;
    var W = 600, H = 190, L = 56, B = 6, T = 6;
    var plotW = W - L, plotH = H - B - T;
    var NS = 'http://www.w3.org/2000/svg';
    var svg = document.createElementNS(NS, 'svg');
    svg.setAttribute('viewBox', '0 0 ' + W + ' ' + H);
    svg.setAttribute('preserveAspectRatio', 'none');
    svg.setAttribute('role', 'img');
    for (var g = 0; g <= 4; g++) {
      var y = T + plotH * g / 4;
      var line = document.createElementNS(NS, 'line');
      line.setAttribute('class', 'grid-line');
      line.setAttribute('x1', L); line.setAttribute('x2', W);
      line.setAttribute('y1', y); line.setAttribute('y2', y);
      line.setAttribute('vector-effect', 'non-scaling-stroke');
      svg.appendChild(line);
      var label = document.createElementNS(NS, 'text');
      label.setAttribute('x', 0);
      label.setAttribute('y', y + 4);
      label.textContent = format(max * (4 - g) / 4);
      svg.appendChild(label);
    }
    series.forEach(function (s, i) {
      var d = '';
      var pen = false;
      s.points.forEach(function (p) {
        if (!p) { pen = false; return; }
        var x = L + plotW * p.x;
        var yy = T + plotH * (1 - p.y / max);
        d += (pen ? 'L' : 'M') + x.toFixed(1) + ' ' + yy.toFixed(1);
        pen = true;
      });
      var path = document.createElementNS(NS, 'path');
      path.setAttribute('d', d);
      path.setAttribute('stroke', 'var(' + SERIES[i % SERIES.length] + ')');
      svg.appendChild(path);
    });
    host.appendChild(svg);
  }

  function renderChart(id, field, format) {
    var names = Object.keys(history).filter(function (n) { return history[n].some(function (p) { return p; }); });
    var series = sampleCount < 2 ? [] : names.map(function (name) {
      return {
        name: name,
        points: history[name].map(function (p, idx) { return p ? { x: idx / (MAX_SAMPLES - 1), y: p[field] } : null; })
      };
    });
    drawChart(id, series, format, t('system.chart_empty'));
  }

  // ===== Saved history =====
  var chartRange = el('chartRange');
  function renderSavedHistory(data) {
    var now = Date.now();
    var span = data.hours * 3600 * 1000;
    var bucketGap = data.hours <= 24 ? 15 * 60 * 1000 : data.hours <= 168 ? 3 * 3600 * 1000 : 18 * 3600 * 1000;
    var byRunner = {};
    (data.points || []).forEach(function (p) {
      (byRunner[p.r] = byRunner[p.r] || []).push(p);
    });
    function seriesFor(field) {
      return Object.keys(byRunner).sort().map(function (name) {
        var pts = [];
        var last = null;
        byRunner[name].forEach(function (p) {
          var at = new Date(p.t).getTime();
          // A gap means the runner was stopped: do not draw a line across it.
          if (last !== null && at - last > bucketGap) pts.push(null);
          pts.push({ x: Math.max(0, 1 - (now - at) / span), y: p[field] });
          last = at;
        });
        return { name: name, points: pts };
      });
    }
    drawChart('sysChartCpu', seriesFor('c'), function (v) { return fmtPct(v); }, t('system.history_empty'));
    drawChart('sysChartMem', seriesFor('m'), fmtBytes, t('system.history_empty'));
  }
  function loadSavedHistory() {
    if (chartRange.value === 'session') return;
    fetch('/api/metrics?hours=' + encodeURIComponent(chartRange.value))
      .then(function (res) { return res.ok ? res.json() : null; })
      .then(function (data) { if (data) renderSavedHistory(data); })
      .catch(function () {});
  }
  chartRange.addEventListener('change', function () {
    if (this.value === 'session') {
      renderChart('sysChartCpu', 'cpu', function (v) { return fmtPct(v); });
      renderChart('sysChartMem', 'mem', fmtBytes);
    } else {
      loadSavedHistory();
    }
  });

  function record(data) {
    sampleCount++;
    var seen = {};
    data.runners.forEach(function (r) {
      seen[r.name] = true;
      var arr = history[r.name] || (history[r.name] = new Array(MAX_SAMPLES).fill(null));
      arr.push(r.has_stats ? { cpu: r.cpu_pct, mem: r.mem_bytes } : null);
      arr.shift();
    });
    Object.keys(history).forEach(function (n) { if (!seen[n]) delete history[n]; });
  }

  function renderSystem(data) {
    var err = el('sysError');
    err.hidden = !data.stats_error;
    if (data.stats_error) err.textContent = tfill('system.stats_failed', { error: data.stats_error });
    if (data.disk) {
      el('sysDisk').textContent = fmtBytes(data.disk.used_bytes);
      el('sysDiskSub').textContent = tfill('system.disk_sub', { free: fmtBytes(data.disk.free_bytes), total: fmtBytes(data.disk.total_bytes) });
      var ratio = data.disk.total_bytes ? data.disk.used_bytes / data.disk.total_bytes : 0;
      var bar = el('sysDiskMeter');
      bar.style.width = (ratio * 100) + '%';
      bar.className = ratio >= 0.9 ? 'bad' : ratio >= 0.75 ? 'warn' : '';
    }
    var running = data.runners.filter(function (r) { return r.running; }).length;
    var cpu = 0, mem = 0;
    data.runners.forEach(function (r) { cpu += r.cpu_pct; mem += r.mem_bytes; });
    el('sysRunning').textContent = running;
    el('sysRunningSub').textContent = tfill('system.running_sub', { total: data.runners.length });
    el('sysCpu').textContent = fmtPct(cpu);
    el('sysCpuSub').textContent = tfill('system.others_sub', { value: fmtPct(data.others.cpu_pct) });
    el('sysMem').textContent = fmtBytes(mem);
    el('sysMemSub').textContent = tfill('system.others_sub', { value: fmtBytes(data.others.mem_bytes) });
    var cpusInput = el('limitCpus');
    var memoryInput = el('limitMemory');
    if (data.limits && document.activeElement !== cpusInput && document.activeElement !== memoryInput && !limitsDirty) {
      cpusInput.value = data.limits.cpus || '';
      memoryInput.value = data.limits.memory || '';
    }
    renderRunners(data);
    renderDocker(data);
    if (chartRange.value === 'session') {
      renderChart('sysChartCpu', 'cpu', function (v) { return fmtPct(v); });
      renderChart('sysChartMem', 'mem', fmtBytes);
    }
  }

  function runCleanup(btn, url, body) {
    btn.disabled = true;
    btn.textContent = t('btn.working');
    fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    })
      .then(function (res) { return res.json().catch(function () { return {}; }).then(function (b) { return { ok: res.ok, body: b }; }); })
      .then(function (r) {
        showToast(r.body.message || t('msg.request_failed'), r.ok ? 'ok' : 'err');
        pollSystem();
      })
      .catch(function () { showToast(t('msg.request_failed'), 'err'); });
  }
  el('sysRows').addEventListener('click', function (e) {
    var btn = e.target.closest('.clean-work');
    if (!btn) return;
    var name = btn.dataset.name;
    confirmDialog(fillVars(t('system.confirm_clean_work'), { name: name })).then(function (ok) {
      if (ok) runCleanup(btn, '/api/runners/' + encodeURIComponent(name) + '/clean', {});
    });
  });
  el('sysDocker').addEventListener('click', function (e) {
    var btn = e.target.closest('.clean-docker');
    if (!btn) return;
    var target = btn.dataset.target;
    confirmDialog(t(target === 'images' ? 'system.confirm_clean_images' : 'system.confirm_clean_cache')).then(function (ok) {
      if (ok) runCleanup(btn, '/api/docker/clean', { target: target });
    });
  });
  var limitsDirty = false;
  el('limitsForm').addEventListener('input', function () { limitsDirty = true; });
  el('limitsForm').addEventListener('submit', function (e) {
    e.preventDefault();
    var btn = this.querySelector('button[type="submit"]');
    btn.disabled = true;
    fetch('/api/settings/resources', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ cpus: el('limitCpus').value.trim(), memory: el('limitMemory').value.trim() })
    })
      .then(function (res) { return res.json().catch(function () { return {}; }).then(function (body) { return { ok: res.ok, body: body }; }); })
      .then(function (r) {
        showToast(r.body.message || t('msg.request_failed'), r.ok ? 'ok' : 'err');
        if (r.ok) { limitsDirty = false; pollSystem(); }
      })
      .catch(function () { showToast(t('msg.request_failed'), 'err'); })
      .finally(function () { btn.disabled = false; });
  });
  var polling = false;
  function pollSystem() {
    // docker stats can outlast the interval on a busy host: skip instead of stacking requests.
    if (polling) return;
    polling = true;
    fetch('/api/system', { headers: { Accept: 'application/json' } })
      .then(function (res) {
        if (!res.ok) throw new Error('HTTP ' + res.status);
        return res.json();
      })
      .then(function (data) { record(data); renderSystem(data); })
      .catch(function (e) {
        var err = el('sysError');
        err.hidden = false;
        err.textContent = tfill('system.stats_failed', { error: e.message });
      })
      .finally(function () { polling = false; });
  }

  // ===== Activity log =====
  var eventsTimer = null;
  var historyTick = 0;
  var eventsRunner = el('eventsRunner');
  var eventsErrors = el('eventsErrors');
  function eventText(e) {
    var key = 'event.' + e.code;
    var text = t(key);
    if (text === key) return e.code + (e.detail ? ': ' + e.detail : '');
    return text.split('{{detail}}').join(e.detail || '');
  }
  function renderEvents(events) {
    var body = el('eventsRows');
    body.textContent = '';
    if (!events.length) {
      var empty = document.createElement('tr');
      var cell = textCell(t('events.empty'), 'cell-empty');
      cell.colSpan = 3;
      empty.appendChild(cell);
      body.appendChild(empty);
      return;
    }
    events.forEach(function (e) {
      var tr = document.createElement('tr');
      if (e.level === 'error') tr.className = 'event-error';
      else if (e.level === 'warn') tr.className = 'event-warn';
      tr.appendChild(textCell(new Date(e.time).toLocaleString(document.documentElement.lang), 'tabular row-sub event-time'));
      tr.appendChild(textCell(e.runner || '—'));
      tr.appendChild(textCell(eventText(e), 'event-text'));
      body.appendChild(tr);
    });
  }
  function fillRunnerFilter() {
    var names = Array.prototype.map.call(document.querySelectorAll('#runnerRowsBody tr[data-name]'), function (r) { return r.dataset.name; });
    var current = eventsRunner.value;
    while (eventsRunner.options.length > 1) eventsRunner.remove(1);
    names.forEach(function (n) {
      var o = document.createElement('option');
      o.value = o.textContent = n;
      eventsRunner.appendChild(o);
    });
    eventsRunner.value = names.indexOf(current) >= 0 ? current : '';
  }
  function fmtDuration(seconds) {
    if (!seconds) return '—';
    var s = Math.round(seconds);
    if (s < 60) return s + 's';
    var m = Math.floor(s / 60);
    if (m < 60) return m + 'm ' + (s % 60) + 's';
    return Math.floor(m / 60) + 'h ' + (m % 60) + 'm';
  }
  function successRate(c) {
    var decided = c.succeeded + c.failed;
    return decided ? c.succeeded * 100 / decided : null;
  }
  function renderStats(s) {
    var rate = successRate(s);
    el('statsTotal').textContent = s.total;
    el('statsTotalSub').textContent = tfill('stats.succeeded_of', { ok: s.succeeded, canceled: s.canceled });
    el('statsRate').textContent = rate === null ? '—' : fmtPct(rate);
    var bar = el('statsRateMeter');
    bar.style.width = (rate || 0) + '%';
    bar.className = rate !== null && rate < 70 ? 'bad' : rate !== null && rate < 90 ? 'warn' : '';
    el('statsFailed').textContent = s.failed;
    el('statsAvg').textContent = fmtDuration(s.avg_seconds);
    el('statsEmpty').hidden = s.total > 0;
    el('statsTableWrap').hidden = s.total === 0;
    var body = el('statsRows');
    body.textContent = '';
    (s.runners || []).forEach(function (r) {
      var tr = document.createElement('tr');
      var runnerRate = successRate(r);
      tr.appendChild(textCell(r.runner));
      tr.appendChild(textCell(r.total, 'tabular'));
      tr.appendChild(textCell(runnerRate === null ? '—' : fmtPct(runnerRate), 'tabular'));
      tr.appendChild(textCell(r.failed, 'tabular'));
      tr.appendChild(textCell(fmtDuration(r.avg_seconds), 'tabular'));
      body.appendChild(tr);
    });
  }
  function loadStats() {
    fetch('/api/stats?days=' + encodeURIComponent(el('statsDays').value))
      .then(function (res) { return res.ok ? res.json() : null; })
      .then(function (s) { if (s) renderStats(s); })
      .catch(function () {});
  }
  el('statsDays').addEventListener('change', loadStats);

  function loadEvents() {
    loadStats();
    var params = new URLSearchParams();
    if (eventsRunner.value) params.set('runner', eventsRunner.value);
    if (eventsErrors.checked) params.set('level', 'error');
    el('eventsOutputBtn').hidden = !eventsRunner.value;
    fetch('/api/events?' + params.toString())
      .then(function (res) { return res.ok ? res.json() : { events: [] }; })
      .then(function (data) { renderEvents(data.events || []); })
      .catch(function () {});
  }
  eventsRunner.addEventListener('change', loadEvents);
  eventsErrors.addEventListener('change', loadEvents);
  el('eventsOutputBtn').addEventListener('click', function () {
    var name = eventsRunner.value;
    el('outputTitle').textContent = t('events.output_title').split('{{name}}').join(name);
    el('outputBody').textContent = '';
    el('outputModal').classList.add('show');
    fetch('/api/runners/' + encodeURIComponent(name) + '/logs')
      .then(function (res) { return res.json().then(function (b) { return { ok: res.ok, body: b }; }); })
      .then(function (r) {
        el('outputBody').textContent = r.ok ? (r.body.logs || t('events.output_empty')) : (r.body.message || t('msg.load_failed'));
        el('outputBody').scrollTop = el('outputBody').scrollHeight;
      })
      .catch(function () { el('outputBody').textContent = t('msg.load_failed'); });
  });
  el('outputClose').addEventListener('click', function () { el('outputModal').classList.remove('show'); });

  function showView() {
    var hash = location.hash;
    var view = hash === '#system' ? 'system' : hash === '#events' ? 'events' : 'runners';
    el('viewRunners').hidden = view !== 'runners';
    el('viewSystem').hidden = view !== 'system';
    el('viewEvents').hidden = view !== 'events';
    document.querySelectorAll('.nav-link').forEach(function (a) {
      var on = a.dataset.view === view;
      a.classList.toggle('active', on);
      if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    });
    sidebar.classList.remove('open');
    clearInterval(pollTimer);
    pollTimer = null;
    clearInterval(eventsTimer);
    eventsTimer = null;
    if (view === 'events') {
      fillRunnerFilter();
      loadEvents();
      eventsTimer = setInterval(loadEvents, 15000);
    }
    if (view === 'system') {
      pollSystem();
      loadSavedHistory();
      pollTimer = setInterval(function () {
        pollSystem();
        historyTick++;
        if (historyTick % 12 === 0) loadSavedHistory();
      }, POLL_MS);
    }
  }
  window.addEventListener('hashchange', showView);
  showView();
})();
