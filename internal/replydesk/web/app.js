/*
 * Reply Desk review UI. Vanilla JS, no dependencies, no build step.
 *
 * CSP-safe: no inline scripts, styles, or handlers, no eval. All data from
 * the server (message text, titles, drafts) is written with textContent and
 * never parsed as HTML.
 *
 * The top half is pure helpers (no DOM) so they can be checked in Node:
 *   node -e "const p = require('./app.js'); console.log(p.countFills('[[FILL: x]]'))"
 */
'use strict';

(function () {
  // ---------------------------------------------------------------------
  // Constants
  // ---------------------------------------------------------------------

  const DEFAULT_MAX_LEN = 7439;
  const FILL = '[[FILL';
  const FAST_POLL_MS = 3000;
  const SLOW_POLL_MS = 30000;
  const REGEN_TIMEOUT_MS = 3 * 60 * 1000;
  const CONFIRM_GUARD_MS = 400;
  const SESSION_EXPIRED = 'Session expired - run webex-mcp --review again';
  const CLOSED = 'Reply Desk is closed - you can close this tab.';
  const DRAFT_ANYWAY = 'Draft a reply anyway.';
  const SEND_UNKNOWN = 'Unknown whether it was sent - check Webex before retrying';
  const SEND_LOST = 'We lost the connection while sending - check the state below before retrying';
  const OPEN_FROM_LINK = 'Open Reply Desk from the link printed by webex-mcp --review';
  const SIGNED_OUT = 'You signed out in another window - close this page and run webex-mcp --review again';
  const LOCK_LOST = 'Reply Desk was opened in another window; this window no longer saves changes - close it';
  // Error codes that end this page's session on any endpoint: the page locks
  // with the matching message.
  const PAGE_LOCK_CODES = { signed_out: SIGNED_OUT, lock_lost: LOCK_LOST };
  const BUSY = 'A draft is already being written - try again when it finishes';
  const DRAFTER_OFF = 'Drafting is unavailable - you can still write the reply yourself';
  // The server's limit on a regenerate instruction, in UTF-8 bytes.
  const MAX_INSTRUCTION_BYTES = 1000;
  // sessionStorage is per origin, and the origin includes the port, so one
  // key cannot collide with another review server's token.
  const CSRF_KEY = 'replydesk-csrf';

  const DONE_STATES = ['sent', 'dismissed'];
  // States the server refuses to (re)draft: the reply is out, on its way, or
  // possibly out.
  const NO_REGEN_STATES = ['sent', 'dismissed', 'sending', 'send_unknown'];

  const STATE_LABELS = {
    pending: 'Pending',
    drafting: 'Drafting',
    drafted: 'Drafted',
    draft_failed: 'Draft failed',
    sending: 'Sending',
    sent: 'Sent',
    send_unknown: 'Send unknown',
    dismissed: 'Dismissed',
    snoozed: 'Snoozed',
  };

  const FLAG_SHORT = {
    attachment: 'files',
    voice_clip: 'voice',
    card: 'card',
    edited: 'edited',
    long_burst: 'long',
    no_history: 'no history',
  };

  const FLAG_TEXT = {
    attachment: 'Has attachments - open in Webex',
    voice_clip: 'Voice clip - listen to it in Webex',
    card: 'Card - open in Webex to see it',
    edited: 'Edited - a message was changed after it was sent',
    long_burst: 'Long burst truncated - only the newest messages are shown',
    no_history: 'No earlier message from you in this conversation',
  };

  const MODE_TITLES = {
    bare: 'claude -p --bare, no tools, no MCP servers',
    isolated: 'claude -p with no tools; your Claude Code hooks still run',
    off: 'claude CLI not found on PATH',
  };

  const DISMISS_LABELS = {
    no_reply_needed: 'No reply needed',
    replied_elsewhere: 'Replied elsewhere',
    reacted: 'Reacted instead',
    other: 'Other',
  };

  const GROUPS = [
    { key: 'needs', label: 'Needs reply' },
    { key: 'noreply', label: 'Probably no reply needed' },
    { key: 'snoozed', label: 'Snoozed' },
    { key: 'done', label: 'Sent & dismissed' },
  ];

  // ---------------------------------------------------------------------
  // Pure helpers
  // ---------------------------------------------------------------------

  const encoder = new TextEncoder();

  /** UTF-8 byte length, the unit the server's limit uses. */
  function byteLen(s) {
    return encoder.encode(s || '').length;
  }

  /** Number of "[[FILL" placeholders left in s. */
  function countFills(s) {
    return s ? s.split(FILL).length - 1 : 0;
  }

  /** [start, end) of the next placeholder at or after from, wrapping; or null. */
  function nextFillRange(s, from) {
    let i = s.indexOf(FILL, from);
    if (i < 0) i = s.indexOf(FILL);
    if (i < 0) return null;
    const j = s.indexOf(']]', i + FILL.length);
    return [i, j < 0 ? i + FILL.length : j + 2];
  }

  function isDone(state) {
    return DONE_STATES.indexOf(state) >= 0;
  }

  /** Whether Draft now / Regenerate is allowed in this state (matches the service). */
  function canRegen(state) {
    return NO_REGEN_STATES.indexOf(state) < 0;
  }

  /** The CSRF token from a "#csrf=..." fragment, or ''. */
  function csrfFromHash(hash) {
    const m = /^#csrf=([A-Za-z0-9_-]+)$/.exec(hash || '');
    return m ? m[1] : '';
  }

  /** Fixed user-facing text for error codes any endpoint can return, or ''. */
  function codeMessage(code) {
    switch (code) {
      case 'busy':
        return BUSY;
      case 'drafter_off':
        return DRAFTER_OFF;
      case 'signed_out':
        return SIGNED_OUT;
      case 'lock_lost':
        return LOCK_LOST;
      default:
        return '';
    }
  }

  /** Inbox group key for an item. */
  function groupOf(item) {
    switch (item.state) {
      case 'snoozed':
        return 'snoozed';
      case 'sent':
      case 'dismissed':
        return 'done';
      case 'drafted':
        return item.draft && item.draft.needs_reply === false ? 'noreply' : 'needs';
      default:
        return 'needs';
    }
  }

  /** Non-empty groups in display order, each {key, label, items}. */
  function groupItems(items, showAll) {
    const out = [];
    for (const g of GROUPS) {
      if (g.key === 'done' && !showAll) continue;
      const list = (items || []).filter((it) => groupOf(it) === g.key);
      if (list.length) out.push({ key: g.key, label: g.label, items: list });
    }
    return out;
  }

  function flatOrder(groups) {
    const out = [];
    for (const g of groups) out.push(...g.items);
    return out;
  }

  function parseTime(iso) {
    if (!iso) return NaN;
    const t = Date.parse(iso);
    // Go's zero time (year 1) is not a real time.
    return t > 0 ? t : NaN;
  }

  /** Short relative age: "just now", "5m ago", "3h ago", "2d ago", "in 4h". */
  function relTime(iso, now) {
    const t = parseTime(iso);
    if (isNaN(t)) return '';
    let s = Math.floor((now - t) / 1000);
    const future = s < 0;
    s = Math.abs(s);
    if (s < 45) return future ? 'in a moment' : 'just now';
    let v;
    if (s < 3600) v = Math.max(1, Math.floor(s / 60)) + 'm';
    else if (s < 86400) v = Math.floor(s / 3600) + 'h';
    else v = Math.floor(s / 86400) + 'd';
    return future ? 'in ' + v : v + ' ago';
  }

  function fmtTime(iso) {
    const t = parseTime(iso);
    if (isNaN(t)) return '';
    return new Date(t).toLocaleString(undefined, {
      year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
    });
  }

  function fmtDate(iso) {
    const t = parseTime(iso);
    if (isNaN(t)) return '';
    return new Date(t).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
  }

  /** Go duration "168h0m0s" -> "7d", "36h0m0s" -> "36h"; anything else unchanged. */
  function fmtLookback(s) {
    const m = /^(\d+)h0m0s$/.exec(s || '');
    if (!m) return s || '';
    const h = parseInt(m[1], 10);
    return h > 0 && h % 24 === 0 ? h / 24 + 'd' : h + 'h';
  }

  /** Why Send is disabled, or '' when it is allowed. */
  function sendBlockReason(text, max, state) {
    if (isDone(state)) return 'Already ' + state;
    if (state === 'sending') return 'Sending...';
    if (!text || !text.trim()) return 'Write a reply first';
    const n = byteLen(text);
    if (n > max) return 'Over the limit by ' + (n - max) + ' bytes';
    const f = countFills(text);
    if (f > 0) return 'Replace ' + (f === 1 ? 'the [[FILL]] placeholder' : 'all ' + f + ' [[FILL]] placeholders') + ' first';
    return '';
  }

  /** Identity of a draft; changes when a new draft is written. */
  function draftKey(draft) {
    if (!draft) return '';
    return (draft.created_at || '') + '\n' + (draft.text || '');
  }

  /**
   * Reconcile a room's editor with the server's current draft.
   * ed is {text, baseKey, baseText, offerKey} or undefined.
   * - First sight: prefill with the draft text.
   * - Draft changed and the user has not edited: take the new draft.
   * - Draft changed and the user has edited: keep the edits, offer the new draft.
   */
  function reconcileEditor(ed, draft) {
    const key = draftKey(draft);
    const text = draft ? draft.text || '' : '';
    if (!ed) return { text: text, baseKey: key, baseText: text, offerKey: '' };
    if (key === ed.baseKey) return ed.offerKey ? Object.assign({}, ed, { offerKey: '' }) : ed;
    const edited = ed.text !== ed.baseText;
    if (!edited) return { text: text, baseKey: key, baseText: text, offerKey: '' };
    if (!draft) return Object.assign({}, ed, { offerKey: '' });
    return Object.assign({}, ed, { offerKey: key });
  }

  /** User-facing message and follow-up actions for a send APIError. */
  function describeError(err, httpStatus) {
    const code = (err && err.code) || '';
    const r = { code: code, message: '', actions: [], retry: false };
    switch (code) {
      case 'already_replied':
        r.message = 'You already replied in this conversation after these messages.';
        r.actions = ['dismiss_replied'];
        break;
      case 'new_messages':
        r.message = 'New messages arrived since the last scan. Rescan and read them before replying.';
        r.actions = ['rescan'];
        break;
      case 'ask_edited':
        r.message = 'The message you are answering was edited since the scan. Rescan and re-read it before replying.';
        r.actions = ['rescan'];
        break;
      case 'ask_deleted':
        r.message = 'The message you are answering was deleted. There may be nothing left to reply to.';
        r.actions = ['rescan'];
        break;
      case 'sanitized':
        if (err && typeof err.sanitized_text === 'string') {
          r.message = 'Reply Desk changed the text for safety (blocked link schemes or mentions). Compare the versions, then send the sanitized version or cancel and edit.';
          r.actions = ['send_sanitized'];
        } else {
          r.message = 'Reply Desk would change the text for safety. Cancel and edit the reply.';
        }
        break;
      case 'fill_placeholders':
        r.message = 'Replace every [[FILL: ...]] placeholder before sending.';
        break;
      case 'too_long':
        r.message = 'The reply is over the Webex length limit. Shorten it and try again.';
        break;
      case 'empty':
        r.message = 'The reply is empty.';
        break;
      case 'hash_mismatch':
        r.message = 'The text changed between confirming and sending. Cancel, review, and confirm again.';
        break;
      case 'not_pending':
        r.message = 'This conversation is no longer waiting for a reply (it was already sent or dismissed).';
        r.actions = ['refresh'];
        break;
      case 'rate_limited':
        r.message = 'Sending too fast. Wait a few seconds and try again.';
        r.retry = true;
        break;
      case 'send_unknown':
        r.message = SEND_UNKNOWN;
        r.actions = ['refresh'];
        break;
      case 'network':
        // The request may have reached the server and the message may be
        // out: never offer a blind retry. The caller shows the server state.
        r.message = SEND_LOST;
        break;
      case 'busy':
      case 'drafter_off':
      case 'signed_out':
      case 'lock_lost':
        r.message = codeMessage(code);
        break;
      default:
        r.message = 'Send failed' + (httpStatus ? ' (HTTP ' + httpStatus + ')' : '') + '.';
        r.retry = true;
    }
    return r;
  }

  function toHex(buf) {
    const b = new Uint8Array(buf);
    let s = '';
    for (let i = 0; i < b.length; i++) s += b[i].toString(16).padStart(2, '0');
    return s;
  }

  /** True when keyboard focus is in a field where letters are typed. */
  function isTypingTarget(node) {
    if (!node || !node.tagName) return false;
    const tag = node.tagName;
    if (tag === 'TEXTAREA' || tag === 'SELECT') return true;
    if (tag === 'INPUT') {
      const t = (node.type || 'text').toLowerCase();
      return ['checkbox', 'radio', 'button', 'submit', 'reset', 'range', 'color', 'file'].indexOf(t) < 0;
    }
    return !!node.isContentEditable;
  }

  const pure = {
    byteLen, countFills, nextFillRange, groupOf, groupItems, flatOrder, relTime, fmtLookback,
    sendBlockReason, draftKey, reconcileEditor, describeError, toHex, isTypingTarget,
    canRegen, csrfFromHash, codeMessage, MAX_INSTRUCTION_BYTES,
  };
  if (typeof module !== 'undefined' && module.exports) module.exports = pure;
  if (typeof document === 'undefined') return;

  // ---------------------------------------------------------------------
  // App state
  // ---------------------------------------------------------------------

  const S = {
    status: null,
    items: [],
    showAll: false,
    selected: null, // room_id
    selectedIndex: 0, // position in flat order, to pick a neighbour when the item leaves
    editors: new Map(), // room_id -> {text, baseKey, baseText, offerKey}
    editorRoom: null, // room whose text is in the textarea
    regenPending: new Map(), // room_id -> {state, key, at}
    autoDrafted: new Set(), // rooms drafted on open in this page
    checked: new Set(), // room_id + '\n' + ask, local checklist ticks
    detailSig: '',
    confirm: null, // {roomId, headId, title, text, sending, lost, openedAt}
    timer: 0,
    inflight: false,
    again: false,
    locked: false,
    offline: false,
  };

  const $ = (id) => document.getElementById(id);
  // Set by loadCSRF in init. The token arrives once, in the fragment of the
  // /login redirect; the HTML never contains it.
  let csrf = '';

  /** Take the token from the fragment (and drop the fragment), else from sessionStorage. */
  function loadCSRF() {
    let tok = csrfFromHash(location.hash);
    if (location.hash) {
      try {
        history.replaceState(null, '', location.pathname + location.search);
      } catch (e) {
        // Leaving the fragment in place is harmless; it is never sent.
      }
    }
    try {
      if (tok) sessionStorage.setItem(CSRF_KEY, tok);
      else tok = sessionStorage.getItem(CSRF_KEY) || '';
    } catch (e) {
      // Storage blocked: the token still works for this page load.
    }
    return tok;
  }

  function forgetCSRF() {
    csrf = '';
    try {
      sessionStorage.removeItem(CSRF_KEY);
    } catch (e) {
      // Nothing stored.
    }
  }

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = String(text);
    return e;
  }

  function button(label, onClick, cls) {
    const b = el('button', cls, label);
    b.type = 'button';
    b.addEventListener('click', onClick);
    return b;
  }

  function maxLen() {
    return (S.status && S.status.max_message_len) || DEFAULT_MAX_LEN;
  }

  function drafterMode() {
    return (S.status && S.status.drafter_mode) || '';
  }

  function canDraft() {
    return !S.locked && drafterMode() !== '' && drafterMode() !== 'off';
  }

  function findItem(roomId) {
    return S.items.find((it) => it.room_id === roomId) || null;
  }

  function selectedItem() {
    return S.selected ? findItem(S.selected) : null;
  }

  function isDrafting(item) {
    return item.state === 'drafting' || S.regenPending.has(item.room_id);
  }

  function anyMenuOpen() {
    return !$('menu-dismiss').hidden || !$('menu-snooze').hidden;
  }

  function anyDialogOpen() {
    return $('dlg-confirm').open || $('dlg-help').open;
  }

  // ---------------------------------------------------------------------
  // Network
  // ---------------------------------------------------------------------

  /** fetch wrapper. Never throws; returns {ok, status, data, error}. */
  async function api(method, path, body) {
    if (S.locked) return { ok: false, status: 0, error: { code: 'locked', detail: '' } };
    // Every /api/ call, GETs included, carries the CSRF header.
    const opts = {
      method: method,
      credentials: 'same-origin',
      cache: 'no-store',
      headers: { Accept: 'application/json', 'X-ReplyDesk-CSRF': csrf },
    };
    if (method !== 'GET') {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body || {});
    }
    let res;
    try {
      res = await fetch(path, opts);
    } catch (e) {
      return { ok: false, status: 0, error: { code: 'network', detail: '' } };
    }
    if (res.status === 401) {
      lock(SESSION_EXPIRED);
      return { ok: false, status: 401, error: { code: 'unauthorized', detail: SESSION_EXPIRED } };
    }
    let data = null;
    try {
      data = await res.json();
    } catch (e) {
      data = null;
    }
    if (!res.ok) {
      const err = data && typeof data.code === 'string' ? data : { code: '', detail: res.statusText || '' };
      if (Object.prototype.hasOwnProperty.call(PAGE_LOCK_CODES, err.code)) {
        // From any endpoint, GET /api/items and POST /api/scan included.
        lock(PAGE_LOCK_CODES[err.code]);
      } else if (res.status === 403 && method === 'GET' && err.code === 'forbidden') {
        // A GET is refused only for a bad CSRF token: this page did not come
        // from the launch link (or the token is from an earlier run).
        forgetCSRF();
        lock(OPEN_FROM_LINK);
      }
      return { ok: false, status: res.status, error: err };
    }
    return { ok: true, status: res.status, data: data };
  }

  async function sha256Hex(text) {
    const digest = await crypto.subtle.digest('SHA-256', encoder.encode(text));
    return toHex(digest);
  }

  function isBusy() {
    if (S.status && (S.status.scanning || S.status.drafting > 0)) return true;
    if (S.regenPending.size) return true;
    return S.items.some((it) => it.state === 'drafting' || it.state === 'sending');
  }

  /** Queue the next poll. A hidden tab does not poll; visibilitychange resumes it. */
  function schedule() {
    clearTimeout(S.timer);
    if (S.locked || document.visibilityState === 'hidden') return;
    S.timer = setTimeout(refresh, isBusy() ? FAST_POLL_MS : SLOW_POLL_MS);
  }

  async function refresh() {
    if (S.locked) return;
    if (S.inflight) {
      S.again = true;
      return;
    }
    S.inflight = true;
    const all = S.showAll;
    try {
      const [st, it] = await Promise.all([
        api('GET', '/api/status'),
        api('GET', all ? '/api/items?all=1' : '/api/items'),
      ]);
      if (S.locked) return;
      if (st.ok && it.ok) {
        S.offline = false;
        S.status = st.data || {};
        if (all === S.showAll) S.items = (it.data && Array.isArray(it.data.items)) ? it.data.items : [];
        else S.again = true;
      } else {
        S.offline = true;
      }
      pruneRegen();
      fixSelection();
      render();
    } finally {
      S.inflight = false;
      if (S.again) {
        S.again = false;
        refresh();
      } else {
        schedule();
      }
    }
  }

  /** Forget regenerate requests whose item has moved on. */
  function pruneRegen() {
    const now = Date.now();
    for (const [roomId, p] of S.regenPending) {
      const item = findItem(roomId);
      if (!item || item.state !== p.state || draftKey(item.draft) !== p.key || now - p.at > REGEN_TIMEOUT_MS) {
        S.regenPending.delete(roomId);
      }
    }
  }

  function fixSelection() {
    const flat = flatOrder(groupItems(S.items, S.showAll));
    if (!flat.length) {
      S.selected = null;
      return;
    }
    let i = flat.findIndex((x) => x.room_id === S.selected);
    if (i < 0) {
      i = Math.min(S.selectedIndex, flat.length - 1);
      S.selected = flat[i].room_id;
    }
    S.selectedIndex = i;
  }

  /** Session expired or closed: stop polling, keep the text copyable. */
  function lock(message) {
    if (S.locked) return;
    S.locked = true;
    clearTimeout(S.timer);
    closeMenus(false);
    if ($('dlg-confirm').open) $('dlg-confirm').close();
    document.body.classList.add('locked');
    const banner = $('banner-lock');
    banner.textContent = message;
    banner.hidden = false;
    for (const b of document.querySelectorAll('button, input')) {
      if (b.id !== 'btn-copy' && b.id !== 'btn-help' && b.id !== 'help-close') b.disabled = true;
    }
  }

  // ---------------------------------------------------------------------
  // Rendering
  // ---------------------------------------------------------------------

  function render() {
    renderStatus();
    renderList();
    renderDetail();
    updateConfirmState();
  }

  function renderStatus() {
    const st = S.status || {};
    const now = Date.now();
    $('st-me').textContent = st.me ? 'Signed in as ' + st.me : '';

    const mode = st.drafter_mode || '';
    const modeEl = $('st-mode');
    modeEl.textContent = mode ? 'Drafter: ' + mode : '';
    modeEl.className = 'st-item badge' + (mode ? ' mode-' + mode : '');
    modeEl.title = MODE_TITLES[mode] || '';
    $('banner-drafter').hidden = mode !== 'off';

    const scan = $('st-scan');
    scan.textContent = st.last_scan ? 'Last scan ' + relTime(st.last_scan, now) : (st.scanning ? '' : 'Not scanned yet');
    scan.title = fmtTime(st.last_scan);

    const act = [];
    if (st.scanning) act.push('Scanning...');
    if (st.drafting > 0) act.push('Drafting ' + st.drafting + '...');
    $('st-activity').textContent = act.join(' ');
    $('st-lookback').textContent = st.lookback ? 'Lookback ' + fmtLookback(st.lookback) : '';

    const err = S.offline ? 'Cannot reach Reply Desk - retrying. If it shut down, run webex-mcp --review again.' : (st.error || '');
    $('banner-error').textContent = err;
    $('banner-error').hidden = !err;
    if (!S.locked) $('btn-rescan').disabled = !!st.scanning;
  }

  function renderList() {
    const now = Date.now();
    const groups = groupItems(S.items, S.showAll);
    const nodes = [];
    for (const g of groups) {
      const sec = el('section', 'group');
      const h = el('h2', null, g.label + ' (' + g.items.length + ')');
      h.id = 'grp-' + g.key;
      sec.setAttribute('aria-labelledby', h.id);
      const ul = el('ul');
      for (const it of g.items) {
        const li = el('li');
        li.append(rowNode(it, now));
        ul.append(li);
      }
      sec.append(h, ul);
      nodes.push(sec);
    }
    $('inbox-groups').replaceChildren(...nodes);
    $('inbox-empty').hidden = groups.length > 0;
    $('inbox-empty').textContent = S.status && S.status.scanning && !groups.length ? 'Scanning...' : 'Nothing is waiting for a reply.';
  }

  function rowNode(it, now) {
    const b = el('button', 'row');
    b.type = 'button';
    b.dataset.room = it.room_id;
    if (it.room_id === S.selected) b.setAttribute('aria-current', 'true');
    b.append(el('span', 'row-title', it.title || '(untitled)'));
    const meta = el('span', 'row-meta');
    const age = el('time', null, relTime(it.head_created, now));
    if (it.head_created) age.dateTime = it.head_created;
    age.title = fmtTime(it.head_created);
    const n = (it.burst || []).length;
    meta.append(age, el('span', null, n + (n === 1 ? ' msg' : ' msgs')));
    meta.append(el('span', 'badge state-' + it.state, isDrafting(it) ? STATE_LABELS.drafting : (STATE_LABELS[it.state] || it.state)));
    for (const f of it.flags || []) {
      const c = el('span', 'chip', FLAG_SHORT[f] || f);
      c.title = FLAG_TEXT[f] || f;
      meta.append(c);
    }
    b.append(meta);
    b.addEventListener('click', () => select(it.room_id, true));
    return b;
  }

  function renderDetail() {
    const item = selectedItem();
    $('detail-empty').hidden = !!item;
    $('detail').hidden = !item;
    if (!item) {
      S.detailSig = '';
      return;
    }
    // Rebuild the read-only parts only when something changed, so scroll
    // position and checklist ticks are not disturbed by every poll.
    const sig = JSON.stringify([item, drafterMode(), isDrafting(item), S.locked]);
    if (sig !== S.detailSig) {
      S.detailSig = sig;
      renderDetailStatic(item);
    }
    syncEditor(item);
    updateEditorUI();
  }

  function renderDetailStatic(item) {
    $('d-title').textContent = item.title || '(untitled)';
    const badge = $('d-state');
    badge.textContent = STATE_LABELS[item.state] || item.state;
    badge.className = 'badge state-' + item.state;
    $('d-reason').textContent = item.reason || '';

    const notes = [];
    if (item.state === 'send_unknown') notes.push(el('div', 'notice error', SEND_UNKNOWN));
    if (item.state === 'sent') notes.push(el('div', 'notice ok', 'Sent.'));
    if (item.state === 'dismissed') notes.push(el('div', 'notice info', 'Dismissed.'));
    if (item.state === 'snoozed') {
      notes.push(el('div', 'notice info', 'Snoozed' + (item.snooze_until ? ' until ' + fmtTime(item.snooze_until) : '') + '.'));
    }
    if (item.error) notes.push(el('div', 'notice error', item.error));
    if (item.in_thread) notes.push(el('div', 'notice info', 'These messages are in a thread - your reply goes to the thread.'));
    $('d-notices').replaceChildren(...notes);

    $('d-flags').replaceChildren(...(item.flags || []).map((f) => el('li', null, FLAG_TEXT[f] || f)));

    renderAsk(item);
    renderConvo(item);
    renderCited(item);
  }

  function renderAsk(item) {
    const d = item.draft;
    const nodes = [];
    if (!d) {
      let msg = 'No draft yet.';
      if (isDrafting(item)) msg = 'Drafting...';
      else if (item.state === 'draft_failed') msg = 'The draft failed. Regenerate, or write the reply yourself.';
      else if (drafterMode() === 'off') msg = 'Drafting unavailable - read the conversation below and write your reply.';
      nodes.push(el('p', 'muted', msg));
      if (canDraft() && !isDrafting(item) && canRegen(item.state)) {
        nodes.push(button('Draft now', () => doDraft(item.room_id, '')));
      }
      $('d-ask-body').replaceChildren(...nodes);
      return;
    }

    if (d.needs_reply === false) {
      const box = el('div', 'notice info');
      box.append(el('strong', null, 'Probably no reply needed. '));
      box.append(document.createTextNode(d.needs_reply_reason || ''));
      if (canDraft() && !isDrafting(item) && canRegen(item.state)) {
        box.append(button('Draft anyway', () => doDraft(item.room_id, DRAFT_ANYWAY)));
      }
      nodes.push(box);
    }
    if (d.summary) nodes.push(el('p', 'summary', d.summary));

    const asks = d.asks || [];
    if (asks.length) {
      nodes.push(el('h4', null, 'Asks'));
      const ul = el('ul', 'checklist');
      for (const a of asks) {
        const key = item.room_id + '\n' + a;
        const label = el('label');
        const cb = el('input');
        cb.type = 'checkbox';
        cb.checked = S.checked.has(key);
        cb.addEventListener('change', () => {
          if (cb.checked) S.checked.add(key);
          else S.checked.delete(key);
        });
        label.append(cb, el('span', null, a));
        const li = el('li');
        li.append(label);
        ul.append(li);
      }
      nodes.push(ul);
    }

    const missing = d.missing_info || [];
    if (missing.length) {
      nodes.push(el('h4', null, 'Information you need to supply'));
      const ul = el('ul', 'missing');
      for (const m of missing) ul.append(el('li', null, m));
      nodes.push(ul);
    }

    let meta = 'Drafted ' + relTime(d.created_at, Date.now());
    if (d.mode) meta += ' (' + d.mode + ')';
    if (d.instruction) meta += ' - instruction: "' + d.instruction + '"';
    nodes.push(el('p', 'hint draft-meta', meta));
    $('d-ask-body').replaceChildren(...nodes);
  }

  function renderConvo(item) {
    const nodes = [];
    for (const m of item.context || []) nodes.push(msgNode(m, item, 'ctx'));
    const burst = item.burst || [];
    if (burst.length) {
      nodes.push(el('li', 'divider', 'Unanswered' + (item.since ? ' since ' + fmtTime(item.since) : '')));
    }
    for (const m of burst) nodes.push(msgNode(m, item, 'burst'));
    $('d-convo').replaceChildren(...nodes);
  }

  function msgNode(m, item, kind) {
    const li = el('li', 'msg msg-' + kind + (m.from_me ? ' from-me' : ''));
    const head = el('div', 'msg-head');
    head.append(el('span', 'author', m.from_me ? 'You' : (m.author || item.title || 'Them')));
    const t = el('time', 'msg-time', fmtTime(m.created));
    if (m.created) t.dateTime = m.created;
    head.append(t);
    if (m.in_thread) head.append(el('span', 'chip', 'thread'));
    if (m.files > 0) head.append(el('span', 'chip', m.files + (m.files === 1 ? ' file' : ' files')));
    if (m.card) head.append(el('span', 'chip', 'card'));
    if (m.voice_clip) head.append(el('span', 'chip', 'voice clip'));
    if (m.edited) head.append(el('span', 'chip', 'edited'));
    li.append(head);
    const body = el('div', 'msg-text', m.text || '(no text)');
    if (!m.text) body.classList.add('empty-text');
    li.append(body);
    return li;
  }

  function renderCited(item) {
    const d = item.draft;
    $('d-cited-sec').hidden = !d;
    if (!d) return;
    const nodes = [];
    if (d.no_prior_answer) nodes.push(el('div', 'notice warn', 'No prior answer found in your messages'));
    const cited = d.cited || [];
    if (cited.length) {
      const ul = el('ul', 'cited');
      for (const c of cited) {
        const li = el('li');
        const t = el('time', null, fmtDate(c.created));
        if (c.created) t.dateTime = c.created;
        li.append(t, el('div', 'msg-text', c.text || ''));
        ul.append(li);
      }
      nodes.push(ul);
    } else if (!d.no_prior_answer) {
      nodes.push(el('p', 'muted', 'The draft did not cite any of your earlier messages.'));
    }
    $('d-cited').replaceChildren(...nodes);
  }

  /** Put the right text in the textarea without clobbering user edits. */
  function syncEditor(item) {
    const id = item.room_id;
    const ed = reconcileEditor(S.editors.get(id), item.draft);
    S.editors.set(id, ed);
    const ta = $('editor');
    if (S.editorRoom !== id) {
      S.editorRoom = id;
      ta.value = ed.text;
      $('regen-input').value = '';
    } else if (ta.value !== ed.text) {
      ta.value = ed.text;
    }
  }

  /** Counter, placeholder count, banners, and button states. Cheap; runs on every keystroke. */
  function updateEditorUI() {
    const item = selectedItem();
    if (!item) return;
    const ed = S.editors.get(item.room_id);
    const ta = $('editor');
    const text = ta.value;
    const max = maxLen();
    const n = byteLen(text);
    const over = n > max;
    const fills = countFills(text);
    const done = isDone(item.state);
    const drafting = isDrafting(item);

    const bytes = $('d-bytes');
    bytes.textContent = n + ' / ' + max + ' bytes';
    bytes.classList.toggle('over', over);
    ta.classList.toggle('over', over);
    ta.classList.toggle('has-fill', fills > 0);
    ta.readOnly = done;

    $('d-fills').hidden = fills === 0;
    $('d-fills').textContent = fills === 1 ? '1 [[FILL: ...]] placeholder to replace' : fills + ' [[FILL: ...]] placeholders to replace';
    $('btn-nextfill').hidden = fills === 0;

    $('d-stale').hidden = !item.draft_stale;
    $('d-newdraft').hidden = !(ed && ed.offerKey);
    $('d-drafting').hidden = !drafting;

    // The server limit is in bytes; maxlength would count UTF-16 units.
    const instrBytes = byteLen($('regen-input').value.trim());
    const instrOver = instrBytes > MAX_INSTRUCTION_BYTES;
    const rb = $('regen-bytes');
    rb.hidden = instrBytes === 0;
    rb.textContent = instrBytes + ' / ' + MAX_INSTRUCTION_BYTES + ' bytes' +
      (instrOver ? ' - over the limit by ' + (instrBytes - MAX_INSTRUCTION_BYTES) : '');
    rb.classList.toggle('over', instrOver);
    $('regen-input').classList.toggle('over', instrOver);

    if (S.locked) return;
    const why = sendBlockReason(text, max, item.state);
    $('btn-send').disabled = !!why;
    $('d-send-why').textContent = why;
    const regenAllowed = canDraft() && canRegen(item.state);
    $('btn-regen').disabled = !regenAllowed || drafting || instrOver;
    $('regen-input').disabled = !regenAllowed;
    $('btn-dismiss').disabled = done;
    $('btn-snooze').disabled = done;
  }

  // ---------------------------------------------------------------------
  // Toasts
  // ---------------------------------------------------------------------

  function toast(msg, kind) {
    const t = el('div', 'toast' + (kind ? ' toast-' + kind : ''), msg);
    $('toasts').append(t);
    setTimeout(() => t.remove(), kind === 'error' ? 7000 : 3500);
  }

  function toastError(prefix, res) {
    if (S.locked || res.status === 401 || (res.error && res.error.code === 'locked')) return;
    const e = res.error || {};
    let msg = prefix;
    const fixed = codeMessage(e.code);
    if (e.code === 'network') msg += ': cannot reach Reply Desk';
    else if (fixed) msg = fixed;
    else if (e.detail) msg += ': ' + e.detail;
    else if (res.status) msg += ' (HTTP ' + res.status + ')';
    toast(msg, 'error');
  }

  // ---------------------------------------------------------------------
  // Actions
  // ---------------------------------------------------------------------

  function select(roomId, explicit) {
    closeMenus(false);
    S.selected = roomId;
    const flat = flatOrder(groupItems(S.items, S.showAll));
    const i = flat.findIndex((x) => x.room_id === roomId);
    if (i >= 0) S.selectedIndex = i;
    renderList();
    renderDetail();
    const row = document.querySelector('.row[aria-current="true"]');
    if (row) row.scrollIntoView({ block: 'nearest' });
    $('detail').parentElement.scrollTop = 0;
    if (explicit) maybeAutoDraft(findItem(roomId));
  }

  /** ADR 0002: items past the first few are drafted when opened. */
  function maybeAutoDraft(item) {
    if (!item || item.draft || item.state !== 'pending' || !canDraft()) return;
    if (S.autoDrafted.has(item.room_id)) return;
    S.autoDrafted.add(item.room_id);
    doDraft(item.room_id, '', true);
  }

  function move(delta) {
    const flat = flatOrder(groupItems(S.items, S.showAll));
    if (!flat.length) return;
    let i = flat.findIndex((x) => x.room_id === S.selected);
    i = i < 0 ? 0 : Math.max(0, Math.min(flat.length - 1, i + delta));
    const rowFocused = document.activeElement && document.activeElement.classList.contains('row');
    select(flat[i].room_id, true);
    if (rowFocused) {
      const row = document.querySelector('.row[aria-current="true"]');
      if (row) row.focus();
    }
  }

  async function doDraft(roomId, instruction, quiet) {
    if (!canDraft()) return;
    const item = findItem(roomId);
    if (!item || !canRegen(item.state)) return;
    if (byteLen(instruction) > MAX_INSTRUCTION_BYTES) {
      toast('The instruction is over ' + MAX_INSTRUCTION_BYTES + ' bytes - shorten it', 'error');
      return;
    }
    const res = await api('POST', '/api/draft', { room_id: roomId, instruction: instruction || '' });
    if (!res.ok) {
      if (!quiet) toastError('Could not start drafting', res);
      return;
    }
    const now = findItem(roomId) || item;
    S.regenPending.set(roomId, { state: now.state, key: draftKey(now.draft), at: Date.now() });
    S.detailSig = '';
    renderList();
    renderDetail();
    refresh();
  }

  function doRegenerate() {
    const item = selectedItem();
    if (!item) return;
    doDraft(item.room_id, $('regen-input').value.trim());
  }

  /** Drop an item locally after it was sent or dismissed, then pick a neighbour. */
  function removeLocally(roomId, newState) {
    if (S.showAll) {
      const it = findItem(roomId);
      if (it) it.state = newState;
    } else {
      S.items = S.items.filter((it) => it.room_id !== roomId);
    }
    S.detailSig = '';
    fixSelection();
    render();
  }

  async function doDismiss(reason, roomId) {
    closeMenus(false);
    const id = roomId || S.selected;
    if (!id) return;
    const res = await api('POST', '/api/dismiss', { room_id: id, reason: reason });
    if (!res.ok) {
      toastError('Could not dismiss', res);
      return;
    }
    toast('Dismissed: ' + (DISMISS_LABELS[reason] || reason));
    removeLocally(id, 'dismissed');
    refresh();
  }

  async function doSnooze(hours) {
    closeMenus(false);
    const id = S.selected;
    if (!id) return;
    const res = await api('POST', '/api/snooze', { room_id: id, hours: hours });
    if (!res.ok) {
      toastError('Could not snooze', res);
      return;
    }
    toast('Snoozed for ' + (hours === 1 ? '1 hour' : hours + ' hours'));
    const it = findItem(id);
    if (it) it.state = 'snoozed';
    S.detailSig = '';
    render();
    refresh();
  }

  async function doRescan() {
    const res = await api('POST', '/api/scan', {});
    if (!res.ok) {
      toastError('Could not start a scan', res);
      return;
    }
    S.status = Object.assign({}, S.status, { scanning: true });
    renderStatus();
    refresh();
  }

  async function doClose() {
    const unsent = [...S.editors.values()].some((ed) => ed.text !== ed.baseText);
    if (unsent && !window.confirm('Close Reply Desk? Unsent edits will be lost.')) return;
    const res = await api('POST', '/api/close', {});
    if (!res.ok && res.status !== 0) {
      toastError('Could not close', res);
      return;
    }
    lock(CLOSED);
  }

  async function doCopy() {
    const ta = $('editor');
    if (!ta.value) {
      toast('Nothing to copy');
      return;
    }
    try {
      await navigator.clipboard.writeText(ta.value);
      toast('Copied');
    } catch (e) {
      ta.focus();
      ta.select();
      toast('Copy was blocked - press Ctrl+C to copy the selected text', 'error');
    }
  }

  function nextFill() {
    const ta = $('editor');
    const r = nextFillRange(ta.value, ta.selectionEnd);
    if (!r) return;
    ta.focus();
    ta.setSelectionRange(r[0], r[1]);
  }

  function replaceDraft(take) {
    const item = selectedItem();
    if (!item) return;
    const ed = S.editors.get(item.room_id);
    if (!ed || !ed.offerKey) return;
    const text = item.draft ? item.draft.text || '' : '';
    const next = { text: take ? text : ed.text, baseKey: ed.offerKey, baseText: text, offerKey: '' };
    S.editors.set(item.room_id, next);
    $('editor').value = next.text;
    updateEditorUI();
    if (take) $('editor').focus();
  }

  // ---------------------------------------------------------------------
  // Menus
  // ---------------------------------------------------------------------

  function openMenu(name) {
    const btn = $('btn-' + name);
    if (S.locked || btn.disabled || !selectedItem()) return;
    closeMenus(false);
    const menu = $('menu-' + name);
    menu.hidden = false;
    btn.setAttribute('aria-expanded', 'true');
    const first = menu.querySelector('[role="menuitem"]');
    if (first) first.focus();
  }

  function closeMenus(returnFocus) {
    for (const name of ['dismiss', 'snooze']) {
      const menu = $('menu-' + name);
      if (menu.hidden) continue;
      menu.hidden = true;
      $('btn-' + name).setAttribute('aria-expanded', 'false');
      if (returnFocus) $('btn-' + name).focus();
    }
  }

  function menuKeys(e) {
    const items = [...e.currentTarget.querySelectorAll('[role="menuitem"]')];
    const i = items.indexOf(document.activeElement);
    let next = -1;
    if (e.key === 'ArrowDown') next = (i + 1) % items.length;
    else if (e.key === 'ArrowUp') next = (i - 1 + items.length) % items.length;
    else if (e.key === 'Home') next = 0;
    else if (e.key === 'End') next = items.length - 1;
    else if (e.key === 'Tab') closeMenus(false);
    if (next >= 0) {
      e.preventDefault();
      items[next].focus();
    }
  }

  // ---------------------------------------------------------------------
  // Confirm and send
  // ---------------------------------------------------------------------

  function openConfirm() {
    const item = selectedItem();
    if (!item || S.locked) return;
    const text = $('editor').value;
    const why = sendBlockReason(text, maxLen(), item.state);
    if (why) {
      toast(why, 'error');
      return;
    }
    closeMenus(false);
    // head_id ties the send to the burst on screen now; if the other person
    // wrote again since, the server refuses with new_messages.
    S.confirm = {
      roomId: item.room_id, headId: item.head_id || '', title: item.title || '', text: text,
      sending: false, lost: false, openedAt: Date.now(),
    };
    $('confirm-to').textContent = item.title || '(untitled)';
    $('confirm-thread').textContent = item.in_thread ? '(in the thread)' : '';
    showConfirmText(text);
    $('confirm-error').hidden = true;
    $('confirm-state').hidden = true;
    $('confirm-compare').hidden = true;
    $('confirm-extra').replaceChildren();
    setConfirmSending(false, true);
    $('dlg-confirm').showModal();
    // Focus the text, not the Send button, so a held Ctrl+Enter cannot send.
    $('confirm-text').focus();
  }

  function showConfirmText(text) {
    $('confirm-text').textContent = text;
    $('confirm-bytes').textContent = byteLen(text) + ' / ' + maxLen() + ' bytes. This exact text will be sent.';
  }

  function setConfirmSending(sending, sendEnabled) {
    const send = $('confirm-send');
    send.textContent = sending ? 'Sending...' : 'Send';
    send.disabled = sending || !sendEnabled;
    $('confirm-cancel').disabled = sending;
    for (const b of $('confirm-extra').querySelectorAll('button')) b.disabled = sending;
  }

  async function confirmSend(text, ackSanitized) {
    const c = S.confirm;
    if (!c || c.sending || Date.now() - c.openedAt < CONFIRM_GUARD_MS) return;
    c.sending = true;
    setConfirmSending(true, false);
    let hash;
    try {
      hash = await sha256Hex(text);
    } catch (e) {
      c.sending = false;
      setConfirmSending(false, false);
      showConfirmError({ code: 'hash', detail: '' }, 0, 'This browser could not hash the text, so it cannot be sent safely.');
      return;
    }
    const res = await api('POST', '/api/send', {
      room_id: c.roomId, head_id: c.headId, text: text, text_sha256: hash, ack_sanitized: !!ackSanitized,
    });
    c.sending = false;
    if (S.confirm !== c) return;
    if (res.status === 0 && res.error && res.error.code === 'network') {
      // The connection dropped mid-send: the message may or may not be out.
      // No retry button; show what the server recorded instead.
      c.lost = true;
      showConfirmError(res.error, 0);
      updateConfirmState();
      refresh();
      return;
    }
    if (res.ok) {
      $('dlg-confirm').close();
      S.editors.delete(c.roomId);
      if (S.editorRoom === c.roomId) S.editorRoom = null;
      toast('Sent');
      removeLocally(c.roomId, 'sent');
      refresh();
      return;
    }
    if (res.status === 401) return;
    showConfirmError(res.error, res.status);
    if (!(res.error && res.error.code === 'sanitized')) refresh();
  }

  function showConfirmError(err, status, override) {
    const c = S.confirm;
    const info = describeError(err, status);
    const box = $('confirm-error');
    box.replaceChildren(document.createTextNode(override || info.message));
    if (err && err.detail && err.detail !== info.message) box.append(el('span', 'detail-line', err.detail));
    box.hidden = false;

    const extra = [];
    $('confirm-compare').hidden = true;
    for (const a of info.actions) {
      if (a === 'dismiss_replied') {
        extra.push(button('Dismiss as replied elsewhere', () => {
          $('dlg-confirm').close();
          doDismiss('replied_elsewhere', c.roomId);
        }));
      } else if (a === 'rescan') {
        extra.push(button('Rescan', () => {
          $('dlg-confirm').close();
          doRescan();
        }));
      } else if (a === 'refresh') {
        extra.push(button('Refresh', () => {
          $('dlg-confirm').close();
          refresh();
        }));
      } else if (a === 'send_sanitized') {
        $('cmp-orig').textContent = c.text;
        $('cmp-san').textContent = err.sanitized_text;
        $('confirm-compare').hidden = false;
        const sanitized = err.sanitized_text;
        extra.push(button('Send sanitized version', () => confirmSend(sanitized, true), 'primary'));
      }
    }
    $('confirm-extra').replaceChildren(...extra);
    // Retrying the same text only makes sense for transient errors.
    setConfirmSending(false, info.retry && !override);
    $('confirm-send').hidden = info.actions.indexOf('send_sanitized') >= 0;
  }

  /**
   * After a send lost its connection, show what the server now says about
   * the room, so the user can tell "sent" from "not sent" before retrying.
   * Runs on every render, so the line follows each poll.
   */
  function updateConfirmState() {
    const box = $('confirm-state');
    const c = S.confirm;
    if (!c || !c.lost) {
      box.hidden = true;
      return;
    }
    const item = findItem(c.roomId);
    let msg;
    if (S.offline) {
      msg = 'Reply Desk is not answering, so its state is unknown. Check the conversation in Webex before sending again.';
    } else if (!item) {
      msg = 'This conversation has left the inbox, so the reply was most likely sent. Check Webex, or tick "Show sent & dismissed".';
    } else {
      const label = STATE_LABELS[item.state] || item.state;
      switch (item.state) {
        case 'sent':
          msg = 'State now: ' + label + '. The reply went out - do not send it again.';
          break;
        case 'sending':
          msg = 'State now: ' + label + '. Reply Desk is still sending - wait for the result.';
          break;
        case 'send_unknown':
          msg = 'State now: ' + label + '. ' + SEND_UNKNOWN + '.';
          break;
        default:
          msg = 'State now: ' + label + '. Reply Desk has no record of this reply going out. Check Webex, then cancel and send again if it is missing.';
      }
    }
    box.textContent = msg;
    box.hidden = false;
  }

  // ---------------------------------------------------------------------
  // Keyboard
  // ---------------------------------------------------------------------

  function onKey(e) {
    if (anyDialogOpen()) return; // <dialog> handles Escape itself
    if (e.repeat && e.key === 'Enter') return;

    if (e.key === 'Escape') {
      if (anyMenuOpen()) {
        e.preventDefault();
        closeMenus(true);
      } else if (isTypingTarget(e.target)) {
        e.target.blur();
      }
      return;
    }
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      openConfirm();
      return;
    }
    if (isTypingTarget(e.target) || e.ctrlKey || e.metaKey || e.altKey) return;
    if (anyMenuOpen()) return; // menuKeys handles navigation

    switch (e.key) {
      case 'j':
        move(1);
        break;
      case 'k':
        move(-1);
        break;
      case 'e':
        if (!selectedItem()) return;
        $('editor').focus();
        break;
      case 'r':
        if (!selectedItem() || $('regen-input').disabled) return;
        $('regen-input').focus();
        break;
      case 'd':
        openMenu('dismiss');
        break;
      case 's':
        openMenu('snooze');
        break;
      case '?':
        $('dlg-help').showModal();
        break;
      default:
        return;
    }
    e.preventDefault();
  }

  // ---------------------------------------------------------------------
  // Wiring
  // ---------------------------------------------------------------------

  function init() {
    csrf = loadCSRF();
    if (!csrf) {
      // Opened by URL or bookmark instead of the launch link: without the
      // token every /api/ call would be refused.
      lock(OPEN_FROM_LINK);
      return;
    }

    $('btn-rescan').addEventListener('click', doRescan);
    $('btn-close').addEventListener('click', doClose);
    $('btn-help').addEventListener('click', () => $('dlg-help').showModal());
    $('help-close').addEventListener('click', () => $('dlg-help').close());

    $('chk-all').addEventListener('change', (e) => {
      S.showAll = e.target.checked;
      refresh();
    });

    $('editor').addEventListener('input', () => {
      const item = selectedItem();
      if (!item) return;
      const ed = S.editors.get(item.room_id) || reconcileEditor(undefined, item.draft);
      S.editors.set(item.room_id, Object.assign({}, ed, { text: $('editor').value }));
      updateEditorUI();
    });
    $('btn-nextfill').addEventListener('click', nextFill);
    $('btn-replace').addEventListener('click', () => replaceDraft(true));
    $('btn-keep').addEventListener('click', () => replaceDraft(false));

    $('btn-regen').addEventListener('click', doRegenerate);
    $('regen-input').addEventListener('input', updateEditorUI);
    $('regen-input').addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.ctrlKey && !e.metaKey && !$('btn-regen').disabled) {
        e.preventDefault();
        doRegenerate();
      }
    });

    $('btn-send').addEventListener('click', openConfirm);
    $('btn-copy').addEventListener('click', doCopy);

    for (const name of ['dismiss', 'snooze']) {
      $('btn-' + name).addEventListener('click', () => {
        if ($('menu-' + name).hidden) openMenu(name);
        else closeMenus(true);
      });
      $('menu-' + name).addEventListener('keydown', menuKeys);
    }
    for (const b of $('menu-dismiss').querySelectorAll('[data-reason]')) {
      b.addEventListener('click', () => doDismiss(b.dataset.reason));
    }
    for (const b of $('menu-snooze').querySelectorAll('[data-hours]')) {
      b.addEventListener('click', () => doSnooze(parseInt(b.dataset.hours, 10)));
    }
    document.addEventListener('click', (e) => {
      if (!e.target.closest('.menu-wrap')) closeMenus(false);
    });

    $('confirm-cancel').addEventListener('click', () => $('dlg-confirm').close());
    $('confirm-send').addEventListener('click', () => {
      if (S.confirm) confirmSend(S.confirm.text, false);
    });
    $('dlg-confirm').addEventListener('cancel', (e) => {
      if (S.confirm && S.confirm.sending) e.preventDefault();
    });
    $('dlg-confirm').addEventListener('close', () => {
      S.confirm = null;
      $('confirm-send').hidden = false;
      if (!S.locked) $('btn-send').focus();
    });

    document.addEventListener('keydown', onKey);
    // A hidden tab stops polling (schedule checks visibility); showing it
    // again refreshes at once.
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible') refresh();
      else clearTimeout(S.timer);
    });

    refresh();
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
