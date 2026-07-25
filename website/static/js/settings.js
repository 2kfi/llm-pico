window.llmPico = window.llmPico || {};

(function(ns) {
  const { $, api, showToast, escapeHtml, skeleton, showModal } = ns;

  const STORAGE_KEY = 'lp_settings';
  const VERSION = 'v2.2';

  function loadSettings() {
    try {
      return JSON.parse(localStorage.getItem(STORAGE_KEY)) || {};
    } catch {
      return {};
    }
  }

  function saveSettings(data) {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(data));
  }

  function getProviderEnabled(provider) {
    var s = loadSettings();
    if (s.disabled_providers && s.disabled_providers.indexOf(provider) !== -1) return false;
    return true;
  }

  function toggleProvider(provider, enabled) {
    var s = loadSettings();
    s.disabled_providers = s.disabled_providers || [];
    var idx = s.disabled_providers.indexOf(provider);
    if (!enabled && idx === -1) {
      s.disabled_providers.push(provider);
    } else if (enabled && idx !== -1) {
      s.disabled_providers.splice(idx, 1);
    }
    saveSettings(s);
  }

  function providerGroupHtml(name, count, enabled) {
    var statusClass = enabled ? 'lp-status-active' : 'lp-status-inactive';
    var statusLabel = enabled ? 'Active' : 'Inactive';
    return '<div class="lp-card lp-settings-provider">' +
      '<div class="lp-flex lp-items-center lp-justify-between">' +
        '<div class="lp-flex lp-items-center lp-gap-md">' +
          '<span class="lp-status ' + statusClass + '">' +
            '<span class="lp-status-dot"></span>' +
            '<span>' + escapeHtml(name) + '</span>' +
          '</span>' +
          '<span class="lp-text-muted lp-text-sm">' + count + ' model' + (count !== 1 ? 's' : '') + '</span>' +
        '</div>' +
        '<label class="lp-toggle">' +
          '<input type="checkbox" class="lp-toggle-input" data-provider="' + escapeHtml(name) + '"' + (enabled ? ' checked' : '') + '>' +
          '<span class="lp-toggle-track"></span>' +
        '</label>' +
      '</div>' +
    '</div>';
  }

  function groupModels(models) {
    var groups = {};
    models.forEach(function(m) {
      var pid = m.provider || 'unknown';
      if (!groups[pid]) groups[pid] = 0;
      groups[pid]++;
    });
    return groups;
  }

  const settings = {
    async render(container) {
      var saved = loadSettings();
      var themeLight = document.documentElement.classList.contains('light-mode');
      var sidebarCollapsed = $('#app') ? $('#app').classList.contains('sidebar-collapsed') : false;

      container.innerHTML =
        '<div class="lp-page-header"><h1 class="lp-page-title">Settings</h1></div>' +

        '<div class="lp-settings-section">' +
          '<div class="lp-settings-section-title">General</div>' +
          '<div class="lp-settings-grid">' +
            '<div class="lp-input-group">' +
              '<label class="lp-label" for="settings-site-name">Site Name</label>' +
              '<input type="text" class="lp-input" id="settings-site-name" value="' + escapeHtml(saved.site_name || 'LLM Pico') + '" placeholder="LLM Pico">' +
            '</div>' +
            '<div class="lp-input-group">' +
              '<label class="lp-label" for="settings-rate-limit">Default Rate Limit (RPM)</label>' +
              '<input type="number" class="lp-input" id="settings-rate-limit" value="' + escapeHtml(String(saved.rate_limit || 60)) + '" min="1" max="100000">' +
            '</div>' +
            '<div class="lp-input-group">' +
              '<label class="lp-label" for="settings-log-level">Log Level</label>' +
              '<select class="lp-select" id="settings-log-level">' +
                '<option value="DEBUG"' + (saved.log_level === 'DEBUG' ? ' selected' : '') + '>DEBUG</option>' +
                '<option value="INFO"' + (saved.log_level === 'INFO' || !saved.log_level ? ' selected' : '') + '>INFO</option>' +
                '<option value="WARNING"' + (saved.log_level === 'WARNING' ? ' selected' : '') + '>WARNING</option>' +
                '<option value="ERROR"' + (saved.log_level === 'ERROR' ? ' selected' : '') + '>ERROR</option>' +
              '</select>' +
            '</div>' +
          '</div>' +
          '<div class="lp-flex" style="margin-top:var(--lp-space-4)">' +
            '<button class="lp-btn lp-btn-primary" id="settings-save-general">Save General Settings</button>' +
          '</div>' +
        '</div>' +

        '<div class="lp-settings-section">' +
          '<div class="lp-settings-section-title">Providers</div>' +
          '<div id="settings-providers">' + skeleton('rows', 3) + '</div>' +
        '</div>' +

        '<div class="lp-settings-section">' +
          '<div class="lp-settings-section-title">Appearance</div>' +
          '<div class="lp-settings-grid">' +
            '<div class="lp-settings-row">' +
              '<div class="lp-settings-label">Theme</div>' +
              '<div class="lp-settings-desc">Switch between dark and light mode</div>' +
            '</div>' +
            '<div class="lp-flex lp-items-center">' +
              '<label class="lp-toggle">' +
                '<input type="checkbox" class="lp-toggle-input" id="settings-theme-toggle"' + (themeLight ? ' checked' : '') + '>' +
                '<span class="lp-toggle-track"></span>' +
              '</label>' +
              '<span class="lp-text-muted lp-text-sm" style="margin-left:var(--lp-space-2)">' + (themeLight ? 'Light' : 'Dark') + '</span>' +
            '</div>' +
            '<div class="lp-settings-row">' +
              '<div class="lp-settings-label">Sidebar Collapsed</div>' +
              '<div class="lp-settings-desc">Collapse the navigation sidebar by default</div>' +
            '</div>' +
            '<div class="lp-flex lp-items-center">' +
              '<label class="lp-toggle">' +
                '<input type="checkbox" class="lp-toggle-input" id="settings-sidebar-toggle"' + (sidebarCollapsed ? ' checked' : '') + '>' +
                '<span class="lp-toggle-track"></span>' +
              '</label>' +
            '</div>' +
          '</div>' +
        '</div>' +

        '<div class="lp-settings-section">' +
          '<div class="lp-settings-section-title">Security</div>' +
          '<div class="lp-settings-grid">' +
            '<div class="lp-input-group">' +
              '<label class="lp-label" for="settings-session-timeout">Session Timeout (minutes)</label>' +
              '<input type="number" class="lp-input" id="settings-session-timeout" value="' + escapeHtml(String(saved.session_timeout || 30)) + '" min="5" max="1440">' +
            '</div>' +
            '<div class="lp-input-group">' +
              '<label class="lp-label" for="settings-cors-origins">CORS Origins</label>' +
              '<textarea class="lp-input" id="settings-cors-origins" rows="3" placeholder="https://example.com&#10;https://app.example.com">' + escapeHtml(saved.cors_origins || '*') + '</textarea>' +
            '</div>' +
          '</div>' +
          '<div class="lp-flex" style="margin-top:var(--lp-space-4)">' +
            '<button class="lp-btn lp-btn-primary" id="settings-save-security">Save Security Settings</button>' +
          '</div>' +
        '</div>' +

        '<div class="lp-settings-section">' +
          '<div class="lp-settings-section-title lp-text-danger">Danger Zone</div>' +
          '<div class="lp-card" style="border-color:var(--lp-red)">' +
            '<div class="lp-flex lp-items-center lp-justify-between">' +
              '<div>' +
                '<div class="lp-text-sm">Reset All Data</div>' +
                '<div class="lp-text-muted lp-text-xs">This will clear all localStorage data and reload the page.</div>' +
              '</div>' +
              '<button class="lp-btn lp-btn-danger" id="settings-reset-data">Reset All Data</button>' +
            '</div>' +
          '</div>' +
        '</div>' +

        '<div class="lp-flex lp-justify-between lp-items-center" style="margin-top:var(--lp-space-8);padding-top:var(--lp-space-4);border-top:1px solid var(--lp-border)">' +
          '<span class="lp-text-muted lp-text-xs">LLM Pico ' + VERSION + '</span>' +
          '<span class="lp-text-muted lp-text-xs">' + new Date().toLocaleDateString() + '</span>' +
        '</div>';

      // Bind events
      this._bindGeneral();
      this._bindAppearance();
      this._bindSecurity();
      this._bindDanger();
      this._loadProviders();
    },

    _bindGeneral: function() {
      var btn = $('#settings-save-general');
      if (btn) btn.onclick = function() {
        var s = loadSettings();
        s.site_name = $('#settings-site-name').value.trim() || 'LLM Pico';
        s.rate_limit = parseInt($('#settings-rate-limit').value, 10) || 60;
        s.log_level = $('#settings-log-level').value;
        saveSettings(s);
        showToast('General settings saved', 'success');
      };
    },

    _bindAppearance: function() {
      var themeToggle = $('#settings-theme-toggle');
      if (themeToggle) themeToggle.onchange = function() {
        document.documentElement.classList.toggle('light-mode', this.checked);
        var label = this.parentNode.nextElementSibling;
        if (label) label.textContent = this.checked ? 'Light' : 'Dark';
      };

      var sidebarToggle = $('#settings-sidebar-toggle');
      if (sidebarToggle) sidebarToggle.onchange = function() {
        var app = $('#app');
        if (app) app.classList.toggle('sidebar-collapsed', this.checked);
      };
    },

    _bindSecurity: function() {
      var btn = $('#settings-save-security');
      if (btn) btn.onclick = function() {
        var s = loadSettings();
        s.session_timeout = parseInt($('#settings-session-timeout').value, 10) || 30;
        s.cors_origins = $('#settings-cors-origins').value.trim() || '*';
        saveSettings(s);
        showToast('Security settings saved', 'success');
      };
    },

    _bindDanger: function() {
      var btn = $('#settings-reset-data');
      if (btn) btn.onclick = function() {
        showModal(
          'Reset All Data',
          '<p>Are you sure? This will clear all saved settings, preferences, and cached data.</p><p class="lp-text-muted lp-text-sm" style="margin-top:var(--lp-space-2)">This action cannot be undone.</p>',
          function() {
            localStorage.clear();
            showToast('All data cleared. Reloading...', 'info');
            setTimeout(function() { location.reload(); }, 1000);
          }
        );
      };
    },

    _loadProviders: function() {
      var el = $('#settings-providers');
      api.getModels().then(function(res) {
        var models = res.models || res || [];
        if (!Array.isArray(models)) models = [];
        var groups = groupModels(models);
        var keys = Object.keys(groups).sort();

        if (!keys.length) {
          el.innerHTML = '<div class="lp-text-muted">No providers configured</div>';
          return;
        }

        el.innerHTML = keys.map(function(name) {
          return providerGroupHtml(name, groups[name], getProviderEnabled(name));
        }).join('');

        el.querySelectorAll('.lp-toggle-input').forEach(function(input) {
          input.onchange = function() {
            toggleProvider(this.dataset.provider, this.checked);
          };
        });
      }).catch(function() {
        el.innerHTML = '<div class="lp-text-muted">Failed to load providers</div>';
      });
    }
  };

  ns.app && ns.app.registerPage('settings', settings);
  ns.settings = settings;
})(window.llmPico);
