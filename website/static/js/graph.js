window.llmPico = window.llmPico || {};

(function(ns) {
  const { $, $$, api, showToast, escapeHtml, spinnerHtml, emptyState } = ns;

  function renderGraph(data, container) {
    if (!data || !data.length) {
      container.innerHTML = emptyState('No models', 'No models registered.');
      return;
    }

    // Group by provider
    const providers = {};
    data.forEach(function(m) {
      const prov = m.provider || 'unknown';
      if (!providers[prov]) providers[prov] = [];
      providers[prov].push(m);
    });

    const provKeys = Object.keys(providers).sort();
    const nodeW = 160, nodeH = 52, gapX = 28, gapY = 16, padX = 24, padY = 24;
    const totalW = Math.max(provKeys.length * (nodeW + gapX) + padX * 2, 400);

    // Calculate heights per column
    const colHeights = provKeys.map(function(p) { return providers[p].length * (nodeH + gapY) - gapY; });
    const totalH = Math.max(...colHeights) + padY * 2 + 40;

    let svg = '<svg class="lp-routing-svg" viewBox="0 0 ' + totalW + ' ' + totalH + '" xmlns="http://www.w3.org/2000/svg">';

    // Draw provider headers + model boxes
    provKeys.forEach(function(prov, col) {
      const x = padX + col * (nodeW + gapX);
      const models = providers[prov];
      const colH = models.length * (nodeH + gapY) - gapY;
      const startY = padY + (totalH - 40 - colH) / 2;

      // Provider header
      svg += '<rect x="' + x + '" y="' + (startY - 24) + '" width="' + nodeW + '" height="20" rx="4" fill="var(--lp-primary)" opacity="0.15"/>';
      svg += '<text x="' + (x + nodeW / 2) + '" y="' + (startY - 10) + '" text-anchor="middle" class="lp-svg-label">' + escapeHtml(prov) + '</text>';

      models.forEach(function(m, row) {
        const y = startY + row * (nodeH + gapY);
        const health = m.health_score || 0;
        const fill = health > 0.7 ? 'var(--lp-success)' : health > 0.3 ? 'var(--lp-warning)' : 'var(--lp-danger)';
        const opacity = m.is_active ? '0.12' : '0.04';

        svg += '<rect x="' + x + '" y="' + y + '" width="' + nodeW + '" height="' + nodeH + '" rx="6" fill="var(--lp-surface)" stroke="' + fill + '" stroke-width="1.5" opacity="' + (m.is_active ? 1 : 0.5) + '"/>';
        svg += '<text x="' + (x + 8) + '" y="' + (y + 18) + '" class="lp-svg-text">' + escapeHtml(m.model_name || m.model || '') + '</text>';
        svg += '<text x="' + (x + 8) + '" y="' + (y + 34) + '" class="lp-svg-sub">keys:' + (m.key_count || 0) + '  health:' + Math.round(health * 100) + '%</text>';

        // Health dot
        svg += '<circle cx="' + (x + nodeW - 10) + '" cy="' + (y + 12) + '" r="4" fill="' + fill + '"/>';
      });
    });

    // Draw connection lines between providers (same model name)
    const modelPositions = {};
    provKeys.forEach(function(prov, col) {
      providers[prov].forEach(function(m, row) {
        const key = m.model_name || m.model;
        if (!modelPositions[key]) modelPositions[key] = [];
        modelPositions[key].push({
          x: padX + col * (nodeW + gapX) + nodeW / 2,
          y: padY + (totalH - 40 - colHeights[col]) / 2 + row * (nodeH + gapY) + nodeH / 2,
          col: col,
        });
      });
    });

    Object.keys(modelPositions).forEach(function(name) {
      const pts = modelPositions[name];
      if (pts.length < 2) return;
      pts.sort(function(a, b) { return a.col - b.col; });
      for (let i = 0; i < pts.length - 1; i++) {
        svg += '<line x1="' + pts[i].x + '" y1="' + pts[i].y + '" x2="' + pts[i + 1].x + '" y2="' + pts[i + 1].y + '" stroke="var(--lp-primary)" stroke-width="1" opacity="0.25" stroke-dasharray="4 2"/>';
      }
    });

    svg += '</svg>';
    container.innerHTML = svg;
  }

  const RoutingGraphPage = {
    render: async function(content) {
      content.innerHTML = spinnerHtml('lg');
      try {
        const data = await api.getModels();
        const container = document.createElement('div');
        container.className = 'lp-card';
        container.innerHTML = '<h3 class="lp-card-title">Routing Topology</h3><div id="routing-graph"></div>';
        content.innerHTML = '';
        content.appendChild(container);
        renderGraph(data.models || data, $('#routing-graph', container));
      } catch (e) {
        content.innerHTML = emptyState('Error loading graph', e.message);
      }
    }
  };

  ns.RoutingGraphPage = RoutingGraphPage;
})(window.llmPico);
