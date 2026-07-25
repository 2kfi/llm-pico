window.llmPico = window.llmPico || {};

(function(ns) {
  var $ = ns.$;
  var api = ns.api;
  var showToast = ns.showToast;
  var showModal = ns.showModal;
  var escapeHtml = ns.escapeHtml;
  var formatNumber = ns.formatNumber;
  var formatCost = ns.formatCost;
  var spinnerHtml = ns.spinnerHtml;
  var emptyState = ns.emptyState;

  var teams = {
    render: function(container) {
      container.innerHTML =
        '<div class="lp-page-header">' +
          '<h1 class="lp-page-title">Teams</h1>' +
          '<div class="lp-flex lp-gap-sm">' +
            '<button class="lp-btn lp-btn-primary" id="add-team-btn">+ Team</button>' +
          '</div>' +
        '</div>' +
        '<div id="teams-content">' + spinnerHtml('lg') + '</div>';

      $('#add-team-btn').addEventListener('click', function() { teams.createTeam(); });

      api.getTeams().then(function(res) {
        var list = res.teams || res || [];
        if (!list.length) {
          $('#teams-content').innerHTML = emptyState('No teams', 'Create a team to organize users.');
          return;
        }
        renderTeams(list);
      }).catch(function(e) {
        $('#teams-content').innerHTML = emptyState('Error loading teams', e.message);
      });
    },

    createTeam: function() {
      var body =
        '<div class="lp-input-group">' +
          '<label class="lp-label">Team Name</label>' +
          '<input class="lp-input" id="t-form-name" placeholder="Engineering">' +
        '</div>' +
        '<div class="lp-input-group">' +
          '<label class="lp-label">Budget (USD, 0=unlimited)</label>' +
          '<input class="lp-input" type="number" id="t-form-budget" step="0.01" value="0">' +
        '</div>';

      showModal('Create Team', body, async function() {
        var data = {
          name: $('#t-form-name').value.trim(),
          budget: parseFloat($('#t-form-budget').value) || 0
        };
        if (!data.name) { throw new Error('Team name is required'); }
        await api.createTeam(data);
        showToast('Team created', 'success');
        ns.app.showPage('teams');
      });
    },

    showUsers: function(teamId) {
      var el = document.getElementById('team-users-' + teamId);
      if (!el) return;
      if (el.style.display !== 'none') {
        el.style.display = 'none';
        return;
      }
      el.style.display = '';
      if (el.dataset.loaded) return;
      el.innerHTML = spinnerHtml();
      api.getUsers(teamId).then(function(res) {
        var list = res.users || res || [];
        if (!list.length) {
          el.innerHTML = '<div class="lp-text-muted lp-text-sm" style="padding:8px">No users</div>';
          el.dataset.loaded = '1';
          return;
        }
        var html = '<table class="lp-table" style="font-size:var(--lp-font-size-sm)">' +
          '<thead><tr><th scope="col">Name</th><th scope="col">Email</th><th scope="col">Budget</th><th scope="col">Actions</th></tr></thead><tbody>';
        list.forEach(function(u) {
          html += '<tr>' +
            '<td>' + escapeHtml(u.name || u.username || '-') + '</td>' +
            '<td>' + escapeHtml(u.email || '-') + '</td>' +
            '<td>' + (u.budget ? formatCost(u.budget) : '-') + '</td>' +
            '<td><button class="lp-btn lp-btn-sm" onclick="llmPico.teams.editUserBudget(\'' + escapeHtml(u.id || u.user_id) + '\')">Budget</button></td>' +
            '</tr>';
        });
        html += '</tbody></table>';
        el.innerHTML = html;
        el.dataset.loaded = '1';
      }).catch(function(e) {
        el.innerHTML = '<div class="lp-text-red lp-text-sm" style="padding:8px">' + escapeHtml(e.message) + '</div>';
      });
    },

    addUser: function(teamId) {
      var body =
        '<div class="lp-input-group">' +
          '<label class="lp-label">Username</label>' +
          '<input class="lp-input" id="u-form-name" placeholder="alice">' +
        '</div>' +
        '<div class="lp-input-group">' +
          '<label class="lp-label">Email</label>' +
          '<input class="lp-input" id="u-form-email" placeholder="alice@example.com">' +
        '</div>' +
        '<div class="lp-input-group">' +
          '<label class="lp-label">Budget (USD, 0=unlimited)</label>' +
          '<input class="lp-input" type="number" id="u-form-budget" step="0.01" value="0">' +
        '</div>';

      showModal('Add User to Team', body, async function() {
        var data = {
          name: $('#u-form-name').value.trim(),
          email: $('#u-form-email').value.trim(),
          budget: parseFloat($('#u-form-budget').value) || 0
        };
        if (!data.name) { throw new Error('Username is required'); }
        await api.createUsers(teamId, data);
        showToast('User added', 'success');
        ns.app.showPage('teams');
      });
    },

    editUserBudget: function(userId) {
      var body =
        '<div class="lp-input-group">' +
          '<label class="lp-label">Budget (USD, 0=unlimited)</label>' +
          '<input class="lp-input" type="number" id="u-budget-val" step="0.01" value="0">' +
        '</div>';
      showModal('Set User Budget', body, async function() {
        await api.updateUserLimits(userId, { budget: parseFloat($('#u-budget-val').value) || 0 });
        showToast('Budget updated', 'success');
      });
    }
  };

  function renderTeams(list) {
    var html = '<div class="lp-grid lp-grid-2">';
    list.forEach(function(t) {
      var usage = t.usage || 0;
      var budget = t.budget || 0;
      var pct = budget > 0 ? Math.min(100, (usage / budget * 100)) : 0;
      var id = escapeHtml(t.id);

      html +=
        '<div class="lp-card">' +
          '<div class="lp-card-header">' +
            '<span class="lp-card-title">' + escapeHtml(t.name) + '</span>' +
            '<div class="lp-flex lp-gap-sm">' +
              '<button class="lp-btn lp-btn-sm" onclick="llmPico.teams.showUsers(\'' + id + '\')">Users</button>' +
              '<button class="lp-btn lp-btn-sm" onclick="llmPico.teams.addUser(\'' + id + '\')">+ User</button>' +
            '</div>' +
          '</div>' +
          '<div class="lp-card-body">' +
            '<div class="lp-flex lp-justify-between lp-mb-md">' +
              '<span class="lp-text-sm">Users: ' + formatNumber(t.user_count || t.users_count || 0) + '</span>' +
              '<span class="lp-text-sm">Budget: ' + (budget > 0 ? formatCost(budget) : 'Unlimited') + '</span>' +
            '</div>';

      if (budget > 0) {
        html +=
          '<div class="lp-progress">' +
            '<div class="lp-progress-fill" style="width:' + pct + '%"></div>' +
          '</div>' +
          '<div class="lp-flex lp-justify-between" style="margin-top:var(--lp-space-xs)">' +
            '<span class="lp-text-xs lp-text-muted">' + formatCost(usage) + ' used</span>' +
            '<span class="lp-text-xs lp-text-muted">' + pct.toFixed(1) + '%</span>' +
          '</div>';
      }

      html +=
          '</div>' +
          '<div id="team-users-' + id + '" class="lp-card-body" style="border-top:1px solid var(--lp-border);display:none"></div>' +
        '</div>';
    });
    html += '</div>';
    $('#teams-content').innerHTML = html;
  }

  ns.app && ns.app.registerPage('teams', teams);
  ns.app && ns.app.registerPage('users', teams);
  ns.teams = teams;
})(window.llmPico);
