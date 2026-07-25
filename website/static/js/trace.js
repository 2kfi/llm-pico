window.llmPico = window.llmPico || {};

(function(ns) {
  const { $, $$, api, showToast, escapeHtml, spinnerHtml, emptyState } = ns;

  function renderWaterfall(spans, container) {
    if (!spans || !spans.length) {
      container.innerHTML = emptyState('No trace data', 'No spans recorded for this request.');
      return;
    }
    const minMs = Math.min(...spans.map(s => s.start_ms));
    const maxMs = Math.max(...spans.map(s => s.end_ms));
    const range = Math.max(maxMs - minMs, 1);

    let html = '<div class="lp-waterfall">';
    for (let i = 0; i < spans.length; i++) {
      const s = spans[i];
      const left = ((s.start_ms - minMs) / range) * 100;
      const width = Math.max(((s.end_ms - s.start_ms) / range) * 100, 0.5);
      const dur = s.end_ms - s.start_ms;
      const statusClass = s.status === 'ok' ? '' : ' lp-waterfall-bar--error';
      const label = escapeHtml(s.label);
      const model = s.model_name ? ' <span class="lp-waterfall-model">' + escapeHtml(s.model_name) + '</span>' : '';
      const provider = s.provider ? ' <span class="lp-waterfall-provider">(' + escapeHtml(s.provider) + ')</span>' : '';
      html += '<div class="lp-waterfall-row">' +
        '<div class="lp-waterfall-label">' + label + model + provider + '</div>' +
        '<div class="lp-waterfall-track">' +
        '<div class="lp-waterfall-bar' + statusClass + '" style="left:' + left + '%;width:' + width + '%"></div>' +
        '</div>' +
        '<div class="lp-waterfall-dur">' + dur + 'ms</div>' +
        '</div>';
    }
    html += '</div>';
    container.innerHTML = html;
  }

  const TracePage = {
    render: async function(content) {
      content.innerHTML = '<div class="lp-card"><h3 class="lp-card-title">Request Trace</h3>' +
        '<div class="lp-form-row">' +
        '<input type="text" class="lp-input" id="trace-req-id" placeholder="Enter request ID..." style="flex:1">' +
        '<button class="lp-btn lp-btn-primary" id="trace-load-btn">Load Trace</button>' +
        '</div>' +
        '<div id="trace-result" style="margin-top:16px"></div>' +
        '</div>';

      const btn = $('#trace-load-btn');
      const input = $('#trace-req-id');
      if (btn) btn.onclick = async function() {
        const rid = (input.value || '').trim();
        if (!rid) { showToast('Enter a request ID', 'error'); return; }
        const result = $('#trace-result');
        result.innerHTML = spinnerHtml('md');
        try {
          const resp = await api.getTrace(rid);
          renderWaterfall(resp, result);
        } catch (e) {
          result.innerHTML = emptyState('Not found', e.message || 'No trace data for this ID.');
        }
      };
    }
  };

  ns.TracePage = TracePage;
  ns.app && ns.app.registerPage && ns.app.registerPage('trace', TracePage);
})(window.llmPico);
