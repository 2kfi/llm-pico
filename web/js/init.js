window.llmPico = window.llmPico || {};

(function(ns) {
  const { $, $$, api, showToast, escapeHtml, capBadgeHtml, detectCapabilities, PROVIDERS, getProviderById, spinnerHtml, announceToScreenReader } = ns;

  class InitWizard {
    constructor() {
      this.currentStep = 0;
      this.data = { masterKey: null, providers: [], keys: {}, models: [], capabilities: {}, users: [], teams: [] };
      this.steps = [
        { id: 'welcome', title: 'Welcome', render: () => this.renderWelcome() },
        { id: 'providers', title: 'Select Providers', render: () => this.renderProviders() },
        { id: 'keys', title: 'API Keys', render: () => this.renderKeys() },
        { id: 'models', title: 'Model Discovery', render: () => this.renderModels() },
        { id: 'capabilities', title: 'Capabilities', render: () => this.renderCapabilities() },
        { id: 'limits', title: 'Rate Limits & Pricing', render: () => this.renderLimits() },
        { id: 'users', title: 'Create Users', render: () => this.renderUsers() },
        { id: 'teams', title: 'Create Teams', render: () => this.renderTeams() },
      ];
    }

    async start() {
      this.currentStep = 0;
      this.data.masterKey = null;
      this.render();
      await this._generateKey();
    }

    async _generateKey() {
      const desc = $('#init-step .lp-init-card-desc');
      const btn = $('#init-next');
      if (btn) btn.disabled = true;
      try {
        const res = await api.initInstance();
        this.data.masterKey = res.master_key;
        if (desc) {
          desc.outerHTML =
            '<div class="lp-init-card-desc">Your master key has been generated. Copy it now — it will not be shown again.</div>' +
            '<div class="lp-key-display">' +
            '<code id="init-master-key">' + escapeHtml(res.master_key) + '</code>' +
            '<button class="lp-btn lp-btn-sm" id="init-copy-key" aria-label="Copy master key">Copy</button>' +
            '</div>' +
            '<div class="lp-key-warning">' +
            '<svg width="14" height="14" viewBox="0 0 18 18" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="M9 1.5L1.5 16h15L9 1.5z"/><line x1="9" y1="7" x2="9" y2="11"/><circle cx="9" cy="13" r="0.5" fill="currentColor"/></svg>' +
            'Save this key somewhere safe. You will need it for all admin access.</div>';
          const copyBtn = $('#init-copy-key');
          if (copyBtn) {
            copyBtn.addEventListener('click', async () => {
              try {
                await navigator.clipboard.writeText(res.master_key);
                copyBtn.textContent = 'Copied!';
                announceToScreenReader('Master key copied to clipboard');
                setTimeout(() => { copyBtn.textContent = 'Copy'; }, 2000);
              } catch {
                showToast('Copy failed — select and copy manually', 'warning');
              }
            });
          }
        }
        if (btn) btn.disabled = false;
      } catch (err) {
        if (desc) desc.textContent = 'Failed to initialize: ' + err.message;
      }
    }

    _saveDOMState() {
      $$('#init-step input[data-provider]').forEach(inp => {
        const pid = inp.dataset.provider;
        const field = inp.dataset.field;
        if (!this.data.keys[pid]) this.data.keys[pid] = {};
        this.data.keys[pid][field] = inp.value;
      });
      $$('#init-step input[data-model-idx]').forEach(inp => {
        const idx = parseInt(inp.dataset.modelIdx);
        const cap = inp.dataset.cap;
        if (cap && this.data.models[idx]) {
          const mid = this.data.models[idx].id;
          if (!this.data.capabilities[mid]) this.data.capabilities[mid] = [];
          if (inp.checked && !this.data.capabilities[mid].includes(cap)) {
            this.data.capabilities[mid].push(cap);
          } else if (!inp.checked) {
            this.data.capabilities[mid] = this.data.capabilities[mid].filter(c => c !== cap);
          }
        }
        const limit = inp.dataset.limit;
        if (limit && this.data.models[idx]) {
          this.data.models[idx][limit] = inp.value ? Number(inp.value) : undefined;
        }
      });
    }

    next() {
      if (this.currentStep < this.steps.length - 1) {
        this.currentStep++;
        this.render();
      } else {
        this.complete();
      }
    }

    back() {
      if (this.currentStep > 0) {
        this.currentStep--;
        this.render();
      }
    }

    render() {
      const step = this.steps[this.currentStep];
      const stepEl = $('#init-step');
      if (!stepEl) return;

      this._saveDOMState();

      const fill = $('.lp-progress-fill');
      if (fill) fill.style.width = ((this.currentStep + 1) / this.steps.length * 100) + '%';

      let dots = '<div class="lp-step-indicator">';
      this.steps.forEach((s, i) => {
        if (i > 0) dots += '<div class="lp-step-line' + (i <= this.currentStep ? ' completed' : '') + '"></div>';
        dots += '<div class="lp-step-dot' + (i === this.currentStep ? ' active' : '') + (i < this.currentStep ? ' completed' : '') + '"></div>';
      });
      dots += '</div>';

      stepEl.innerHTML = dots + '<div class="lp-init-card-title">' + escapeHtml(step.title) + '</div>' + step.render();

      $('#init-back').style.display = this.currentStep === 0 ? 'none' : '';
      $('#init-next').textContent = this.currentStep === this.steps.length - 1 ? 'Finish' : 'Next';
      $('#init-skip').style.display = this.currentStep === 0 ? '' : 'none';
    }

    renderWelcome() {
      return '<div class="lp-init-card-desc">Generating your master key...</div>' +
        '<div class="lp-flex lp-content-center" style="margin-top:var(--lp-space-xl)">' +
        '<div class="lp-spinner lp-spinner-lg" role="status"><span class="sr-only">Generating key...</span></div></div>';
    }

    renderProviders() {
      const grouped = {};
      PROVIDERS.forEach(p => {
        if (!grouped[p.category]) grouped[p.category] = [];
        grouped[p.category].push(p);
      });

      const categoryLabels = { cloud: 'Cloud Providers', aggregator: 'Aggregators', local: 'Local', custom: 'Custom API' };
      let html = '<div class="lp-init-card-desc">Pick the providers you want to configure.</div>';

      html += '<div class="lp-provider-list">';
      html += '<div class="lp-provider-search"><label for="provider-search">Search providers</label><input type="text" id="provider-search" placeholder="Type to filter..."></div>';

      Object.entries(grouped).forEach(([cat, providers]) => {
        html += '<div class="lp-provider-group-label">' + (categoryLabels[cat] || cat) + '</div>';
        providers.forEach(p => {
          const sel = this.data.providers.includes(p.id);
          const caps = (p.capabilities || ['Text']).map(c => capBadgeHtml(c)).join(' ');
          html += '<div class="lp-provider-row" data-provider="' + p.id + '" tabindex="0" role="checkbox" aria-checked="' + sel + '" aria-label="' + escapeHtml(p.name) + '">' +
            '<div class="lp-provider-toggle' + (sel ? ' on' : '') + '"></div>' +
            '<span class="lp-provider-row-name">' + escapeHtml(p.name) + '</span>' +
            '<span class="lp-provider-row-caps">' + caps + '</span>' +
            '</div>';
        });
      });

      html += '</div>';
      html += '<div class="lp-provider-count" id="provider-count">' + this.data.providers.length + ' selected</div>';
      return html;
    }

    renderKeys() {
      let html = '<div class="lp-init-card-desc">Enter API keys for selected providers.</div>';
      this.data.providers.forEach(pid => {
        const p = getProviderById(pid);
        if (!p) return;
        html += '<div class="lp-input-group"><label class="lp-label">' + escapeHtml(p.name) + '</label>';
        p.fields.forEach(f => {
          const val = (this.data.keys[pid] && this.data.keys[pid][f]) || '';
          html += '<input class="lp-input" data-provider="' + pid + '" data-field="' + f + '" ' +
            'value="' + escapeHtml(val) + '" placeholder="' + escapeHtml(f.replace(/_/g, ' ')) + '" style="margin-bottom:var(--lp-space-sm)">';
        });
        html += '</div>';
      });
      return html;
    }

    renderModels() {
      return '<div class="lp-init-card-desc">Probe providers to discover available models.</div>' +
        '<div id="init-models-list">' +
        '<button class="lp-btn lp-btn-primary" id="init-probe-btn">Discover Models</button>' +
        '</div>';
    }

    renderCapabilities() {
      let html = '<div class="lp-init-card-desc">Review and edit detected capabilities for each model.</div>';
      this.data.models.forEach((m, i) => {
        const caps = this.data.capabilities[m.id] || m.capabilities || ['Text'];
        html += '<div class="lp-flex lp-items-center lp-gap-md" style="margin-bottom:var(--lp-space-sm)">' +
          '<span style="min-width:160px">' + escapeHtml(m.name || m.id) + '</span>';
        ns.CAPABILITIES.forEach(c => {
          const checked = caps.includes(c) ? ' checked' : '';
          html += '<label class="lp-flex lp-items-center lp-gap-xs">' +
            '<input type="checkbox" data-model-idx="' + i + '" data-cap="' + c + '"' + checked + '> ' +
            capBadgeHtml(c) + '</label>';
        });
        html += '</div>';
      });
      return html;
    }

    renderLimits() {
      let html = '<div class="lp-init-card-desc">Set rate limits and pricing per model (optional).</div>';
      this.data.models.forEach((m, i) => {
        html += '<div class="lp-flex lp-items-center lp-gap-md" style="margin-bottom:var(--lp-space-sm)">' +
          '<span style="min-width:160px">' + escapeHtml(m.name || m.id) + '</span>' +
          '<input class="lp-input" style="max-width:100px" placeholder="RPM" data-model-idx="' + i + '" data-limit="rpm" value="' + (m.rpm || '') + '">' +
          '<input class="lp-input" style="max-width:100px" placeholder="Cost/1M" data-model-idx="' + i + '" data-limit="cost_per_1m" value="' + (m.cost_per_1m || '') + '">' +
          '</div>';
      });
      return html;
    }

    renderUsers() {
      return '<div class="lp-init-card-desc">Create your first user (optional, can add later).</div>' +
        '<div class="lp-input-group"><label class="lp-label">Username</label><input class="lp-input" id="init-user-name" placeholder="admin"></div>' +
        '<div class="lp-input-group"><label class="lp-label">Email</label><input class="lp-input" id="init-user-email" placeholder="admin@example.com"></div>';
    }

    renderTeams() {
      return '<div class="lp-init-card-desc">Create your first team (optional, can add later).</div>' +
        '<div class="lp-input-group"><label class="lp-label">Team Name</label><input class="lp-input" id="init-team-name" placeholder="Default Team"></div>' +
        '<div class="lp-input-group"><label class="lp-label">Budget (USD, 0=unlimited)</label><input class="lp-input" type="number" id="init-team-budget" value="0"></div>';
    }

    async complete() {
      this._saveDOMState();
      showToast('Setup complete!', 'success');
      ns.app.hideInit();
      ns.app.route();
    }
  }

  ns.InitWizard = InitWizard;

  document.addEventListener('click', (e) => {
    const row = e.target.closest('.lp-provider-row');
    if (row && row.closest('#init-step')) {
      const pid = row.dataset.provider;
      const idx = ns.initWizard.data.providers.indexOf(pid);
      if (idx >= 0) ns.initWizard.data.providers.splice(idx, 1);
      else ns.initWizard.data.providers.push(pid);
      row.classList.toggle('on', idx < 0);
      const toggle = row.querySelector('.lp-provider-toggle');
      if (toggle) toggle.classList.toggle('on', idx < 0);
      row.setAttribute('aria-checked', idx < 0);
      const count = $('#provider-count');
      if (count) count.textContent = ns.initWizard.data.providers.length + ' selected';
    }
    if (e.target.id === 'init-probe-btn') {
      probeModels();
    }
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') {
      const row = e.target.closest('.lp-provider-row');
      if (row && row.closest('#init-step')) {
        e.preventDefault();
        row.click();
      }
    }
  });

  document.addEventListener('input', (e) => {
    if (e.target.id === 'provider-search') {
      const q = e.target.value.toLowerCase();
      $$('#init-step .lp-provider-row').forEach(row => {
        const name = row.querySelector('.lp-provider-row-name').textContent.toLowerCase();
        row.classList.toggle('hidden', q && !name.includes(q));
      });
      $$('#init-step .lp-provider-group-label').forEach(label => {
        let next = label.nextElementSibling;
        let hasVisible = false;
        while (next && !next.classList.contains('lp-provider-group-label')) {
          if (next.classList.contains('lp-provider-row') && !next.classList.contains('hidden')) hasVisible = true;
          next = next.nextElementSibling;
        }
        label.style.display = hasVisible || !q ? '' : 'none';
      });
    }
  });

  async function probeModels() {
    const list = $('#init-models-list');
    if (!list) return;
    list.innerHTML = ns.spinnerHtml() + ' Probing providers...';

    const allModels = [];
    for (const pid of ns.initWizard.data.providers) {
      const keys = ns.initWizard.data.keys[pid] || {};
      const apiKey = keys.api_key || '';
      if (!apiKey) continue;
      try {
        const prov = getProviderById(pid);
        const result = await api.probeProvider({
          provider: pid,
          api_key: apiKey,
          base_url: keys.base_url || (prov && prov.baseUrl) || '',
          account_id: keys.account_id,
        });
        const models = (result.models || []).map(m => ({
          id: m.id || m,
          name: m.name || m.id || m,
          provider: pid,
          capabilities: ns.detectCapabilities(m.id || m),
        }));
        allModels.push(...models);
      } catch (err) {
        showToast('Failed to probe ' + pid + ': ' + err.message, 'error');
      }
    }

    ns.initWizard.data.models = allModels;

    if (!allModels.length) {
      list.innerHTML = '<div class="lp-text-secondary">No models found. Add keys and try again.</div>';
      return;
    }

    list.innerHTML = '<div class="lp-text-secondary lp-mb-md">Found ' + allModels.length + ' models</div>' +
      allModels.map((m, i) => '<label class="lp-flex lp-items-center lp-gap-sm" style="margin-bottom:var(--lp-space-sm)">' +
        '<input type="checkbox" data-model-check="' + i + '" checked> ' +
        '<span>' + escapeHtml(m.name) + '</span>' +
        '<span class="lp-text-muted lp-text-sm">' + escapeHtml(m.provider) + '</span>' +
        '</label>').join('');

    list.addEventListener('change', (e) => {
      if (e.target.dataset.modelCheck != null) {
        const idx = parseInt(e.target.dataset.modelCheck);
        if (!e.target.checked) {
          ns.initWizard.data.models[idx] = null;
        }
      }
    });
  }
})(window.llmPico);
