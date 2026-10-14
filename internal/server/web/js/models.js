window.llmPico = window.llmPico || {};

(function(ns) {
  const { $, api, showToast, showModal, escapeHtml, capBadgeHtml, formatCost, spinnerHtml, emptyState, detectCapabilities, PROVIDERS, getProviderById, debounce } = ns;

  var _all = [];
  var _filtered = [];

  var SEARCH_ICON = '<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="7" cy="7" r="5"/><path d="M11 11l3.5 3.5"/></svg>';
  var EDIT_ICON = '<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M11.5 1.5l3 3L5 14H2v-3L11.5 1.5z"/></svg>';
  var DELETE_ICON = '<svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M2 4h12M5.3 4V2.7a.7.7 0 01.7-.7h4a.7.7 0 01.7.7V4M6.7 7v4M9.3 7v4"/><path d="M3.3 4l.7 9.3a1 1 0 001 .7h6a1 1 0 001-.7L12.7 4"/></svg>';

  function buildRow(m) {
    var caps = (m.capabilities || ['Text']).map(function(c) {
      return capBadgeHtml(c);
    }).join(' ');

    var statusClass = (m.status === 'active' || m.status == null) ? 'lp-status-active' : 'lp-status-inactive';
    var providerName = m.provider || '-';
    var provider = getProviderById(m.provider);
    if (provider) providerName = escapeHtml(provider.name);

    return '<tr>' +
      '<td class="lp-mono">' + escapeHtml(m.id) + '</td>' +
      '<td><span class="lp-badge lp-badge-accent">' + providerName + '</span></td>' +
      '<td class="lp-flex lp-gap-sm">' + caps + '</td>' +
      '<td><span class="lp-status ' + statusClass + '"><span class="lp-status-dot"></span>' + (m.status || 'active') + '</span></td>' +
      '<td>' + (m.cost_per_1m != null ? formatCost(m.cost_per_1m) : '-') + '</td>' +
      '<td class="lp-flex lp-gap-sm">' +
      '<button class="lp-btn lp-btn-icon" aria-label="Edit ' + escapeHtml(m.id) + '" onclick="llmPico.models.editModel(\'' + escapeHtml(m.id) + '\')">' + EDIT_ICON + '</button>' +
      '<button class="lp-btn lp-btn-icon lp-btn-danger" aria-label="Delete ' + escapeHtml(m.id) + '" onclick="llmPico.models.deleteModel(\'' + escapeHtml(m.id) + '\')">' + DELETE_ICON + '</button>' +
      '</td></tr>';
  }

  function applyFilters() {
    var q = ($('#models-search').value || '').toLowerCase();
    var pid = $('#models-provider-filter').value;

    _filtered = _all.filter(function(m) {
      if (pid && m.provider !== pid) return false;
      if (q) {
        var hay = ((m.id || '') + ' ' + (m.name || '') + ' ' + (m.provider || '')).toLowerCase();
        if (hay.indexOf(q) === -1) return false;
      }
      return true;
    });

    var countEl = $('#models-count');
    if (countEl) countEl.textContent = _filtered.length + ' model' + (_filtered.length !== 1 ? 's' : '');

    var tbody = $('#models-tbody');
    if (!tbody) return;
    tbody.innerHTML = '';
    _filtered.forEach(function(m) {
      tbody.insertAdjacentHTML('beforeend', buildRow(m));
    });
  }

  function buildProviderOpts() {
    return PROVIDERS.map(function(p) {
      return '<option value="' + p.id + '">' + escapeHtml(p.name) + '</option>';
    }).join('');
  }

  var models = {
    async render(container) {
      container.innerHTML = '<div class="lp-page-header">' +
        '<div><h1 class="lp-page-title">Models</h1>' +
        '<p class="lp-page-subtitle">Manage available LLM models and providers</p></div>' +
        '<button class="lp-btn lp-btn-primary" id="add-model-btn">+ Add Model</button></div>' +
        '<div id="models-table">' + spinnerHtml('lg') + '</div>';

      $('#add-model-btn').addEventListener('click', function() { models.addModel(); });

      try {
        var res = await api.getModels();
        _all = res.models || res || [];
        _filtered = _all.slice();

        if (!_all.length) {
          $('#models-table').innerHTML = emptyState('No models', 'Add a model to get started.');
          return;
        }

        var count = _all.length;
        var html =         '<div class="lp-table-toolbar">' +
          '<div class="lp-table-search">' + SEARCH_ICON +
          '<label for="models-search" class="sr-only">Search models</label>' +
          '<input type="text" id="models-search" placeholder="Search models..." class="lp-input" style="padding-left:36px"></div>' +
          '<label for="models-provider-filter" class="sr-only">Filter by provider</label>' +
          '<select class="lp-select" id="models-provider-filter">' +
          '<option value="">All Providers</option>' + buildProviderOpts() + '</select>' +
          '<span class="lp-text-sm lp-text-muted" id="models-count">' + count + ' model' + (count !== 1 ? 's' : '') + '</span></div>' +
          '<div class="lp-table-wrap"><table class="lp-table"><thead><tr>' +
          '<th scope="col">Model</th><th scope="col">Provider</th><th scope="col">Capabilities</th><th scope="col">Status</th><th scope="col">Cost/1M</th><th scope="col">Actions</th>' +
          '</tr></thead><tbody id="models-tbody">';

        _filtered.forEach(function(m) { html += buildRow(m); });
        html += '</tbody></table></div>';
        $('#models-table').innerHTML = html;

        $('#models-search').addEventListener('input', debounce(applyFilters, 150));
        $('#models-provider-filter').addEventListener('change', applyFilters);

      } catch (e) {
        $('#models-table').innerHTML = emptyState('Error loading models', e.message);
      }
    },

    addModel() {
      var body = '<div class="lp-input-group"><label class="lp-label">Model ID</label>' +
        '<input class="lp-input" id="m-form-id" placeholder="gpt-4o"></div>' +
        '<div class="lp-input-group"><label class="lp-label">Display Name</label>' +
        '<input class="lp-input" id="m-form-name" placeholder="GPT-4o"></div>' +
        '<div class="lp-input-group"><label class="lp-label">Provider</label>' +
        '<select class="lp-select" id="m-form-provider">' + buildProviderOpts() + '</select></div>' +
        '<div class="lp-input-group"><label class="lp-label">Cost per 1M tokens (USD)</label>' +
        '<input class="lp-input" type="number" id="m-form-cost" step="0.01" placeholder="0.00"></div>' +
        '<div class="lp-input-group"><label class="lp-label">Max Tokens</label>' +
        '<input class="lp-input" type="number" id="m-form-max-tokens" placeholder="4096"></div>';

      showModal('Add Model', body, async function() {
        var data = {
          id: $('#m-form-id').value.trim(),
          name: $('#m-form-name').value.trim() || $('#m-form-id').value.trim(),
          provider: $('#m-form-provider').value,
          cost_per_1m: parseFloat($('#m-form-cost').value) || 0,
          max_tokens: parseInt($('#m-form-max-tokens').value) || undefined,
          capabilities: detectCapabilities($('#m-form-id').value),
          status: 'active',
        };
        if (!data.id) { showToast('Model ID required', 'error'); return; }
        await api.createModel(data);
        showToast('Model added', 'success');
        ns.app.showPage('models');
      });
    },

    editModel(id) {
      api.getModels().then(function(res) {
        var list = res.models || res || [];
        var m = list.find(function(x) { return x.id === id; });
        if (!m) return;
        var body = '<div class="lp-input-group"><label class="lp-label">Display Name</label>' +
          '<input class="lp-input" id="m-edit-name" value="' + escapeHtml(m.name || '') + '"></div>' +
          '<div class="lp-input-group"><label class="lp-label">Cost per 1M (USD)</label>' +
          '<input class="lp-input" type="number" id="m-edit-cost" step="0.01" value="' + (m.cost_per_1m || 0) + '"></div>' +
          '<div class="lp-input-group"><label class="lp-label">Status</label>' +
          '<select class="lp-select" id="m-edit-status">' +
          '<option value="active"' + (m.status === 'active' ? ' selected' : '') + '>Active</option>' +
          '<option value="inactive"' + (m.status === 'inactive' ? ' selected' : '') + '>Inactive</option>' +
          '</select></div>';

        showModal('Edit Model: ' + escapeHtml(id), body, async function() {
          await api.updateModel(id, {
            name: $('#m-edit-name').value.trim(),
            cost_per_1m: parseFloat($('#m-edit-cost').value) || 0,
            status: $('#m-edit-status').value,
          });
          showToast('Model updated', 'success');
          ns.app.showPage('models');
        });
      });
    },

    deleteModel(id) {
      showModal('Delete Model',
        '<p>Delete <strong>' + escapeHtml(id) + '</strong>? This cannot be undone.</p>',
        async function() {
          await api.deleteModel(id);
          showToast('Model deleted', 'success');
          ns.app.showPage('models');
        }
      );
    }
  };

  ns.app && ns.app.registerPage('models', models);
  ns.models = models;
})(window.llmPico);
