window.llmPico = window.llmPico || {};

(function(ns) {
  const { $, $$, api, showToast, escapeHtml } = ns;

  const BASE = '/admin/dashboard';

  const routes = {
    '': 'overview',
    '/': 'overview',
    '/models': 'models',
    '/keys': 'keys',
    '/teams': 'teams',
    '/users': 'teams',
    '/usage': 'usage',
    '/playground': 'playground',
    '/settings': 'settings',
    '/trace': 'trace',
    '/graph': 'graph',
  };

  const pageTitles = {
    overview: 'Dashboard',
    models: 'Models',
    keys: 'API Keys',
    teams: 'Teams',
    usage: 'Usage & Analytics',
    playground: 'Playground',
    settings: 'Settings',
    trace: 'Request Tracing',
    graph: 'Routing Graph',
  };

  const pages = {};

  const commands = [
    { label: 'Go to Dashboard', shortcut: 'g d', action: function() { window.llmPico.app.navigate(BASE + '/'); } },
    { label: 'Go to Models', shortcut: 'g m', action: function() { window.llmPico.app.navigate(BASE + '/models'); } },
    { label: 'Go to Keys', shortcut: 'g k', action: function() { window.llmPico.app.navigate(BASE + '/keys'); } },
    { label: 'Go to Teams', shortcut: 'g t', action: function() { window.llmPico.app.navigate(BASE + '/teams'); } },
    { label: 'Go to Users', shortcut: 'g u', action: function() { window.llmPico.app.navigate(BASE + '/users'); } },
    { label: 'Go to Usage', shortcut: 'g p', action: function() { window.llmPico.app.navigate(BASE + '/usage'); } },
    { label: 'Go to Playground', shortcut: 'g l', action: function() { window.llmPico.app.navigate(BASE + '/playground'); } },
    { label: 'Go to Settings', shortcut: 'g s', action: function() { window.llmPico.app.navigate(BASE + '/settings'); } },
    { label: 'Toggle Theme', shortcut: '', action: function() { document.documentElement.classList.toggle('light-mode'); } },
    { label: 'Reload Config', shortcut: '', action: async function() {
      try { await api.reloadConfig(); showToast('Config reloaded', 'success'); } catch (e) { showToast('Failed to reload config', 'error'); }
    }},
  ];

  // --- Command palette ---

  function renderCommands(cmds) {
    const list = $('#command-list');
    if (!list) return;
    list.innerHTML = cmds.map(function(cmd, i) {
      return '<div class="lp-command-item' + (i === 0 ? ' lp-command-active' : '') +
        '" role="option" data-index="' + i + '">' +
        '<span class="lp-command-label">' + escapeHtml(cmd.label) + '</span>' +
        (cmd.shortcut ? '<span class="lp-command-shortcut"><span class="lp-kbd">' + escapeHtml(cmd.shortcut) + '</span></span>' : '') +
        '</div>';
    }).join('');
    list.querySelectorAll('.lp-command-item').forEach(function(el, i) {
      el.onclick = function() { closeCommandPalette(); cmds[i].action(); };
    });
  }

  function openCommandPalette() {
    var palette = $('#command-palette');
    var input = $('#command-input');
    var list = $('#command-list');
    if (!palette || !input || !list) return;

    palette.classList.remove('hidden');
    input.value = '';
    input.focus();
    renderCommands(commands);

    var backdrop = palette.querySelector('.lp-command-backdrop');
    if (backdrop) backdrop.onclick = closeCommandPalette;

    input.oninput = function() {
      var query = input.value.toLowerCase();
      var filtered = commands.filter(function(c) { return c.label.toLowerCase().indexOf(query) !== -1; });
      renderCommands(filtered);
    };

    input.onkeydown = function(e) {
      var items = list.querySelectorAll('[role="option"]');
      var current = list.querySelector('.lp-command-active');
      var idx = Array.from(items).indexOf(current);

      if (e.key === 'ArrowDown') {
        e.preventDefault();
        idx = Math.min(idx + 1, items.length - 1);
        items.forEach(function(i) { i.classList.remove('lp-command-active'); });
        items[idx] && items[idx].classList.add('lp-command-active');
        items[idx] && items[idx].scrollIntoView({ block: 'nearest' });
      } else if (e.key === 'ArrowUp') {
        e.preventDefault();
        idx = Math.max(idx - 1, 0);
        items.forEach(function(i) { i.classList.remove('lp-command-active'); });
        items[idx] && items[idx].classList.add('lp-command-active');
        items[idx] && items[idx].scrollIntoView({ block: 'nearest' });
      } else if (e.key === 'Enter') {
        e.preventDefault();
        current && current.click();
      } else if (e.key === 'Escape') {
        closeCommandPalette();
      }
    };
  }

  function closeCommandPalette() {
    var palette = $('#command-palette');
    if (palette) palette.classList.add('hidden');
  }

  function toggleCommandPalette() {
    var palette = $('#command-palette');
    if (!palette) return;
    if (palette.classList.contains('hidden')) {
      openCommandPalette();
    } else {
      closeCommandPalette();
    }
  }

  // --- App ---

  class App {
    constructor() {
      this.currentPage = null;
      var self = this;

      window.addEventListener('popstate', function() { self.route(); });

      document.addEventListener('click', function(e) {
        var a = e.target.closest('a[data-page]');
        if (a) {
          e.preventDefault();
          self.navigate(a.getAttribute('href'));
          self.closeMobileMenu();
        }
      });

      if ($('#init-back')) $('#init-back').addEventListener('click', function() { ns.initWizard && ns.initWizard.back(); });
      if ($('#init-next')) $('#init-next').addEventListener('click', function() { ns.initWizard && ns.initWizard.next(); });
      if ($('#init-skip')) $('#init-skip').addEventListener('click', function() { self.skipInit(); });

      if ($('#lp-reload-config')) $('#lp-reload-config').addEventListener('click', async function() {
        try { await api.reloadConfig(); showToast('Config reloaded', 'success'); } catch (e) { showToast('Failed to reload config', 'error'); }
      });

      if ($('#lp-theme-toggle')) $('#lp-theme-toggle').addEventListener('click', function() {
        document.documentElement.classList.toggle('light-mode');
      });

      if ($('#sidebar-collapse')) $('#sidebar-collapse').addEventListener('click', function() {
        $('#app') && $('#app').classList.toggle('sidebar-collapsed');
      });

      if ($('#mobile-menu-btn')) $('#mobile-menu-btn').addEventListener('click', function() { self.toggleMobileMenu(); });
      if ($('#sidebar-backdrop')) $('#sidebar-backdrop').addEventListener('click', function() { self.closeMobileMenu(); });

      if ($('#search-trigger')) $('#search-trigger').addEventListener('click', function() { toggleCommandPalette(); });

      this._initKeyboard();
    }

    registerPage(name, controller) {
      pages[name] = controller;
    }

    async init() {
      try {
        var status = await api.getInitStatus();
        if (!status.initialized || !ns.isLoggedIn()) {
          this.showInit();
          return;
        }
      } catch {
        this.showInit();
        return;
      }
      this.hideInit();
      this.route();
    }

    route() {
      var path = location.pathname.replace(BASE, '') || '/';
      var page = routes[path] || 'overview';
      this.showPage(page);
    }

    navigate(path) {
      history.pushState(null, '', path);
      this.route();
    }

    showPage(name) {
      this.currentPage = name;
      this.updateSidebar(name);

      var title = pageTitles[name] || name;
      document.title = 'LLM Pico — ' + title;
      var pageTitle = $('#page-title');
      if (pageTitle) pageTitle.textContent = title;

      var content = $('#content');
      var controller = pages[name];
      if (!controller) {
        content.innerHTML = ns.emptyState('Page not found', '');
        return;
      }
      content.innerHTML = ns.spinnerHtml('lg');
      try {
        controller.render(content);
      } catch (e) {
        content.innerHTML = ns.emptyState('Error loading page', e.message);
      }
    }

    updateSidebar(pageName) {
      $$('.lp-nav-item[data-page]').forEach(function(a) {
        var href = a.getAttribute('href').replace(BASE, '') || '/';
        var isActive = routes[href] === pageName;
        a.classList.toggle('active', isActive);
        if (isActive) a.setAttribute('aria-current', 'page');
        else a.removeAttribute('aria-current');
      });
    }

    toggleMobileMenu() {
      var sidebar = $('#sidebar');
      if (sidebar) sidebar.classList.toggle('mobile-open');
      var backdrop = $('#sidebar-backdrop');
      if (backdrop) backdrop.classList.toggle('active');
    }

    closeMobileMenu() {
      var sidebar = $('#sidebar');
      if (sidebar) sidebar.classList.remove('mobile-open');
      var backdrop = $('#sidebar-backdrop');
      if (backdrop) backdrop.classList.remove('active');
    }

    showInit() {
      var overlay = $('#init-overlay');
      if (!overlay) return;
      overlay.classList.remove('hidden');
      overlay.classList.add('lp-init-overlay');
      ns.initWizard = new ns.InitWizard();
      ns.initWizard.start();
    }

    hideInit() {
      var overlay = $('#init-overlay');
      if (!overlay) return;
      overlay.classList.add('hidden');
      overlay.classList.remove('lp-init-overlay');
    }

    skipInit() {
      this.hideInit();
      this.route();
    }

    _initKeyboard() {
      var self = this;
      document.addEventListener('keydown', function(e) {
        var tag = e.target.tagName;
        if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;

        if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
          e.preventDefault();
          toggleCommandPalette();
          return;
        }

        if (e.key === 'Escape') {
          closeCommandPalette();
          return;
        }

        if (e.key === '/' && !e.metaKey && !e.ctrlKey) {
          e.preventDefault();
          var trigger = $('#search-trigger');
          if (trigger) trigger.click();
          return;
        }

        if (e.key === 'g' && !e.repeat) {
          var handler = function(e2) {
            document.removeEventListener('keydown', handler);
            if (e2.key === 'd') self.navigate(BASE + '/');
            else if (e2.key === 'm') self.navigate(BASE + '/models');
            else if (e2.key === 'k') self.navigate(BASE + '/keys');
            else if (e2.key === 't') self.navigate(BASE + '/teams');
            else if (e2.key === 'u') self.navigate(BASE + '/users');
            else if (e2.key === 'p') self.navigate(BASE + '/usage');
            else if (e2.key === 'l') self.navigate(BASE + '/playground');
            else if (e2.key === 's') self.navigate(BASE + '/settings');
          };
          setTimeout(function() { document.addEventListener('keydown', handler, { once: true }); }, 0);
        }
      });
    }
  }

  ns.App = App;
})(window.llmPico);

document.addEventListener('DOMContentLoaded', function() {
  window.llmPico.app = new window.llmPico.App();
  window.llmPico.app.init();
});
