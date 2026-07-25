window.llmPico = window.llmPico || {};

(function(ns) {
  const { $, api, showToast, formatNumber, formatCost, escapeHtml, skeleton, emptyState, formatDate, timeAgo } = ns;

  const dashboard = {
    async render(container) {
      container.innerHTML = '<div class="lp-page-header"><h1 class="lp-page-title">Dashboard</h1>' +
        '<span class="lp-page-subtitle">Overview of your LLM proxy</span></div>' +
        '<div class="lp-dashboard-grid" id="dash-stats"></div>' +
        '<div class="lp-dashboard-main" id="dash-main"></div>' +
        '<div id="dash-bottom"></div>';

      $('#dash-stats').innerHTML = skeleton('stat') + skeleton('stat') + skeleton('stat') + skeleton('stat');
      $('#dash-main').innerHTML =
        '<div class="lp-card"><div class="lp-card-header"><span class="lp-card-title">Provider Health</span></div>' +
        '<div class="lp-card-body" id="dash-health">' + skeleton('rows', 3) + '</div></div>' +
        '<div class="lp-card"><div class="lp-card-header"><span class="lp-card-title">Recent Errors</span></div>' +
        '<div class="lp-card-body" id="dash-errors">' + skeleton('rows', 3) + '</div></div>';
      $('#dash-bottom').innerHTML =
        '<div class="lp-card" style="margin-top:var(--lp-space-xl)"><div class="lp-card-header">' +
        '<span class="lp-card-title">Cost by Provider</span></div>' +
        '<div class="lp-card-body" id="dash-cost">' + skeleton('rows', 2) + '</div></div>';

      const [models, keys, metrics, costs, errors] = await Promise.all([
        api.getModels().catch(() => []),
        api.getKeys().catch(() => []),
        api.getMetrics().catch(() => ({})),
        api.getCosts({ period: 'today' }).catch(() => ({})),
        api.getErrors({ period: 'today' }).catch(() => []),
      ]);

      const modelList = models.models || models || [];
      const keyList = keys.keys || keys || [];
      const modelCount = Array.isArray(modelList) ? modelList.length : 0;
      const keyCount = Array.isArray(keyList) ? keyList.length : 0;
      const requestsToday = metrics.requests_today || metrics.total_requests || 0;
      const costToday = costs.total_cost || costs.cost || 0;

      $('#dash-stats').innerHTML =
        statCard('Models', formatNumber(modelCount), modelSvg()) +
        statCard('Active Keys', formatNumber(keyCount), keySvg()) +
        statCard('Requests Today', formatNumber(requestsToday), chartSvg()) +
        statCard('Cost Today', formatCost(costToday), dollarSvg());

      $('#dash-main').innerHTML =
        '<div class="lp-card"><div class="lp-card-header"><span class="lp-card-title">Provider Health</span></div>' +
        '<div class="lp-card-body" id="dash-health"></div></div>' +
        '<div class="lp-card"><div class="lp-card-header"><span class="lp-card-title">Recent Errors</span></div>' +
        '<div class="lp-card-body" id="dash-errors"></div></div>';

      // Provider health
      const providers = {};
      if (Array.isArray(modelList)) {
        modelList.forEach(function(m) {
          var pid = m.provider || 'unknown';
          if (!providers[pid]) providers[pid] = { ok: 0, fail: 0, models: [] };
          providers[pid].models.push(m.id);
          if (m.status === 'active' || m.status == null) providers[pid].ok++;
          else providers[pid].fail++;
        });
      }

      var healthEl = $('#dash-health');
      var providerKeys = Object.keys(providers);
      if (providerKeys.length) {
        var rows = '';
        providerKeys.forEach(function(pid) {
          var s = providers[pid];
          var status = s.fail === 0 ? 'healthy' : s.ok === 0 ? 'down' : 'degraded';
          rows += '<div class="lp-flex lp-items-center lp-justify-between" style="padding:10px 0;border-bottom:1px solid var(--lp-border)">' +
            '<div class="lp-flex lp-items-center lp-gap-md">' +
            '<span class="lp-health-dot ' + status + '"></span>' +
            '<span style="font-weight:var(--lp-font-weight-medium)">' + escapeHtml(pid) + '</span>' +
            '<span class="lp-text-sm lp-text-muted">' + s.models.length + ' model' + (s.models.length !== 1 ? 's' : '') + '</span>' +
            '</div>' +
            '<div class="lp-flex lp-items-center lp-gap-sm">' +
            '<span class="lp-health-label">' + status + '</span>' +
            '<span class="lp-health-score">' + s.ok + '/' + (s.ok + s.fail) + '</span>' +
            '</div></div>';
        });
        healthEl.innerHTML = rows;
      } else {
        healthEl.innerHTML = emptyState('No providers', 'Add models to see provider health.');
      }

      // Errors
      var errList = Array.isArray(errors) ? errors : (errors.errors || []);
      var errEl = $('#dash-errors');
      if (errList.length) {
        var errRows = '';
        errList.slice(0, 8).forEach(function(e) {
          errRows += '<div class="lp-flex lp-justify-between lp-items-center" style="padding:8px 0;border-bottom:1px solid var(--lp-border)">' +
            '<span class="lp-text-sm" style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' +
            escapeHtml(e.message || e.error || JSON.stringify(e)) + '</span>' +
            '<span class="lp-text-muted lp-text-xs" style="flex-shrink:0;margin-left:12px">' + timeAgo(e.timestamp || e.created_at) + '</span></div>';
        });
        errEl.innerHTML = errRows;
      } else {
        errEl.innerHTML = '<div class="lp-text-muted" style="padding:16px 0;text-align:center">No recent errors</div>';
      }

      // Cost by provider
      var costBreakdown = costs.breakdown || costs.models || costs || {};
      var costEl = $('#dash-cost');
      if (typeof costBreakdown === 'object' && Object.keys(costBreakdown).length) {
        var entries = Object.entries(costBreakdown).sort(function(a, b) { return b[1] - a[1]; });
        var maxCost = Math.max.apply(null, entries.map(function(e) { return e[1]; }).concat([1]));
        var costRows = '';
        entries.forEach(function(entry) {
          var model = entry[0], cost = entry[1];
          var pct = (cost / maxCost * 100).toFixed(1);
          costRows += '<div class="lp-bar-row" style="margin-bottom:8px">' +
            '<span class="lp-bar-label">' + escapeHtml(model) + '</span>' +
            '<div class="lp-bar-track"><div class="lp-bar-fill" style="width:' + pct + '%"></div></div>' +
            '<span class="lp-bar-value">' + formatCost(cost) + '</span></div>';
        });
        costEl.innerHTML = costRows;
      } else {
        costEl.innerHTML = '<div class="lp-text-muted" style="padding:16px 0;text-align:center">No cost data for today</div>';
      }
    }
  };

  function statCard(label, value, icon) {
    return '<div class="lp-dashboard-stat">' +
      '<div class="lp-dashboard-stat-icon">' + icon + '</div>' +
      '<div class="lp-dashboard-stat-value">' + value + '</div>' +
      '<div class="lp-dashboard-stat-label">' + escapeHtml(label) + '</div></div>';
  }

  function modelSvg() {
    return '<svg width="20" height="20" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="M10 2L2 6v8l8 4 8-4V6l-8-4z"/></svg>';
  }

  function keySvg() {
    return '<svg width="20" height="20" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><circle cx="8" cy="8" r="3"/><path d="M11 11l5 5"/></svg>';
  }

  function chartSvg() {
    return '<svg width="20" height="20" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><path d="M2 14l4-4 3 3 4-5 4 4"/></svg>';
  }

  function dollarSvg() {
    return '<svg width="20" height="20" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><circle cx="10" cy="10" r="8"/><path d="M10 5v10M7 8h6M7 12h6"/></svg>';
  }

  ns.app && ns.app.registerPage('overview', dashboard);
  ns.dashboard = dashboard;
})(window.llmPico);
