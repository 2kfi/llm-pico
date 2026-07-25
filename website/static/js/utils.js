window.llmPico = window.llmPico || {};

(function(ns) {
  const $ = (s, p) => (p || document).querySelector(s);
  const $$ = (s, p) => [...(p || document).querySelectorAll(s)];

  function el(tag, attrs, children) {
    const e = document.createElement(tag);
    if (attrs) Object.entries(attrs).forEach(([k, v]) => {
      if (k === 'className') e.className = v;
      else if (k.startsWith('on')) e.addEventListener(k.slice(2).toLowerCase(), v);
      else if (k === 'html') e.innerHTML = v;
      else e.setAttribute(k, v);
    });
    if (children != null) {
      (Array.isArray(children) ? children : [children]).forEach(c => {
        if (c == null) return;
        e.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
      });
    }
    return e;
  }

  /* Toast — timing: info=4s, warning=7s, error=persistent, max 3, pause on hover */
  const TOAST_TIMING = { info: 4000, success: 4000, warning: 7000, error: 0 };
  const MAX_TOASTS = 3;

  function showToast(message, type) {
    type = type || 'info';
    let c = $('#toasts');
    if (!c) { c = document.createElement('div'); c.id = 'toasts'; c.className = 'lp-toast-container'; c.setAttribute('role', 'status'); c.setAttribute('aria-live', 'polite'); document.body.appendChild(c); }
    while (c.children.length >= MAX_TOASTS) c.firstChild.remove();
    var toastRole = (type === 'error' || type === 'warning') ? 'alert' : 'status';
    const toast = el('div', { className: 'lp-toast lp-toast-' + type, 'role': toastRole }, [
      el('span', null, message),
      el('button', { className: 'lp-toast-dismiss', 'aria-label': 'Dismiss', onClick: () => toast.remove() }, '\u00d7')
    ]);
    c.appendChild(toast);
    const duration = TOAST_TIMING[type] || 4000;
    if (duration > 0) {
      let timer = setTimeout(() => { if (toast.parentNode) toast.remove(); }, duration);
      toast.addEventListener('mouseenter', () => clearTimeout(timer));
      toast.addEventListener('mouseleave', () => { timer = setTimeout(() => { if (toast.parentNode) toast.remove(); }, 2000); });
    }
  }

  function showModal(title, bodyHtml, onConfirm) {
    const overlay = $('#modal-overlay');
    overlay.className = 'lp-modal-overlay';
    overlay.innerHTML = '';
    overlay.setAttribute('aria-label', title);
    const confirmBtn = el('button', { className: 'lp-btn lp-btn-primary', id: 'modal-confirm' }, 'Confirm');
    const modal = el('div', { className: 'lp-modal' }, [
      el('div', { className: 'lp-modal-header' }, [
        el('span', { className: 'lp-modal-title', id: 'lp-modal-title' }, title),
        el('button', { className: 'lp-modal-close', 'aria-label': 'Close', onClick: closeModal }, '\u00d7')
      ]),
      el('div', { className: 'lp-modal-body', html: bodyHtml }),
      el('div', { className: 'lp-modal-footer' }, [
        el('button', { className: 'lp-btn', id: 'modal-cancel' }, 'Cancel'),
        confirmBtn
      ])
    ]);
    overlay.appendChild(modal);
    $('#modal-cancel').addEventListener('click', closeModal);
    if (onConfirm) {
      confirmBtn.addEventListener('click', async function() {
        confirmBtn.disabled = true;
        confirmBtn.textContent = 'Saving...';
        try {
          await onConfirm();
        } catch (e) {
          showToast(e.message || 'Something went wrong', 'error');
        } finally {
          closeModal();
        }
      });
    }
    overlay.addEventListener('click', (e) => { if (e.target === overlay) closeModal(); });

    // Focus trap
    var previouslyFocused = document.activeElement;
    confirmBtn.focus();

    function trapFocus(e) {
      if (e.key !== 'Tab') return;
      var focusable = modal.querySelectorAll('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])');
      if (!focusable.length) return;
      var first = focusable[0];
      var last = focusable[focusable.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }

    document.addEventListener('keydown', trapFocus);
    document.addEventListener('keydown', function esc(e) {
      if (e.key === 'Escape') { closeModal(); document.removeEventListener('keydown', esc); document.removeEventListener('keydown', trapFocus); }
    });

    overlay._cleanupFocus = function() {
      document.removeEventListener('keydown', trapFocus);
      if (previouslyFocused && previouslyFocused.focus) previouslyFocused.focus();
    };
  }

  function closeModal() {
    const overlay = $('#modal-overlay');
    if (overlay._cleanupFocus) overlay._cleanupFocus();
    overlay.className = 'hidden';
    overlay.innerHTML = '';
  }

  function debounce(fn, ms) {
    let t;
    return function() {
      clearTimeout(t);
      t = setTimeout(() => fn.apply(this, arguments), ms);
    };
  }

  function formatDate(iso) {
    if (!iso) return '-';
    return new Date(iso).toLocaleDateString('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  }

  function formatNumber(n) {
    if (n == null) return '-';
    return Number(n).toLocaleString();
  }

  function formatCost(usd) {
    if (usd == null) return '-';
    return '$' + Number(usd).toFixed(2);
  }

  function escapeHtml(s) {
    const d = document.createElement('div');
    d.textContent = s;
    return d.innerHTML;
  }

  function sleep(ms) {
    return new Promise(r => setTimeout(r, ms));
  }

  function capBadgeClass(cap) {
    return 'lp-badge lp-capability-' + cap.toLowerCase();
  }

  function capBadgeHtml(cap) {
    return '<span class="' + capBadgeClass(cap) + '">' + escapeHtml(cap) + '</span>';
  }

  function spinnerHtml(size) {
    return '<span class="lp-spinner' + (size === 'lg' ? ' lp-spinner-lg' : '') + '" role="status"><span class="sr-only">Loading...</span></span>';
  }

  function skeletonHtml(lines) {
    lines = lines || 4;
    let html = '<div class="lp-skeleton">';
    for (let i = 0; i < lines; i++) html += '<div class="lp-skeleton-line"></div>';
    html += '</div>';
    return html;
  }

  function skeleton(type, count) {
    count = count || 3;
    if (type === 'stat') {
      return '<div class="lp-dashboard-stat">' +
        '<div class="skeleton" style="width:60px;height:14px;border-radius:4px"></div>' +
        '<div class="skeleton" style="width:80px;height:32px;margin-top:12px;border-radius:6px"></div>' +
        '<div class="skeleton" style="width:100px;height:12px;margin-top:8px;border-radius:4px"></div></div>';
    }
    if (type === 'rows') {
      var html = '';
      for (var i = 0; i < count; i++) {
        html += '<div style="display:flex;gap:12px;padding:12px 0;border-bottom:1px solid var(--lp-border)">' +
          '<div class="skeleton" style="width:100%;height:16px;border-radius:4px"></div></div>';
      }
      return html;
    }
    if (type === 'card') {
      return '<div class="lp-card"><div class="lp-card-header"><div class="skeleton" style="width:120px;height:18px;border-radius:4px"></div></div>' +
        '<div class="lp-card-body"><div class="skeleton" style="width:100%;height:80px;border-radius:6px"></div></div></div>';
    }
    if (type === 'table') {
      var cols = count || 4;
      var html = '<div class="lp-table-wrap"><table class="lp-table"><thead><tr>';
      for (var i = 0; i < cols; i++) html += '<th><div class="skeleton" style="width:80px;height:14px;border-radius:4px"></div></th>';
      html += '</tr></thead><tbody>';
      for (var r = 0; r < 5; r++) {
        html += '<tr>';
        for (var c = 0; c < cols; c++) {
          html += '<td><div class="skeleton" style="width:' + (60 + Math.random() * 40) + '%;height:14px;border-radius:4px"></div></td>';
        }
        html += '</tr>';
      }
      html += '</tbody></table></div>';
      return html;
    }
    var html = '<div style="display:flex;flex-direction:column;gap:8px">';
    for (var i = 0; i < count; i++) {
      html += '<div class="skeleton" style="width:' + (70 + Math.random() * 30) + '%;height:14px;border-radius:4px"></div>';
    }
    return html + '</div>';
  }

  function emptyState(title, text) {
    return '<div class="lp-empty">' +
      '<div class="lp-empty-title">' + escapeHtml(title) + '</div>' +
      (text ? '<div class="lp-empty-text">' + escapeHtml(text) + '</div>' : '') +
      '</div>';
  }

  function announceToScreenReader(msg) {
    let region = $('#lp-sr-announce');
    if (!region) {
      region = document.createElement('div');
      region.id = 'lp-sr-announce';
      region.setAttribute('role', 'status');
      region.setAttribute('aria-live', 'polite');
      region.className = 'sr-only';
      document.body.appendChild(region);
    }
    region.textContent = msg;
  }

  function timeAgo(date) {
    if (!date) return '-';
    var now = Date.now();
    var d = new Date(date).getTime();
    var diff = Math.floor((now - d) / 1000);
    if (diff < 60) return 'just now';
    if (diff < 3600) return Math.floor(diff / 60) + 'm ago';
    if (diff < 86400) return Math.floor(diff / 3600) + 'h ago';
    if (diff < 604800) return Math.floor(diff / 86400) + 'd ago';
    return new Date(date).toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
  }

  function formatDateShort(iso) {
    if (!iso) return '-';
    return new Date(iso).toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
  }

  function formatPercent(value, total) {
    if (!total) return '0%';
    return (value / total * 100).toFixed(1) + '%';
  }

  function copyToClipboard(text) {
    if (navigator.clipboard) {
      navigator.clipboard.writeText(text).then(function() {
        showToast('Copied to clipboard', 'success');
      }).catch(function() {
        fallbackCopy(text);
      });
    } else {
      fallbackCopy(text);
    }
  }

  function fallbackCopy(text) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.left = '-9999px';
    document.body.appendChild(ta);
    ta.select();
    var ok = document.execCommand('copy');
    document.body.removeChild(ta);
    if (ok) {
      showToast('Copied to clipboard', 'success');
    } else {
      showToast('Failed to copy', 'error');
    }
  }

  function truncate(str, maxLen) {
    if (!str) return '';
    return str.length > maxLen ? str.slice(0, maxLen) + '...' : str;
  }

  function clamp(value, min, max) {
    return Math.min(Math.max(value, min), max);
  }

  Object.assign(ns, {
    $, $$, el, showToast, showModal, closeModal, debounce,
    formatDate, formatDateShort, formatNumber, formatCost, formatPercent,
    escapeHtml, sleep, copyToClipboard, truncate, clamp, timeAgo,
    capBadgeHtml, capBadgeClass, spinnerHtml, skeletonHtml, skeleton, emptyState,
    announceToScreenReader
  });
})(window.llmPico);
