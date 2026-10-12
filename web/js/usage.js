window.llmPico = window.llmPico || {};

(function(ns) {
  const { $, api, formatNumber, formatCost, escapeHtml, spinnerHtml, emptyState, formatDate, skeleton } = ns;

  function barChart(rows, max, valueFn) {
    if (!rows.length) return '<div class="lp-text-muted" style="padding:var(--lp-space-6) 0">No data</div>';
    var html = '';
    for (var i = 0; i < rows.length; i++) {
      var name = rows[i].model || rows[i].id || '-';
      var val = valueFn(rows[i]);
      var pct = (val / max * 100).toFixed(1);
      html += '<div class="lp-bar-row">' +
        '<span class="lp-bar-label">' + escapeHtml(name) + '</span>' +
        '<div class="lp-bar-track"><div class="lp-bar-fill" style="width:' + pct + '%"></div></div>' +
        '<span class="lp-bar-value">' + formatNumber(val) + '</span></div>';
    }
    return html;
  }

  function renderStats(c, metrics, errCount) {
    c.innerHTML =
      '<div class="lp-dashboard-stat"><div class="lp-dashboard-stat-label">Total Requests</div><div class="lp-dashboard-stat-value">' + formatNumber(metrics.total_requests || 0) + '</div></div>' +
      '<div class="lp-dashboard-stat"><div class="lp-dashboard-stat-label">Requests Today</div><div class="lp-dashboard-stat-value">' + formatNumber(metrics.requests_today || 0) + '</div></div>' +
      '<div class="lp-dashboard-stat"><div class="lp-dashboard-stat-label">Total Cost</div><div class="lp-dashboard-stat-value">' + formatCost(metrics.total_cost || 0) + '</div></div>' +
      '<div class="lp-dashboard-stat"><div class="lp-dashboard-stat-label">Errors Today</div><div class="lp-dashboard-stat-value">' + formatNumber(metrics.errors_today || errCount) + '</div></div>';
  }

  function renderCharts(models, costs) {
    var modelList = models.models || models || [];
    var costMap = costs.breakdown || costs.models || costs || {};

    var maxReqs = 1;
    for (var i = 0; i < modelList.length; i++) {
      var r = modelList[i].requests || modelList[i].count || 0;
      if (r > maxReqs) maxReqs = r;
    }
    var topHtml = barChart(modelList.slice(0, 10), maxReqs, function(m) { return m.requests || m.count || 0; });

    var costEntries = [];
    var maxCost = 1;
    if (typeof costMap === 'object') {
      var keys = Object.keys(costMap);
      for (var i = 0; i < keys.length; i++) {
        var v = Number(costMap[keys[i]]) || 0;
        costEntries.push({ model: keys[i], _cost: v });
        if (v > maxCost) maxCost = v;
      }
      costEntries.sort(function(a, b) { return b._cost - a._cost; });
    }
    var costHtml = '';
    for (var i = 0; i < costEntries.length && i < 10; i++) {
      var name = costEntries[i].model;
      var val = costEntries[i]._cost;
      var pct = (val / maxCost * 100).toFixed(1);
      costHtml += '<div class="lp-bar-row">' +
        '<span class="lp-bar-label">' + escapeHtml(name) + '</span>' +
        '<div class="lp-bar-track"><div class="lp-bar-fill" style="width:' + pct + '%"></div></div>' +
        '<span class="lp-bar-value">' + formatCost(val) + '</span></div>';
    }

    return '<div class="lp-grid lp-grid-2">' +
      '<div class="lp-card"><div class="lp-card-header"><span class="lp-card-title">Top Models</span></div><div class="lp-card-body">' + topHtml + '</div></div>' +
      '<div class="lp-card" id="usage-cost-chart"><div class="lp-card-header"><span class="lp-card-title">Cost by Model</span></div><div class="lp-card-body">' + costHtml + '</div></div>' +
      '</div>';
  }

  function renderErrors(errList) {
    var html = '<div class="lp-card"><div class="lp-card-header"><span class="lp-card-title">Recent Errors</span></div>';
    if (!errList.length) {
      html += '<div class="lp-card-body">' + emptyState('No errors', 'Everything is running smoothly') + '</div>';
    } else {
      html += '<div class="lp-table-wrap"><table class="lp-table"><thead><tr><th scope="col">Error</th><th scope="col">Model</th><th scope="col">Count</th><th scope="col">Last Seen</th></tr></thead><tbody>';
      for (var i = 0; i < errList.length && i < 20; i++) {
        var e = errList[i];
        html += '<tr><td>' + escapeHtml(e.message || e.error || '-') + '</td>' +
          '<td class="lp-mono">' + escapeHtml(e.model || '-') + '</td>' +
          '<td>' + formatNumber(e.count || 1) + '</td>' +
          '<td class="lp-text-muted">' + formatDate(e.timestamp || e.last_seen) + '</td></tr>';
      }
      html += '</tbody></table></div>';
    }
    html += '</div>';
    return html;
  }

  async function loadData(period) {
    return Promise.all([
      api.getMetrics({ period: period }).catch(function() { return {}; }),
      api.getTopModels({ period: period }).catch(function() { return []; }),
      api.getCosts({ period: period }).catch(function() { return {}; }),
      api.getErrors({ period: period }).catch(function() { return []; })
    ]);
  }

  const usage = {
    async render(container) {
      container.innerHTML =
        '<div class="lp-page-header">' +
          '<h1 class="lp-page-title">Usage & Analytics</h1>' +
          '<label for="usage-period" class="sr-only">Select time period</label>' +
          '<select class="lp-select" id="usage-period">' +
            '<option value="today">Today</option>' +
            '<option value="7d">Last 7 Days</option>' +
            '<option value="30d">Last 30 Days</option>' +
            '<option value="all">All Time</option>' +
          '</select>' +
        '</div>' +
        '<div class="lp-dashboard-grid" id="usage-stats">' + skeleton('stat') + skeleton('stat') + skeleton('stat') + skeleton('stat') + '</div>' +
        '<div id="usage-charts"></div>' +
        '<div id="usage-errors"></div>';

      var period = ($('#usage-period') || {}).value || 'today';
      var parts = await loadData(period);
      var metrics = parts[0], topModels = parts[1], costs = parts[2], errors = parts[3];

      var errList = Array.isArray(errors) ? errors : (errors.errors || []);
      renderStats($('#usage-stats'), metrics, errList.length);
      $('#usage-charts').innerHTML = renderCharts(topModels, costs);
      $('#usage-errors').innerHTML = renderErrors(errList);

      $('#usage-period').addEventListener('change', async function() {
        var p = this.value;
        $('#usage-stats').innerHTML = skeleton('stat') + skeleton('stat') + skeleton('stat') + skeleton('stat');
        $('#usage-charts').innerHTML = '';
        $('#usage-errors').innerHTML = '';

        var data = await loadData(p);
        var m = data[0], tm = data[1], c = data[2], e = data[3];
        var el = Array.isArray(e) ? e : (e.errors || []);
        renderStats($('#usage-stats'), m, el.length);
        $('#usage-charts').innerHTML = renderCharts(tm, c);
        $('#usage-errors').innerHTML = renderErrors(el);
      });
    }
  };

  ns.app && ns.app.registerPage('usage', usage);
  ns.usage = usage;
})(window.llmPico);
