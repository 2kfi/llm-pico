window.llmPico = window.llmPico || {};

(function(ns) {
  var $ = ns.$, api = ns.api, showToast = ns.showToast, showModal = ns.showModal;
  var escapeHtml = ns.escapeHtml, formatNumber = ns.formatNumber, formatCost = ns.formatCost;
  var formatDate = ns.formatDate, spinnerHtml = ns.spinnerHtml, emptyState = ns.emptyState;

  function maskKey(k) {
    var raw = k.full_key || k.key || k.raw || '';
    var prefix = k.prefix || k.key_prefix || '';
    if (prefix) return prefix + '****';
    if (raw) return raw.slice(0, 8) + '****' + raw.slice(-4);
    return '****';
  }

  function modelBadgeHtml(m) {
    var cap = (m || 'text').toLowerCase();
    return '<span class="lp-badge lp-capability-' + cap + '">' + escapeHtml(m) + '</span>';
  }

  function modelsHtml(models) {
    if (!models || !models.length) return '<span class="lp-text-muted">All</span>';
    var html = '<div class="lp-flex lp-flex-wrap lp-gap-xs">';
    for (var i = 0; i < models.length; i++) html += modelBadgeHtml(models[i]);
    return html + '</div>';
  }

  function formatRateLimit(rpm) {
    if (!rpm) return '<span class="lp-text-muted">Unlimited</span>';
    return formatNumber(rpm) + ' <span class="lp-text-muted">RPM</span>';
  }

  function formatBudget(budget) {
    if (!budget) return '<span class="lp-text-muted">Unlimited</span>';
    return formatCost(budget);
  }

  function timeAgo(iso) {
    if (!iso) return '-';
    var s = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
    if (s < 60) return 'Just now';
    if (s < 3600) return Math.floor(s / 60) + 'm ago';
    if (s < 86400) return Math.floor(s / 3600) + 'h ago';
    if (s < 2592000) return Math.floor(s / 86400) + 'd ago';
    return formatDate(iso);
  }

  var keys = {
    render: function(container) {
      container.innerHTML =
        '<div class="lp-page-header">' +
          '<div>' +
            '<h1 class="lp-page-title">API Keys</h1>' +
            '<div class="lp-page-subtitle">Manage API keys for programmatic access</div>' +
          '</div>' +
          '<button class="lp-btn lp-btn-primary" id="add-key-btn">+ Create Key</button>' +
        '</div>' +
        '<div class="lp-table-toolbar">' +
          '<div class="lp-search-input">' +
            '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><circle cx="11" cy="11" r="8"/><line x1="21" y1="21" x2="16.65" y2="16.65"/></svg>' +
            '<label for="keys-search" class="sr-only">Search keys</label>' +
            '<input class="lp-input" type="text" id="keys-search" placeholder="Search keys...">' +
          '</div>' +
        '</div>' +
        '<div id="keys-table">' + spinnerHtml('lg') + '</div>';

      var self = this;
      $('#add-key-btn').addEventListener('click', function() { self.createKey(); });
      $('#keys-search').addEventListener('input', function(e) {
        self._filter = e.target.value.toLowerCase();
        self._renderRows();
      });

      this._filter = '';
      this._list = [];

      api.getKeys().then(function(res) {
        self._list = res.keys || res || [];
        if (!self._list.length) {
          $('#keys-table').innerHTML = emptyState('No API keys', 'Create a key to allow programmatic access.');
          return;
        }
        self._renderRows();
      }).catch(function(e) {
        $('#keys-table').innerHTML = emptyState('Error loading keys', e.message);
      });
    },

    _renderRows: function() {
      var filtered = this._list;
      if (this._filter) {
        filtered = filtered.filter(function(k) {
          var prefix = (k.prefix || k.key_prefix || '').toLowerCase();
          var label = (k.label || '').toLowerCase();
          var models = (k.models || []).join(' ').toLowerCase();
          return prefix.indexOf(this._filter) !== -1 || label.indexOf(this._filter) !== -1 || models.indexOf(this._filter) !== -1;
        }.bind(this));
      }

      if (!filtered.length) {
        $('#keys-table').innerHTML = emptyState('No keys found', this._filter ? 'Try a different search.' : 'Create a key to get started.');
        return;
      }

      var html = '<div class="lp-table-wrap"><table class="lp-table"><thead><tr>' +
        '<th scope="col">Prefix</th><th scope="col">Label</th><th scope="col">Models</th><th scope="col">Rate Limit</th><th scope="col">Budget</th><th scope="col">Created</th><th scope="col">Actions</th>' +
        '</tr></thead><tbody>';

      for (var i = 0; i < filtered.length; i++) {
        var k = filtered[i];
        var prefix = k.prefix || k.key_prefix || k.id || '';
        var fullKey = k.full_key || k.key || prefix;
        html += '<tr>' +
          '<td class="lp-mono">' +
            '<span class="lp-flex lp-items-center lp-gap-sm">' +
              '<span>' + escapeHtml(maskKey(k)) + '</span>' +
              '<button class="lp-btn lp-btn-ghost lp-btn-icon lp-btn-sm" data-copy="' + escapeHtml(fullKey) + '" aria-label="Copy key">' +
                '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>' +
              '</button>' +
            '</span>' +
          '</td>' +
          '<td>' + escapeHtml(k.label || '-') + '</td>' +
          '<td class="lp-text-sm">' + modelsHtml(k.models) + '</td>' +
          '<td>' + formatRateLimit(k.rpm) + '</td>' +
          '<td>' + formatBudget(k.budget) + '</td>' +
          '<td class="lp-text-muted lp-text-sm">' + timeAgo(k.created_at) + '</td>' +
          '<td><button class="lp-btn lp-btn-sm lp-btn-danger" data-revoke="' + escapeHtml(prefix) + '">Revoke</button></td>' +
        '</tr>';
      }
      html += '</tbody></table></div>';
      $('#keys-table').innerHTML = html;

      var self = this;
      var table = $('#keys-table');
      table.querySelectorAll('[data-copy]').forEach(function(btn) {
        btn.addEventListener('click', function() {
          var val = btn.getAttribute('data-copy');
          navigator.clipboard.writeText(val).then(function() {
            showToast('Copied to clipboard', 'success');
          });
        });
      });
      table.querySelectorAll('[data-revoke]').forEach(function(btn) {
        btn.addEventListener('click', function() {
          self.revokeKey(btn.getAttribute('data-revoke'));
        });
      });
    },

    createKey: function() {
      var body =
        '<div class="lp-input-group">' +
          '<label class="lp-label">Label</label>' +
          '<input class="lp-input" id="k-form-label" placeholder="My API Key">' +
        '</div>' +
        '<div class="lp-input-group">' +
          '<label class="lp-label">Models</label>' +
          '<input class="lp-input" id="k-form-models" placeholder="gpt-4o, claude-3">' +
          '<div class="lp-text-xs lp-text-muted lp-mt-sm">Comma-separated. Leave blank for all models.</div>' +
        '</div>' +
        '<div class="lp-input-group">' +
          '<label class="lp-label">Rate Limit (RPM)</label>' +
          '<input class="lp-input" type="number" id="k-form-rpm" value="0">' +
          '<div class="lp-text-xs lp-text-muted lp-mt-sm">0 = unlimited.</div>' +
        '</div>' +
        '<div class="lp-input-group">' +
          '<label class="lp-label">Budget (USD)</label>' +
          '<input class="lp-input" type="number" id="k-form-budget" step="0.01" value="0">' +
          '<div class="lp-text-xs lp-text-muted lp-mt-sm">0 = unlimited.</div>' +
        '</div>';

      var self = this;
      showModal('Create API Key', body, async function() {
        var modelsRaw = $('#k-form-models').value.trim();
        var data = {
          label: $('#k-form-label').value.trim() || undefined,
          models: modelsRaw ? modelsRaw.split(',').map(function(s) { return s.trim(); }).filter(Boolean) : undefined,
          rpm: parseInt($('#k-form-rpm').value) || undefined,
          budget: parseFloat($('#k-form-budget').value) || undefined
        };
        var res = await api.createKey(data);
        var rawKey = res.key || res.full_key || '';
        var msg = rawKey
          ? 'Key created. Copy it now \u2014 it won\'t be shown again: ' + rawKey.slice(0, 12) + '\u2026'
          : 'Key created \u2014 ' + (res.prefix || '');
        showToast(msg, 'success');
        ns.app.showPage('keys');
      });
    },

    revokeKey: function(prefix) {
      showModal('Revoke Key',
        '<p>Revoke key <strong>' + escapeHtml(prefix) + '</strong>? This cannot be undone.</p>',
        async function() {
          await api.deleteKey(prefix);
          showToast('Key revoked', 'success');
          ns.app.showPage('keys');
        }
      );
    }
  };

  ns.app && ns.app.registerPage('keys', keys);
  ns.keys = keys;
})(window.llmPico);
