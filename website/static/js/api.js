window.llmPico = window.llmPico || {};

(function(ns) {
  class Api {
    constructor() {
      this._session = ns.getSession();
    }

    _headers() {
      const h = { 'Content-Type': 'application/json' };
      if (this._session) h['Authorization'] = 'Bearer ' + this._session.token;
      return h;
    }

    _refreshSession() {
      this._session = ns.getSession();
    }

    async _request(method, path, body) {
      this._refreshSession();
      const opts = { method, headers: this._headers() };
      if (body != null) opts.body = JSON.stringify(body);
      const res = await fetch('/admin' + path, opts);
      if (!res.ok) {
        const text = await res.text().catch(() => '');
        let msg = 'Request failed (' + res.status + ')';
        try { const j = JSON.parse(text); msg = j.error || j.message || msg; } catch {}
        ns.showToast(msg, 'error');
        throw new Error(msg);
      }
      if (res.status === 204) return null;
      return res.json();
    }

    // Auth
    async initInstance() {
      const body = await this._request('POST', '/init');
      const keyHash = await ns.hashKey(body.master_key);
      ns.storeSession(keyHash, keyHash);
      this._refreshSession();
      return body;
    }

    async initMasterKey(rawKey) {
      const keyHash = await ns.hashKey(rawKey);
      const body = await this._request('POST', '/auth/init-master-key', { keyHash });
      ns.storeSession(keyHash, body.token);
      this._refreshSession();
      return body;
    }

    async verifyMasterKey(rawKey) {
      const keyHash = await ns.hashKey(rawKey);
      const body = await this._request('POST', '/auth/verify-master-key', { keyHash });
      ns.storeSession(keyHash, body.token);
      this._refreshSession();
      return body;
    }

    async getInitStatus() {
      const res = await fetch('/admin/init/status');
      return res.json();
    }

    // Config
    async getConfig() { return this._request('GET', '/config'); }
    async updateSettings(settings) { return this._request('PUT', '/config/settings', settings); }
    async reloadConfig() { return this._request('POST', '/config/reload'); }

    // Models
    async getModels() { return this._request('GET', '/config/models'); }
    async createModel(data) { return this._request('POST', '/config/models', data); }
    async updateModel(id, data) { return this._request('PUT', '/config/models/' + id, data); }
    async deleteModel(id) { return this._request('DELETE', '/config/models/' + id); }

    // Provider Keys
    async addProviderKey(modelId, data) { return this._request('POST', '/config/models/' + modelId + '/keys', data); }
    async deleteProviderKey(keyId) { return this._request('DELETE', '/config/keys/' + keyId); }

    // Probe
    async probeProvider(data) { return this._request('POST', '/providers/probe', data); }

    // User Keys
    async getKeys() { return this._request('GET', '/keys'); }
    async createKey(data) { return this._request('POST', '/keys', data); }
    async deleteKey(prefix) { return this._request('DELETE', '/keys/' + prefix); }
    async updateKeyModels(prefix, models) { return this._request('PUT', '/keys/' + prefix + '/models', { models }); }
    async updateKeyLimits(prefix, limits) { return this._request('PUT', '/keys/' + prefix + '/limits', limits); }

    // Teams
    async getTeams() { return this._request('GET', '/teams'); }
    async createTeam(data) { return this._request('POST', '/teams', data); }
    async getTeam(id) { return this._request('GET', '/teams/' + id); }
    async updateTeamLimits(id, limits) { return this._request('PUT', '/teams/' + id + '/limits', limits); }
    async deleteTeam(id) { return this._request('DELETE', '/teams/' + id); }

    // Users
    async createUsers(teamId, data) { return this._request('POST', '/teams/' + teamId + '/users', data); }
    async getUsers(teamId) { return this._request('GET', '/teams/' + teamId + '/users'); }
    async getUser(userId) { return this._request('GET', '/users/' + userId); }
    async updateUserLimits(userId, limits) { return this._request('PUT', '/users/' + userId + '/limits', limits); }
    async updateUserBudget(userId, budget) { return this._request('PUT', '/users/' + userId + '/budget', budget); }

    // Usage/Stats
    async getUsage(params) { return this._request('GET', '/usage' + this._qs(params)); }
    async getTopModels(params) { return this._request('GET', '/usage/top-models' + this._qs(params)); }
    async getCosts(params) { return this._request('GET', '/stats/costs' + this._qs(params)); }
    async getErrors(params) { return this._request('GET', '/stats/errors' + this._qs(params)); }
    async getMetrics() { return this._request('GET', '/stats/metrics'); }
    async getBudgets() { return this._request('GET', '/budgets'); }

    // Traces
    async getTrace(requestId) { return this._request('GET', '/traces/' + encodeURIComponent(requestId)); }

    _qs(params) {
      if (!params) return '';
      const s = new URLSearchParams(params).toString();
      return s ? '?' + s : '';
    }
  }

  ns.api = new Api();
})(window.llmPico);
