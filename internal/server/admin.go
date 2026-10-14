package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/arkan/llm-pico/internal/auth"
	"github.com/arkan/llm-pico/internal/config"
	"github.com/arkan/llm-pico/internal/httpx"
	"github.com/arkan/llm-pico/internal/org"
	"github.com/arkan/llm-pico/internal/providers"
	"github.com/arkan/llm-pico/internal/store"
	"github.com/arkan/llm-pico/internal/usage"
)

// adminAPI implements the /admin surface: instance configuration, keys, teams,
// users, budgets, observability and the dashboard.
type adminAPI struct {
	server *Server

	// streamTokens authorises the dashboard's EventSource, which cannot send
	// an Authorization header. Each token is single-purpose and short-lived.
	streamMu     sync.Mutex
	streamTokens map[string]time.Time
}

// register wires every admin route.
func (a *adminAPI) register(mux *http.ServeMux) {
	a.streamTokens = map[string]time.Time{}

	// Instance bootstrap. These run before a master key exists.
	mux.HandleFunc("GET /admin/init/status", a.initStatus)
	mux.HandleFunc("POST /admin/init", a.initInstance)
	mux.HandleFunc("POST /admin/auth/init-master-key", a.initMasterKey)
	mux.HandleFunc("POST /admin/auth/verify-master-key", a.verifyMasterKey)

	// Keys.
	mux.HandleFunc("GET /admin/keys", a.admin(a.listKeys))
	mux.HandleFunc("POST /admin/keys", a.admin(a.createKey))
	mux.HandleFunc("DELETE /admin/keys/{prefix}", a.admin(a.deleteKey))
	mux.HandleFunc("PUT /admin/keys/{prefix}/models", a.admin(a.setKeyModels))
	mux.HandleFunc("PUT /admin/keys/{prefix}/limits", a.admin(a.setKeyLimits))
	mux.HandleFunc("PUT /admin/keys/{prefix}/user", a.admin(a.assignKeyUser))
	mux.HandleFunc("GET /admin/keys/{prefix}", a.admin(a.getKey))

	// Teams.
	mux.HandleFunc("GET /admin/teams", a.admin(a.listTeams))
	mux.HandleFunc("POST /admin/teams", a.admin(a.createTeam))
	mux.HandleFunc("GET /admin/teams/{id}", a.admin(a.getTeam))
	mux.HandleFunc("DELETE /admin/teams/{id}", a.admin(a.deleteTeam))
	mux.HandleFunc("PUT /admin/teams/{id}/limits", a.admin(a.setTeamLimits))
	mux.HandleFunc("PUT /admin/teams/{id}/models", a.admin(a.setTeamModels))
	mux.HandleFunc("PUT /admin/teams/{id}/chain", a.admin(a.setTeamChain))
	mux.HandleFunc("GET /admin/teams/{id}/chain", a.admin(a.getTeamChain))
	mux.HandleFunc("PUT /admin/teams/{id}/chain/rewrites", a.admin(a.setChainRewrites))
	mux.HandleFunc("GET /admin/teams/{id}/chain/rewrites", a.admin(a.getChainRewrites))
	mux.HandleFunc("GET /admin/teams/{id}/usage", a.admin(a.teamUsage))
	mux.HandleFunc("GET /admin/teams/{id}/users", a.admin(a.listUsers))
	mux.HandleFunc("POST /admin/teams/{id}/users", a.admin(a.createUser))

	// Users.
	mux.HandleFunc("GET /admin/users/{id}", a.admin(a.getUser))
	mux.HandleFunc("PUT /admin/users/{id}/limits", a.admin(a.setUserLimits))
	mux.HandleFunc("PUT /admin/users/{id}/budget", a.admin(a.setUserBudget))
	mux.HandleFunc("PUT /admin/users/{id}/models", a.admin(a.setUserModels))
	mux.HandleFunc("GET /admin/users/{id}/usage", a.admin(a.userUsage))

	// Models and provider configuration.
	mux.HandleFunc("GET /admin/models", a.admin(a.listModels))
	mux.HandleFunc("POST /admin/models", a.admin(a.createModel))
	mux.HandleFunc("GET /admin/config/models", a.admin(a.listModels))
	mux.HandleFunc("POST /admin/config/models", a.admin(a.createModel))
	mux.HandleFunc("PUT /admin/config/models/{id}", a.admin(a.updateModel))
	mux.HandleFunc("DELETE /admin/config/models/{id}", a.admin(a.deleteModel))
	mux.HandleFunc("POST /admin/config/models/{id}/keys", a.admin(a.addProviderKey))
	mux.HandleFunc("DELETE /admin/config/keys/{id}", a.admin(a.deleteProviderKey))
	mux.HandleFunc("GET /admin/providers", a.admin(a.listProviders))
	mux.HandleFunc("POST /admin/providers/probe", a.admin(a.probeProvider))
	mux.HandleFunc("POST /admin/providers", a.admin(a.defineProvider))

	// Instance settings.
	mux.HandleFunc("GET /admin/config", a.admin(a.getConfig))
	mux.HandleFunc("PUT /admin/config/settings", a.admin(a.updateSettings))
	mux.HandleFunc("POST /admin/config/reload", a.admin(a.reloadConfig))

	// Usage and observability.
	mux.HandleFunc("GET /admin/usage", a.admin(a.usageStats))
	mux.HandleFunc("GET /admin/usage/recent", a.admin(a.usageStats))
	mux.HandleFunc("GET /admin/usage/top-models", a.admin(a.topModels))
	mux.HandleFunc("GET /admin/stats/costs", a.admin(a.costStats))
	mux.HandleFunc("GET /admin/stats/errors", a.admin(a.errorStats))
	mux.HandleFunc("GET /admin/stats/metrics", a.admin(a.metrics))
	mux.HandleFunc("GET /admin/metrics", a.admin(a.metrics))
	mux.HandleFunc("GET /admin/budgets", a.admin(a.budgets))
	mux.HandleFunc("GET /admin/traces/{id}", a.admin(a.trace))
	mux.HandleFunc("GET /admin/graph", a.admin(a.routingGraph))
	mux.HandleFunc("GET /admin/log", a.admin(a.adminLog))

	// Live log stream.
	mux.HandleFunc("POST /admin/logs/stream-token", a.admin(a.issueStreamToken))
	mux.HandleFunc("GET /admin/logs/stream", a.logStream)

	// Degradation.
	mux.HandleFunc("GET /admin/degradation", a.admin(a.getDegradation))
	mux.HandleFunc("POST /admin/degradation", a.admin(a.setDegradation))

	// Capabilities.
	mux.HandleFunc("POST /admin/capabilities/probe", a.admin(a.probeCapabilities))
}

// adminHandler is an endpoint requiring the master key.
type adminHandler func(w http.ResponseWriter, r *http.Request, actor string)

// admin wraps a handler with master-key authentication.
func (a *adminAPI) admin(h adminHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := a.requireMasterKey(r)
		if err != nil {
			err.Write(w)
			return
		}
		h(w, r, actor)
	}
}

// requireMasterKey authenticates an administrative caller.
func (a *adminAPI) requireMasterKey(r *http.Request) (string, *httpx.Error) {
	raw := auth.ExtractBearer(r.Header.Get("Authorization"))
	if raw == "" {
		return "", httpx.Unauthorized("invalid or missing master API key")
	}
	stored := a.server.proxy.Config().GeneralSettings.MasterKey
	if stored == "" {
		return "", httpx.Unauthorized("instance not initialized")
	}
	// The dashboard sends sha256(rawKey) so the raw key never transits the
	// network; the CLI sends the raw key. Both authenticate.
	if auth.SecureEqual(raw, stored) || auth.SecureEqual(raw, auth.HashKey(stored)) {
		return raw, nil
	}
	return "", httpx.Unauthorized("invalid or missing master API key")
}

// log records an administrative action to both the database and the JSONL
// audit trail.
func (a *adminAPI) log(w http.ResponseWriter, r *http.Request, action, actor string, details map[string]any) {
	clientIP := auth.ClientIP(r, a.server.opts.TrustProxyHeaders)
	payload := fmt.Sprint(details)

	if _, err := a.server.db.Exec(r.Context(), `
		INSERT INTO admin_log (action, actor_hash, details, structured_details, client_ip, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		action, actor, payload, payload, clientIP, store.Now()); err != nil {
		a.server.log.Error("admin audit insert failed", "action", action, "error", err)
	}

	entry := map[string]any{"action": action, "actor": truncate(actor, 16)}
	for k, v := range details {
		entry[k] = v
	}
	if clientIP != "" {
		entry["ip"] = clientIP
	}
	a.server.proxy.Audit.Write(entry)
}

// --- instance bootstrap ---

func (a *adminAPI) initStatus(w http.ResponseWriter, r *http.Request) {
	initialized := a.server.proxy.Config().GeneralSettings.MasterKey != ""
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"initialized": initialized})
}

func (a *adminAPI) initInstance(w http.ResponseWriter, r *http.Request) {
	if a.server.proxy.Config().GeneralSettings.MasterKey != "" {
		httpx.BadRequest("instance already initialized").Write(w)
		return
	}
	masterKey := "sk-pico-" + randomHex(32)
	if err := a.persistMasterKey(r, masterKey); err != nil {
		httpx.Internal("could not persist the master key").Write(w)
		return
	}
	a.log(w, r, "init_instance", auth.HashKey(masterKey), nil)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"master_key": masterKey})
}

func (a *adminAPI) initMasterKey(w http.ResponseWriter, r *http.Request) {
	if a.server.proxy.Config().GeneralSettings.MasterKey != "" {
		httpx.BadRequest("instance already initialized").Write(w)
		return
	}
	var body struct {
		KeyHash string `json:"keyHash"`
	}
	if httpx.DecodeJSON(r, &body) != nil {
		httpx.BadRequest("field 'keyHash' is required").Write(w)
		return
	}
	if body.KeyHash == "" {
		httpx.BadRequest("field 'keyHash' is required").Write(w)
		return
	}
	if err := a.persistMasterKey(r, body.KeyHash); err != nil {
		httpx.Internal("could not persist the master key").Write(w)
		return
	}
	a.log(w, r, "auth_init_master_key", body.KeyHash, nil)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (a *adminAPI) persistMasterKey(r *http.Request, key string) error {
	if err := config.SaveSettings(r.Context(), a.server.db, map[string]any{"master_key": key}); err != nil {
		return err
	}
	return a.server.proxy.Reload(r.Context())
}

func (a *adminAPI) verifyMasterKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		KeyHash string `json:"keyHash"`
	}
	if httpx.DecodeJSON(r, &body) != nil || body.KeyHash == "" {
		httpx.BadRequest("field 'keyHash' is required").Write(w)
		return
	}
	stored := a.server.proxy.Config().GeneralSettings.MasterKey
	if stored == "" {
		httpx.Unauthorized("instance not initialized").Write(w)
		return
	}
	if !auth.SecureEqual(body.KeyHash, stored) {
		httpx.Unauthorized("invalid master key").Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- keys ---

func (a *adminAPI) listKeys(w http.ResponseWriter, r *http.Request, _ string) {
	rows, err := a.server.db.Query(r.Context(), `
		SELECT id, key_prefix, label, is_active, created_at, expires_at,
		       model_allowlist, rpm_limit, rpd_limit, tpm_limit, tpd_limit,
		       ash_limit, asd_limit, user_id, ip_allowlist, monthly_budget_usd
		FROM user_keys ORDER BY created_at DESC`)
	if err != nil {
		httpx.Internal("could not list keys").Write(w)
		return
	}
	defer rows.Close()

	keys := []map[string]any{}
	for rows.Next() {
		var (
			id                 int64
			prefix, label      string
			active             bool
			createdAt          string
			expiresAt          *string
			allowlist, ipAllow *string
			rpm, rpd, tpm, tpd *int64
			ash, asd           *int64
			userID             *int64
			budget             *float64
		)
		if err := rows.Scan(&id, &prefix, &label, &active, &createdAt, &expiresAt,
			&allowlist, &rpm, &rpd, &tpm, &tpd, &ash, &asd, &userID, &ipAllow, &budget); err != nil {
			httpx.Internal("could not read a key row").Write(w)
			return
		}
		keys = append(keys, map[string]any{
			"id":                 id,
			"key_prefix":         prefix,
			"label":              label,
			"is_active":          active,
			"created_at":         createdAt,
			"expires_at":         expiresAt,
			"model_allowlist":    decodeAllowlist(allowlist),
			"rpm_limit":          rpm,
			"rpd_limit":          rpd,
			"tpm_limit":          tpm,
			"tpd_limit":          tpd,
			"ash_limit":          ash,
			"asd_limit":          asd,
			"user_id":            userID,
			"ip_allowlist":       decodeAllowlist(ipAllow),
			"monthly_budget_usd": budget,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"keys": keys, "total": len(keys)})
}

func (a *adminAPI) getKey(w http.ResponseWriter, r *http.Request, _ string) {
	prefix := r.PathValue("prefix")
	var id int64
	err := a.server.db.Get(r.Context(), &id,
		`SELECT id FROM user_keys WHERE key_prefix LIKE ? AND is_active = 1`, prefixPattern(prefix))
	if err != nil {
		httpx.NotFound("no active key found with that prefix").Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "key_prefix": prefix})
}

func (a *adminAPI) createKey(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		Label     *string   `json:"label"`
		Models    *[]string `json:"models"`
		ExpiresAt *string   `json:"expires_at"`
		UserID    *int64    `json:"user_id"`
		RPM       *int64    `json:"rpm_limit"`
		RPD       *int64    `json:"rpd_limit"`
		TPM       *int64    `json:"tpm_limit"`
		TPD       *int64    `json:"tpd_limit"`
		ASH       *int64    `json:"ash_limit"`
		ASD       *int64    `json:"asd_limit"`
		Scopes    []string  `json:"scopes"`
		Budget    *float64  `json:"monthly_budget_usd"`
		IPAllow   *[]string `json:"ip_allowlist"`
	}
	if httpx.DecodeJSON(r, &body) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}

	rawKey := "sk-pico-" + randomHex(32)
	hash := auth.HashKey(rawKey)

	_, err := a.server.db.Exec(r.Context(), `
		INSERT INTO user_keys
		  (key_hash, key_prefix, label, is_active, created_at, expires_at,
		   model_allowlist, rpm_limit, rpd_limit, tpm_limit, tpd_limit,
		   ash_limit, asd_limit, user_id, monthly_budget_usd, ip_allowlist)
		VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hash, auth.PrefixFromKey(rawKey), body.Label, store.Now(), body.ExpiresAt,
		encodeAllowlist(body.Models), body.RPM, body.RPD, body.TPM, body.TPD,
		body.ASH, body.ASD, body.UserID, body.Budget, encodeAllowlist(body.IPAllow))
	if err != nil {
		httpx.Internal("could not create the key").Write(w)
		return
	}

	if len(body.Scopes) > 0 {
		var id int64
		if err := a.server.db.Get(r.Context(), &id, `SELECT id FROM user_keys WHERE key_hash = ?`, hash); err == nil {
			if err := auth.SetScopes(r.Context(), a.server.db, id, body.Scopes); err != nil {
				a.server.log.Error("could not set key scopes", "error", err)
			}
		}
	}

	a.log(w, r, "create_key", actor, map[string]any{"key_prefix": auth.PrefixFromKey(rawKey)})

	// The raw key is returned exactly once; only its hash is stored.
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"key":        rawKey,
		"key_prefix": auth.PrefixFromKey(rawKey),
		"label":      body.Label,
	})
}

func (a *adminAPI) deleteKey(w http.ResponseWriter, r *http.Request, actor string) {
	prefix := r.PathValue("prefix")
	res, err := a.server.db.Exec(r.Context(),
		`UPDATE user_keys SET is_active = 0 WHERE key_prefix LIKE ? AND is_active = 1`, prefixPattern(prefix))
	if err != nil {
		httpx.Internal("could not revoke the key").Write(w)
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		httpx.NotFound("no active key found with that prefix").Write(w)
		return
	}
	a.log(w, r, "revoke_key", actor, map[string]any{"key_prefix": prefix, "revoked": affected})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"revoked": affected})
}

func (a *adminAPI) setKeyModels(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		Models *[]string `json:"models"`
	}
	if httpx.DecodeJSON(r, &body) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}
	updated, err := updateAllowlist(r.Context(), a.server.db, "user_keys",
		`key_prefix LIKE ? AND is_active = 1`, []any{prefixPattern(r.PathValue("prefix"))}, body.Models)
	if err != nil {
		httpx.Internal("could not update the key").Write(w)
		return
	}
	if updated == 0 {
		httpx.NotFound("no active key found with that prefix").Write(w)
		return
	}
	a.log(w, r, "set_key_models", actor, map[string]any{"key_prefix": r.PathValue("prefix")})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": updated, "models": body.Models})
}

func (a *adminAPI) setKeyLimits(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		RPM *int64 `json:"rpm"`
		RPD *int64 `json:"rpd"`
		TPM *int64 `json:"tpm"`
		TPD *int64 `json:"tpd"`
		ASH *int64 `json:"ash"`
		ASD *int64 `json:"asd"`
	}
	if httpx.DecodeJSON(r, &body) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}
	res, err := a.server.db.Exec(r.Context(), `
		UPDATE user_keys SET
		  rpm_limit=COALESCE(?, rpm_limit), rpd_limit=COALESCE(?, rpd_limit),
		  tpm_limit=COALESCE(?, tpm_limit), tpd_limit=COALESCE(?, tpd_limit),
		  ash_limit=COALESCE(?, ash_limit), asd_limit=COALESCE(?, asd_limit)
		WHERE key_prefix LIKE ? AND is_active = 1`,
		body.RPM, body.RPD, body.TPM, body.TPD, body.ASH, body.ASD, prefixPattern(r.PathValue("prefix")))
	if err != nil {
		httpx.Internal("could not update the key").Write(w)
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		httpx.NotFound("no active key found with that prefix").Write(w)
		return
	}
	a.log(w, r, "set_key_limits", actor, map[string]any{"key_prefix": r.PathValue("prefix")})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": affected})
}

func (a *adminAPI) assignKeyUser(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		UserID *int64 `json:"user_id"`
	}
	if httpx.DecodeJSON(r, &body) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}
	res, err := a.server.db.Exec(r.Context(),
		`UPDATE user_keys SET user_id = ? WHERE key_prefix LIKE ? AND is_active = 1`,
		body.UserID, prefixPattern(r.PathValue("prefix")))
	if err != nil {
		httpx.Internal("could not assign the key").Write(w)
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		httpx.NotFound("no active key found with that prefix").Write(w)
		return
	}
	a.log(w, r, "assign_key_user", actor, map[string]any{"user_id": body.UserID})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": affected})
}

// --- teams and users ---

func (a *adminAPI) listTeams(w http.ResponseWriter, r *http.Request, _ string) {
	teams, err := org.ListTeams(r.Context(), a.server.db)
	if err != nil {
		httpx.Internal("could not list teams").Write(w)
		return
	}
	if teams == nil {
		teams = []org.Team{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"teams": teams, "total": len(teams)})
}

func (a *adminAPI) createTeam(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		Name        string  `json:"name"`
		Description *string `json:"description"`
	}
	if httpx.DecodeJSON(r, &body) != nil || body.Name == "" {
		httpx.BadRequest("field 'name' is required").Write(w)
		return
	}
	team, err := org.CreateTeam(r.Context(), a.server.db, body.Name, body.Description)
	if err != nil {
		httpx.BadRequest("could not create the team: " + err.Error()).Write(w)
		return
	}
	a.log(w, r, "create_team", actor, map[string]any{"team_id": team.ID, "name": body.Name})
	httpx.WriteJSON(w, http.StatusCreated, team)
}

func (a *adminAPI) getTeam(w http.ResponseWriter, r *http.Request, _ string) {
	team, err := org.LoadTeam(r.Context(), a.server.db, pathInt(r, "id"))
	if err != nil {
		httpx.NotFound("team not found").Write(w)
		return
	}
	spend, err := org.TeamMonthSpend(r.Context(), a.server.db, team.ID)
	if err != nil {
		httpx.Internal("could not read team spend").Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": team.ID, "name": team.Name, "description": team.Description,
		"is_active": team.IsActive, "created_at": team.CreatedAt,
		"model_allowlist": team.Allowlist, "limits": team.LimitsOut,
		"monthly_budget_usd": team.MonthlyBudget,
		"model_chain":        team.ModelChain,
		"month_spend_usd":    spend,
	})
}

func (a *adminAPI) deleteTeam(w http.ResponseWriter, r *http.Request, actor string) {
	ok, err := org.DeactivateTeam(r.Context(), a.server.db, pathInt(r, "id"))
	if err != nil || !ok {
		httpx.NotFound("team not found").Write(w)
		return
	}
	a.log(w, r, "deactivate_team", actor, map[string]any{"team_id": pathInt(r, "id")})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deactivated": true})
}

func (a *adminAPI) setTeamLimits(w http.ResponseWriter, r *http.Request, actor string) {
	var l org.Limits
	if httpx.DecodeJSON(r, &l) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}
	ok, err := org.UpdateTeamLimits(r.Context(), a.server.db, pathInt(r, "id"), l)
	if err != nil || !ok {
		httpx.NotFound("team not found").Write(w)
		return
	}
	a.log(w, r, "update_team_limits", actor, map[string]any{"team_id": pathInt(r, "id")})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": true})
}

func (a *adminAPI) setTeamModels(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		Models *[]string `json:"models"`
	}
	if httpx.DecodeJSON(r, &body) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}
	ok, err := org.SetTeamAllowlist(r.Context(), a.server.db, pathInt(r, "id"), optionalSlice(body.Models))
	if err != nil || !ok {
		httpx.NotFound("team not found").Write(w)
		return
	}
	a.log(w, r, "update_team_models", actor, map[string]any{"team_id": pathInt(r, "id")})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": true, "models": body.Models})
}

func (a *adminAPI) setTeamChain(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		ModelChain *[]string `json:"model_chain"`
	}
	if httpx.DecodeJSON(r, &body) != nil || body.ModelChain == nil {
		httpx.BadRequest("model_chain must be a list of model names").Write(w)
		return
	}
	id := pathInt(r, "id")
	if _, err := org.LoadTeam(r.Context(), a.server.db, id); err != nil {
		httpx.NotFound("team not found").Write(w)
		return
	}
	if err := org.SetModelChain(r.Context(), a.server.db, id, *body.ModelChain); err != nil {
		httpx.Internal("could not save the chain").Write(w)
		return
	}
	// The chain affects routing, so the routing table must reflect it.
	if err := a.server.proxy.Reload(r.Context()); err != nil {
		a.server.log.Error("reload after chain change failed", "error", err)
	}
	a.log(w, r, "set_model_chain", actor, map[string]any{"team_id": id, "model_chain": *body.ModelChain})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"team_id": id, "model_chain": *body.ModelChain})
}

func (a *adminAPI) getTeamChain(w http.ResponseWriter, r *http.Request, _ string) {
	id := pathInt(r, "id")
	chain, err := org.GetModelChain(r.Context(), a.server.db, id)
	if err != nil {
		httpx.NotFound("team not found").Write(w)
		return
	}
	if chain == nil {
		chain = []string{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"team_id": id, "model_chain": chain})
}

func (a *adminAPI) setChainRewrites(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		Rewrites *string `json:"rewrites_response"`
	}
	if httpx.DecodeJSON(r, &body) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}
	id := pathInt(r, "id")
	if err := org.SetChainRewrites(r.Context(), a.server.db, id, body.Rewrites); err != nil {
		httpx.Internal("could not save rewrites").Write(w)
		return
	}
	a.log(w, r, "set_chain_rewrites", actor, map[string]any{"team_id": id})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"team_id": id, "chain_rewrites_response": body.Rewrites})
}

func (a *adminAPI) getChainRewrites(w http.ResponseWriter, r *http.Request, _ string) {
	text, err := org.GetChainRewrites(r.Context(), a.server.db, pathInt(r, "id"))
	if err != nil {
		httpx.NotFound("team not found").Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"team_id": pathInt(r, "id"), "chain_rewrites_response": text,
	})
}

func (a *adminAPI) listUsers(w http.ResponseWriter, r *http.Request, _ string) {
	teamID := int64(pathInt(r, "id"))
	users, err := org.ListUsers(r.Context(), a.server.db, &teamID)
	if err != nil {
		httpx.Internal("could not list users").Write(w)
		return
	}
	if users == nil {
		users = []org.User{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"users": users, "total": len(users)})
}

func (a *adminAPI) createUser(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if httpx.DecodeJSON(r, &body) != nil || body.Email == "" || body.Name == "" {
		httpx.BadRequest("fields 'email' and 'name' are required").Write(w)
		return
	}
	user, err := org.CreateUser(r.Context(), a.server.db, pathInt(r, "id"), body.Email, body.Name)
	if err != nil {
		httpx.BadRequest("could not create the user: " + err.Error()).Write(w)
		return
	}
	a.log(w, r, "create_user", actor, map[string]any{"user_id": user.ID, "email": body.Email})
	httpx.WriteJSON(w, http.StatusCreated, user)
}

func (a *adminAPI) getUser(w http.ResponseWriter, r *http.Request, _ string) {
	user, err := org.LoadUser(r.Context(), a.server.db, pathInt(r, "id"))
	if err != nil {
		httpx.NotFound("user not found").Write(w)
		return
	}
	spend, err := org.UserMonthSpend(r.Context(), a.server.db, user.ID)
	if err != nil {
		httpx.Internal("could not read user spend").Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": user.ID, "team_id": user.TeamID, "email": user.Email, "name": user.Name,
		"is_active": user.IsActive, "created_at": user.CreatedAt,
		"model_allowlist": user.Allowlist, "limits": user.LimitsOut,
		"monthly_budget_usd": user.MonthlyBudget, "month_spend_usd": spend,
	})
}

func (a *adminAPI) setUserLimits(w http.ResponseWriter, r *http.Request, actor string) {
	var l org.Limits
	if httpx.DecodeJSON(r, &l) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}
	ok, err := org.UpdateUserLimits(r.Context(), a.server.db, pathInt(r, "id"), l)
	if err != nil || !ok {
		httpx.NotFound("user not found").Write(w)
		return
	}
	a.log(w, r, "update_user_limits", actor, map[string]any{"user_id": pathInt(r, "id")})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": true})
}

func (a *adminAPI) setUserBudget(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		Budget *float64 `json:"monthly_budget_usd"`
	}
	if httpx.DecodeJSON(r, &body) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}
	ok, err := org.SetUserBudget(r.Context(), a.server.db, pathInt(r, "id"), body.Budget)
	if err != nil || !ok {
		httpx.NotFound("user not found").Write(w)
		return
	}
	a.log(w, r, "update_user_budget", actor, map[string]any{"budget": body.Budget})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": true, "monthly_budget_usd": body.Budget})
}

func (a *adminAPI) setUserModels(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		Models *[]string `json:"models"`
	}
	if httpx.DecodeJSON(r, &body) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}
	ok, err := org.SetUserAllowlist(r.Context(), a.server.db, pathInt(r, "id"), optionalSlice(body.Models))
	if err != nil || !ok {
		httpx.NotFound("user not found").Write(w)
		return
	}
	a.log(w, r, "update_user_models", actor, map[string]any{"user_id": pathInt(r, "id")})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": true, "models": body.Models})
}

// --- models and providers ---

func (a *adminAPI) listModels(w http.ResponseWriter, r *http.Request, _ string) {
	rows, err := a.server.db.Query(r.Context(), `
		SELECT id, model_name, model, api_base, images, embeddings, stt, tts,
		       failover_model, can_cache, cost_per_1m_input, cost_per_1m_output,
		       rpm, rpd, tpm, tpd, ash, asd, is_active
		FROM models ORDER BY id`)
	if err != nil {
		httpx.Internal("could not list models").Write(w)
		return
	}
	defer rows.Close()

	models := []map[string]any{}
	for rows.Next() {
		var (
			id                                       int64
			name, model                              string
			apiBase, failover                        *string
			images, embeddings, stt, tts, cache, act bool
			costIn, costOut                          *float64
			rpm, rpd, tpm, tpd, ash, asd             *int64
		)
		if err := rows.Scan(&id, &name, &model, &apiBase, &images, &embeddings, &stt, &tts,
			&failover, &cache, &costIn, &costOut, &rpm, &rpd, &tpm, &tpd, &ash, &asd, &act); err != nil {
			httpx.Internal("could not read a model row").Write(w)
			return
		}
		keys, err := config.ListProviderKeys(r.Context(), a.server.db, id)
		if err != nil {
			httpx.Internal("could not read provider keys").Write(w)
			return
		}
		if keys == nil {
			keys = []config.ProviderKey{}
		}
		models = append(models, map[string]any{
			"id": id, "model_name": name, "model": model, "api_base": apiBase,
			"images": images, "embeddings": embeddings, "stt": stt, "tts": tts,
			"failover_model": failover, "can_cache": cache,
			"cost_per_1m_input": costIn, "cost_per_1m_output": costOut,
			"rpm": rpm, "rpd": rpd, "tpm": tpm, "tpd": tpd, "ash": ash, "asd": asd,
			"is_active": act, "provider_keys": keys,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"models": models, "total": len(models)})
}

// modelInputBody is the create/update model payload: the model fields plus the
// provider key rotation pool, which is stored in a separate table.
type modelInputBody struct {
	config.ModelInput
	APIKeys []string `json:"api_keys"`
}

func (a *adminAPI) createModel(w http.ResponseWriter, r *http.Request, actor string) {
	var body modelInputBody
	if httpx.DecodeJSON(r, &body) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}
	if body.ModelName == "" || body.Model == "" {
		httpx.BadRequest("fields 'model_name' and 'model' are required").Write(w)
		return
	}

	id, err := config.SaveModel(r.Context(), a.server.db, 0, body.ModelInput)
	if err != nil {
		httpx.BadRequest("could not create the model: " + err.Error()).Write(w)
		return
	}

	for i, key := range body.APIKeys {
		if key == "" {
			continue
		}
		if _, err := config.AddProviderKey(r.Context(), a.server.db, id, key, i); err != nil {
			a.server.log.Error("could not add provider key", "model_id", id, "error", err)
		}
	}

	if err := a.server.proxy.Reload(r.Context()); err != nil {
		a.server.log.Error("reload after model create failed", "error", err)
	}
	a.log(w, r, "create_model", actor, map[string]any{"model_id": id, "model_name": body.ModelName})
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "model_name": body.ModelName})
}

func (a *adminAPI) updateModel(w http.ResponseWriter, r *http.Request, actor string) {
	var in config.ModelInput
	if httpx.DecodeJSON(r, &in) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}
	id, err := config.SaveModel(r.Context(), a.server.db, pathInt(r, "id"), in)
	if err != nil {
		httpx.NotFound("model not found").Write(w)
		return
	}
	if err := a.server.proxy.Reload(r.Context()); err != nil {
		a.server.log.Error("reload after model update failed", "error", err)
	}
	a.log(w, r, "update_model", actor, map[string]any{"model_id": id})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": true})
}

func (a *adminAPI) deleteModel(w http.ResponseWriter, r *http.Request, actor string) {
	ok, err := config.DeleteModel(r.Context(), a.server.db, pathInt(r, "id"))
	if err != nil || !ok {
		httpx.NotFound("model not found").Write(w)
		return
	}
	if err := a.server.proxy.Reload(r.Context()); err != nil {
		a.server.log.Error("reload after model delete failed", "error", err)
	}
	a.log(w, r, "delete_model", actor, map[string]any{"model_id": pathInt(r, "id")})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (a *adminAPI) addProviderKey(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		APIKey   string `json:"api_key"`
		Priority int    `json:"priority"`
	}
	if httpx.DecodeJSON(r, &body) != nil || body.APIKey == "" {
		httpx.BadRequest("field 'api_key' is required").Write(w)
		return
	}
	id, err := config.AddProviderKey(r.Context(), a.server.db, pathInt(r, "id"), body.APIKey, body.Priority)
	if err != nil {
		httpx.Internal("could not add the key").Write(w)
		return
	}
	if err := a.server.proxy.Reload(r.Context()); err != nil {
		a.server.log.Error("reload after key add failed", "error", err)
	}
	a.log(w, r, "add_provider_key", actor, map[string]any{"model_id": pathInt(r, "id"), "key_id": id})
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (a *adminAPI) deleteProviderKey(w http.ResponseWriter, r *http.Request, actor string) {
	ok, err := config.DeleteProviderKey(r.Context(), a.server.db, pathInt(r, "id"))
	if err != nil || !ok {
		httpx.NotFound("key not found").Write(w)
		return
	}
	if err := a.server.proxy.Reload(r.Context()); err != nil {
		a.server.log.Error("reload after key delete failed", "error", err)
	}
	a.log(w, r, "delete_provider_key", actor, map[string]any{"key_id": pathInt(r, "id")})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (a *adminAPI) listProviders(w http.ResponseWriter, r *http.Request, _ string) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"builtin": providers.Catalog(),
		"defined": a.server.proxy.Providers.Definitions(),
	})
}

// defineProvider registers a provider without a rebuild, which is what makes
// adding a new vendor a configuration change rather than a code change.
func (a *adminAPI) defineProvider(w http.ResponseWriter, r *http.Request, actor string) {
	var d providers.Definition
	if httpx.DecodeJSON(r, &d) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}
	if err := a.server.proxy.Providers.Define(d); err != nil {
		httpx.BadRequest(err.Error()).Write(w)
		return
	}
	a.log(w, r, "define_provider", actor, map[string]any{"slug": d.Slug, "protocol": string(d.Protocol)})
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"slug": d.Slug, "defined": true})
}

func (a *adminAPI) probeProvider(w http.ResponseWriter, r *http.Request, _ string) {
	var body struct {
		Provider  string `json:"provider"`
		APIKey    string `json:"api_key"`
		BaseURL   string `json:"base_url"`
		AccountID string `json:"account_id"`
	}
	if httpx.DecodeJSON(r, &body) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}
	if body.APIKey == "" {
		httpx.BadRequest("field 'api_key' is required").Write(w)
		return
	}
	base := body.BaseURL
	if base == "" && body.AccountID != "" {
		base = "https://api.cloudflare.com/client/v4/accounts/" + body.AccountID + "/ai/v1"
	}

	models, err := a.server.proxy.Providers.FetchModels(r.Context(), body.Provider, body.APIKey, base)
	if err != nil {
		httpx.Upstream(http.StatusBadGateway, err.Error()).Write(w)
		return
	}
	out := make([]map[string]string, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]string{"id": m.ID, "name": m.Name})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"models": out})
}

func (a *adminAPI) probeCapabilities(w http.ResponseWriter, r *http.Request, _ string) {
	var body struct {
		ModelName string `json:"model_name"`
		Provider  string `json:"provider"`
		APIKey    string `json:"api_key"`
		BaseURL   string `json:"api_base"`
	}
	if httpx.DecodeJSON(r, &body) != nil || body.ModelName == "" {
		httpx.BadRequest("field 'model_name' is required").Write(w)
		return
	}
	slug := body.Provider
	if slug == "" {
		entry, ok := a.server.proxy.Router().Entry(body.ModelName)
		if !ok {
			httpx.NotFound("model not configured").Write(w)
			return
		}
		slug = entry.ProviderSlug()
	}

	adapter := a.server.proxy.Providers.AdapterFor(slug, body.BaseURL)
	caps, err := adapter.Probe(r.Context(), providers.Request{
		Model:  body.ModelName,
		APIKey: body.APIKey,
	})
	if err != nil {
		httpx.Upstream(http.StatusBadGateway, err.Error()).Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, caps)
}

// --- settings ---

func (a *adminAPI) getConfig(w http.ResponseWriter, r *http.Request, _ string) {
	cfg := a.server.proxy.Config()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"general_settings": map[string]any{
			"master_key":               cfg.GeneralSettings.MasterKey,
			"usage_log_retention_days": cfg.GeneralSettings.UsageLogRetentionDays,
			"admin_log_retention_days": cfg.GeneralSettings.AdminLogRetentionDays,
			"hmac_enabled":             cfg.GeneralSettings.HMACEnabled,
		},
		"router_settings": map[string]any{
			"num_retries":   cfg.RouterSettings.NumRetries,
			"cooldown_time": cfg.RouterSettings.CooldownTime,
			"circuit_breaker": map[string]any{
				"enabled":           cfg.RouterSettings.CircuitBreaker.Enabled,
				"failure_threshold": cfg.RouterSettings.CircuitBreaker.FailureThreshold,
				"recovery_timeout":  cfg.RouterSettings.CircuitBreaker.RecoveryTimeout,
			},
		},
		"models":      len(cfg.ModelList),
		"degradation": string(a.server.proxy.Mode()),
	})
}

func (a *adminAPI) updateSettings(w http.ResponseWriter, r *http.Request, actor string) {
	body, err := readBody(r)
	if err != nil {
		httpx.BadRequest("could not read the request body").Write(w)
		return
	}
	if err := config.SaveSettingsJSON(r.Context(), a.server.db, body); err != nil {
		httpx.BadRequest(err.Error()).Write(w)
		return
	}
	if err := a.server.proxy.Reload(r.Context()); err != nil {
		// Settings were written but could not be applied; report it rather
		// than pretending the change took effect.
		httpx.Internal("settings saved but the reload failed: " + err.Error()).Write(w)
		return
	}
	a.log(w, r, "update_settings", actor, nil)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"updated": true})
}

// reloadConfig rebuilds the routing table from the database.
//
// Unlike the Python implementation, a failed reload leaves the running
// configuration serving traffic and reports the error. The Python version
// called os.execve here, restarting the process and dropping in-flight
// streams whenever the database was briefly unavailable.
func (a *adminAPI) reloadConfig(w http.ResponseWriter, r *http.Request, actor string) {
	if err := a.server.proxy.Reload(r.Context()); err != nil {
		httpx.Internal("reload failed: " + err.Error()).Write(w)
		return
	}
	a.log(w, r, "config_reload", actor, map[string]any{"models": len(a.server.proxy.Config().ModelList)})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"reloaded": true,
		"models":   len(a.server.proxy.Config().ModelList),
	})
}

// --- usage and observability ---

func (a *adminAPI) usageStats(w http.ResponseWriter, r *http.Request, _ string) {
	summary, details, err := usage.Stats(r.Context(), a.server.db, queryFrom(r))
	if err != nil {
		httpx.Internal("could not read usage").Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"summary": summary, "details": details})
}

func (a *adminAPI) topModels(w http.ResponseWriter, r *http.Request, _ string) {
	models, err := usage.TopModels(r.Context(), a.server.db,
		r.URL.Query().Get("from"), r.URL.Query().Get("to"), queryInt(r, "limit", 10))
	if err != nil {
		httpx.Internal("could not read usage").Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (a *adminAPI) costStats(w http.ResponseWriter, r *http.Request, _ string) {
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = "user"
	}
	switch groupBy {
	case "user", "model", "day":
	default:
		httpx.BadRequest("group_by must be 'user', 'model', or 'day'").Write(w)
		return
	}
	groups, err := usage.CostStats(r.Context(), a.server.db, groupBy,
		r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		httpx.Internal("could not read costs").Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"costs_by_" + groupBy: groups})
}

func (a *adminAPI) errorStats(w http.ResponseWriter, r *http.Request, _ string) {
	stats, err := usage.ErrorStats(r.Context(), a.server.db,
		r.URL.Query().Get("provider"), r.URL.Query().Get("model"),
		r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		httpx.Internal("could not read error stats").Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"errors": stats})
}

func (a *adminAPI) metrics(w http.ResponseWriter, r *http.Request, _ string) {
	body, err := a.prometheus(r)
	if err != nil {
		httpx.Internal("could not render metrics").Write(w)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

func (a *adminAPI) budgets(w http.ResponseWriter, r *http.Request, _ string) {
	rows, err := a.server.db.Query(r.Context(), `
		SELECT u.id, u.name, u.email, u.monthly_budget_usd,
		       t.name AS team_name, t.id AS team_id,
		       COALESCE(SUM(ul.cost_usd), 0) AS current_spend
		FROM users u
		JOIN teams t ON u.team_id = t.id
		LEFT JOIN user_keys uk ON uk.user_id = u.id
		LEFT JOIN usage_log ul ON ul.key_hash = uk.key_hash
		  AND ul.created_at >= ? AND ul.cost_usd IS NOT NULL
		WHERE u.is_active = 1
		GROUP BY u.id
		ORDER BY t.name, u.name`, org.MonthStart())
	if err != nil {
		httpx.Internal("could not read budgets").Write(w)
		return
	}
	defer rows.Close()

	users := []map[string]any{}
	for rows.Next() {
		var (
			id, teamID            int64
			name, email, teamName string
			budget, spend         *float64
		)
		if err := rows.Scan(&id, &name, &email, &budget, &teamName, &teamID, &spend); err != nil {
			httpx.Internal("could not read a budget row").Write(w)
			return
		}
		users = append(users, map[string]any{
			"id": id, "name": name, "email": email,
			"team_id": teamID, "team_name": teamName,
			"monthly_budget_usd": budget, "current_spend": spend,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (a *adminAPI) trace(w http.ResponseWriter, r *http.Request, _ string) {
	spans, err := a.traces(r.Context(), r.PathValue("id"))
	if err != nil {
		httpx.Internal("could not read the trace").Write(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, spans)
}

func (a *adminAPI) routingGraph(w http.ResponseWriter, r *http.Request, _ string) {
	nodes := []map[string]any{}
	for _, name := range a.server.proxy.Router().ModelNames() {
		entry, _ := a.server.proxy.Router().Entry(name)
		health := []map[string]any{}
		for _, st := range a.server.proxy.Router().Status(name) {
			health = append(health, map[string]any{
				"provider": st.ProviderSlug, "api_base": st.APIBase,
				"health": st.Health, "breaker": st.Breaker,
				"keys": st.KeyCount, "keys_cooling": st.KeysCooling,
				"latency_p50_ms": st.LatencyP50MS, "latency_p95_ms": st.LatencyP95MS,
			})
		}
		sort.Slice(health, func(i, j int) bool {
			return health[i]["provider"].(string) < health[j]["provider"].(string)
		})
		nodes = append(nodes, map[string]any{
			"model": name, "provider": entry.ProviderSlug(),
			"upstream": entry.ModelParams.Model, "health": health,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

func (a *adminAPI) adminLog(w http.ResponseWriter, r *http.Request, _ string) {
	rows, err := a.server.db.Query(r.Context(), `
		SELECT action, actor_hash, details, created_at FROM admin_log
		ORDER BY created_at DESC LIMIT ?`, queryInt(r, "limit", 50))
	if err != nil {
		httpx.Internal("could not read the admin log").Write(w)
		return
	}
	defer rows.Close()

	entries := []map[string]any{}
	for rows.Next() {
		var action, actor, createdAt string
		var details *string
		if err := rows.Scan(&action, &actor, &details, &createdAt); err != nil {
			break
		}
		entry := map[string]any{
			"action": action, "actor_hash": actor,
			"created_at": createdAt, "details": nil,
		}
		if details != nil {
			var parsed any
			if jsonUnmarshal([]byte(*details), &parsed) == nil {
				entry["details"] = parsed
			} else {
				entry["details"] = *details
			}
		}
		entries = append(entries, entry)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"entries": entries, "total": len(entries)})
}

// --- live log stream ---

// streamTokenTTL bounds how long a dashboard stream token stays valid.
const streamTokenTTL = 5 * time.Minute

func (a *adminAPI) issueStreamToken(w http.ResponseWriter, r *http.Request, _ string) {
	token := randomHex(16)

	a.streamMu.Lock()
	now := time.Now()
	for t, exp := range a.streamTokens {
		if exp.Before(now) {
			delete(a.streamTokens, t)
		}
	}
	a.streamTokens[token] = now.Add(streamTokenTTL)
	a.streamMu.Unlock()

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"stream_token": token})
}

// logStream serves the dashboard's live event feed.
//
// EventSource cannot send an Authorization header, so a short-lived token from
// issueStreamToken is accepted in the query string instead.
func (a *adminAPI) logStream(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token != "" {
		a.streamMu.Lock()
		expiry, ok := a.streamTokens[token]
		if ok && expiry.After(time.Now()) {
			delete(a.streamTokens, token)
		} else {
			ok = false
		}
		a.streamMu.Unlock()
		if !ok {
			httpx.Unauthorized("invalid or expired stream token").Write(w)
			return
		}
	} else if _, err := a.requireMasterKey(r); err != nil {
		err.Write(w)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		httpx.Internal("streaming is not supported by this connection").Write(w)
		return
	}

	events, cancel := a.server.proxy.Events.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := w.Write([]byte("data: {\"type\":\"keepalive\"}\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case payload, open := <-events:
			if !open {
				return
			}
			if _, err := w.Write(append([]byte("data: "), append(payload, '\n', '\n')...)); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// --- degradation ---

func (a *adminAPI) getDegradation(w http.ResponseWriter, r *http.Request, _ string) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"mode": string(a.server.proxy.Mode()),
	})
}

func (a *adminAPI) setDegradation(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		Mode string `json:"mode"`
	}
	if httpx.DecodeJSON(r, &body) != nil {
		httpx.BadRequest("invalid JSON body").Write(w)
		return
	}
	mode := proxyDegradation(body.Mode)
	if mode == "" {
		httpx.BadRequest("invalid mode: use normal, reject or fallback_only").Write(w)
		return
	}
	a.server.proxy.SetMode(mode)
	a.log(w, r, "set_degradation", actor, map[string]any{"mode": body.Mode})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"mode": body.Mode})
}

// --- helpers ---

func prefixPattern(prefix string) string {
	if i := lastIndex(prefix, "..."); i >= 0 {
		prefix = prefix[:i]
	}
	return prefix + "%"
}

func lastIndex(s, sub string) int {
	for i := len(s) - len(sub); i >= 0; i-- {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func decodeAllowlist(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	var out any
	if jsonUnmarshal([]byte(*s), &out) != nil {
		return nil
	}
	return out
}

func encodeAllowlist(s *[]string) *string {
	if s == nil {
		return nil
	}
	b, err := jsonMarshal(*s)
	if err != nil {
		return nil
	}
	out := string(b)
	return &out
}

func optionalSlice(s *[]string) []string {
	if s == nil {
		return nil
	}
	return *s
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the platform entropy source is broken;
		// a predictable master key would be worse than a startup error.
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func pathInt(r *http.Request, name string) int64 {
	var v int64
	for _, c := range r.PathValue(name) {
		if c < '0' || c > '9' {
			return 0
		}
		v = v*10 + int64(c-'0')
	}
	return v
}

func queryInt(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}
