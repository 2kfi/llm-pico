// Package config loads and persists the proxy's runtime configuration.
//
// All configuration lives in SQLite; there are no YAML or JSON config files.
// Config is loaded once at startup and swapped atomically when an admin calls
// the reload endpoint, so in-flight requests never observe a partial update.
package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/arkan/llm-pico/internal/store"
)

// CircuitBreakerSettings controls when a provider is taken out of rotation.
type CircuitBreakerSettings struct {
	Enabled          bool `json:"enabled"`
	FailureThreshold int  `json:"failure_threshold"`
	RecoveryTimeout  int  `json:"recovery_timeout"` // seconds
}

// RouterSettings controls retry and failover behaviour.
type RouterSettings struct {
	NumRetries     int                    `json:"num_retries"`
	CooldownTime   int                    `json:"cooldown_time"`
	CircuitBreaker CircuitBreakerSettings `json:"circuit_breaker"`
}

// GeneralSettings holds instance-wide settings.
type GeneralSettings struct {
	MasterKey             string `json:"master_key"`
	DBPath                string `json:"db_path"`
	UsageLogRetentionDays int    `json:"usage_log_retention_days"`
	AdminLogRetentionDays int    `json:"admin_log_retention_days"`
	HMACEnabled           bool   `json:"hmac_enabled"`
	HMACSecret            string `json:"hmac_secret"`
}

// ModelParams identifies the upstream provider and credentials for a model.
type ModelParams struct {
	// Model is the provider-qualified model string, e.g. "openai/gpt-4". The
	// prefix selects the adapter; the remainder is sent upstream.
	Model   string
	APIKey  string   // single key, when only one is configured
	APIKeys []string // rotation pool, when several are configured
	APIBase string
}

// ModelEntry is one configured model plus its routing, limit and cost metadata.
type ModelEntry struct {
	ModelName     string
	ModelParams   ModelParams
	RPM           *int
	RPD           *int
	TPM           *int
	TPD           *int
	ASH           *int
	ASD           *int
	Images        bool
	Embeddings    bool
	STT           bool
	TTS           bool
	FailoverModel string
	CanCache      bool
	CostPer1MIn   *float64
	CostPer1MOut  *float64
	DBModelID     int64
}

// Config is a complete, immutable snapshot of instance configuration. Once
// loaded it is never mutated; reloads build a new Config and swap the pointer.
type Config struct {
	GeneralSettings GeneralSettings
	RouterSettings  RouterSettings
	ModelList       []ModelEntry
	DegradationMode string
}

// Model looks up a model entry by its configured name.
func (c *Config) Model(name string) (*ModelEntry, bool) {
	for i := range c.ModelList {
		if c.ModelList[i].ModelName == name {
			return &c.ModelList[i], true
		}
	}
	return nil, false
}

// Keys returns the rotation pool for a model, normalising the single-key case.
func (m *ModelEntry) Keys() []string {
	if len(m.ModelParams.APIKeys) > 0 {
		return m.ModelParams.APIKeys
	}
	if m.ModelParams.APIKey != "" {
		return []string{m.ModelParams.APIKey}
	}
	return nil
}

// ProviderSlug is the adapter selector: the part before "/" in the model
// string, defaulting to "openai" when unqualified.
func (m *ModelEntry) ProviderSlug() string {
	if i := strings.Index(m.ModelParams.Model, "/"); i > 0 {
		return m.ModelParams.Model[:i]
	}
	return "openai"
}

// UpstreamModel is the model string sent to the provider, with the provider
// prefix stripped.
func (m *ModelEntry) UpstreamModel() string {
	if i := strings.Index(m.ModelParams.Model, "/"); i > 0 {
		return m.ModelParams.Model[i+1:]
	}
	return m.ModelParams.Model
}

// --- loading ---

// Load reads the full configuration from the database.
func Load(ctx context.Context, db *store.Store) (*Config, error) {
	settings, err := loadSettings(ctx, db)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		GeneralSettings: GeneralSettings{
			MasterKey:             settings["master_key"],
			DBPath:                settings["db_path"],
			UsageLogRetentionDays: settingsInt(settings, "usage_log_retention_days", 30),
			AdminLogRetentionDays: settingsInt(settings, "admin_log_retention_days", 90),
			HMACEnabled:           settingsBool(settings, "hmac_enabled", false),
			HMACSecret:            settings["hmac_secret"],
		},
		RouterSettings: RouterSettings{
			NumRetries:   settingsInt(settings, "num_retries", 2),
			CooldownTime: settingsInt(settings, "cooldown_time", 45),
			CircuitBreaker: CircuitBreakerSettings{
				Enabled:          settingsBool(settings, "circuit_breaker_enabled", true),
				FailureThreshold: settingsInt(settings, "circuit_breaker_failure_threshold", 3),
				RecoveryTimeout:  settingsInt(settings, "circuit_breaker_recovery_timeout", 30),
			},
		},
		DegradationMode: orDefault(settings["degradation_mode"], "normal"),
	}

	models, err := loadModels(ctx, db)
	if err != nil {
		return nil, err
	}
	cfg.ModelList = models
	return cfg, nil
}

func loadSettings(ctx context.Context, db *store.Store) (map[string]string, error) {
	rows, err := db.Query(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("config: read settings: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func loadModels(ctx context.Context, db *store.Store) ([]ModelEntry, error) {
	// Provider keys are aggregated per model. A comma-separated join is safe
	// because API keys never contain commas, and matches the Python query so
	// both implementations build identical routing tables.
	rows, err := db.Query(ctx, `
		SELECT m.id, m.model_name, m.model, m.api_base,
		       m.images, m.embeddings, m.stt, m.tts,
		       m.failover_model, m.can_cache,
		       m.cost_per_1m_input, m.cost_per_1m_output,
		       m.rpm, m.rpd, m.tpm, m.tpd, m.ash, m.asd,
		       GROUP_CONCAT(pk.api_key) AS api_keys
		FROM models m
		LEFT JOIN provider_keys pk ON pk.model_id = m.id AND pk.is_active = 1
		WHERE m.is_active = 1
		GROUP BY m.id
		ORDER BY m.id`)
	if err != nil {
		return nil, fmt.Errorf("config: read models: %w", err)
	}
	defer rows.Close()

	var out []ModelEntry
	for rows.Next() {
		var (
			e        ModelEntry
			apiKeys  sql.NullString
			failover sql.NullString
			costIn   sql.NullFloat64
			costOut  sql.NullFloat64
		)
		if err := rows.Scan(
			&e.DBModelID, &e.ModelName, &e.ModelParams.Model, &e.ModelParams.APIBase,
			&e.Images, &e.Embeddings, &e.STT, &e.TTS,
			&failover, &e.CanCache,
			&costIn, &costOut,
			&e.RPM, &e.RPD, &e.TPM, &e.TPD, &e.ASH, &e.ASD,
			&apiKeys,
		); err != nil {
			return nil, err
		}

		e.FailoverModel = failover.String
		if costIn.Valid {
			e.CostPer1MIn = &costIn.Float64
		}
		if costOut.Valid {
			e.CostPer1MOut = &costOut.Float64
		}
		if apiKeys.Valid && apiKeys.String != "" {
			keys := strings.Split(apiKeys.String, ",")
			e.ModelParams.APIKeys = keys
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- persistence ---

// SaveSettings writes arbitrary settings, stringifying each value. Keys with a
// nil JSON value are skipped.
func SaveSettings(ctx context.Context, db *store.Store, values map[string]any) error {
	for k, v := range values {
		if v == nil {
			continue
		}
		var s string
		switch t := v.(type) {
		case string:
			s = t
		case bool:
			if t {
				s = "1"
			} else {
				s = "0"
			}
		case int:
			s = strconv.Itoa(t)
		case int64:
			s = strconv.FormatInt(t, 10)
		case float64:
			s = strconv.FormatFloat(t, 'f', -1, 64)
		default:
			b, err := json.Marshal(t)
			if err != nil {
				return fmt.Errorf("config: serialize setting %q: %w", k, err)
			}
			s = string(b)
		}
		if _, err := db.Exec(ctx,
			`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, k, s); err != nil {
			return fmt.Errorf("config: write setting %q: %w", k, err)
		}
	}
	return nil
}

// SaveSettingsJSON stores a settings map supplied as a JSON object, which is
// how the admin PUT /admin/config/settings endpoint receives it.
func SaveSettingsJSON(ctx context.Context, db *store.Store, body []byte) error {
	var values map[string]any
	if err := json.Unmarshal(body, &values); err != nil {
		return fmt.Errorf("config: invalid settings JSON: %w", err)
	}
	return SaveSettings(ctx, db, values)
}

// ModelInput is the writable projection of a model, used by the admin API.
type ModelInput struct {
	ModelName     string   `json:"model_name"`
	Model         string   `json:"model"`
	APIBase       *string  `json:"api_base"`
	Images        bool     `json:"images"`
	Embeddings    bool     `json:"embeddings"`
	STT           bool     `json:"stt"`
	TTS           bool     `json:"tts"`
	FailoverModel *string  `json:"failover_model"`
	CanCache      bool     `json:"can_cache"`
	CostPer1MIn   *float64 `json:"cost_per_1m_input"`
	CostPer1MOut  *float64 `json:"cost_per_1m_output"`
	RPM           *int     `json:"rpm"`
	RPD           *int     `json:"rpd"`
	TPM           *int     `json:"tpm"`
	TPD           *int     `json:"tpd"`
	ASH           *int     `json:"ash"`
	ASD           *int     `json:"asd"`
}

// SaveModel inserts or updates a model row and returns its id. A nil id
// inserts; otherwise the row is updated and the id returned unchanged.
func SaveModel(ctx context.Context, db *store.Store, id int64, in ModelInput) (int64, error) {
	if id != 0 {
		_, err := db.Exec(ctx, `
			UPDATE models SET
			  model_name=?, model=?, api_base=?, images=?, embeddings=?,
			  stt=?, tts=?, failover_model=?, can_cache=?,
			  cost_per_1m_input=?, cost_per_1m_output=?,
			  rpm=?, rpd=?, tpm=?, tpd=?, ash=?, asd=?, is_active=1
			WHERE id=?`,
			in.ModelName, in.Model, in.APIBase,
			boolToInt(in.Images), boolToInt(in.Embeddings),
			boolToInt(in.STT), boolToInt(in.TTS),
			in.FailoverModel, boolToInt(in.CanCache),
			in.CostPer1MIn, in.CostPer1MOut,
			in.RPM, in.RPD, in.TPM, in.TPD, in.ASH, in.ASD,
			id)
		if err != nil {
			return 0, fmt.Errorf("config: update model %d: %w", id, err)
		}
		return id, nil
	}

	res, err := db.Exec(ctx, `
		INSERT INTO models
		  (model_name, model, api_base, images, embeddings, stt, tts,
		   failover_model, can_cache, cost_per_1m_input, cost_per_1m_output,
		   rpm, rpd, tpm, tpd, ash, asd, is_active, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?)`,
		in.ModelName, in.Model, in.APIBase,
		boolToInt(in.Images), boolToInt(in.Embeddings),
		boolToInt(in.STT), boolToInt(in.TTS),
		in.FailoverModel, boolToInt(in.CanCache),
		in.CostPer1MIn, in.CostPer1MOut,
		in.RPM, in.RPD, in.TPM, in.TPD, in.ASH, in.ASD,
		store.Now())
	if err != nil {
		return 0, fmt.Errorf("config: insert model: %w", err)
	}
	return res.LastInsertId()
}

// DeleteModel soft-deletes a model by clearing is_active, preserving usage
// history that references it.
func DeleteModel(ctx context.Context, db *store.Store, id int64) (bool, error) {
	res, err := db.Exec(ctx, `UPDATE models SET is_active = 0 WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// AddProviderKey appends a key to a model's rotation pool.
func AddProviderKey(ctx context.Context, db *store.Store, modelID int64, apiKey string, priority int) (int64, error) {
	res, err := db.Exec(ctx,
		`INSERT INTO provider_keys (model_id, api_key, priority, is_active) VALUES (?,?,?,1)`,
		modelID, apiKey, priority)
	if err != nil {
		return 0, fmt.Errorf("config: add provider key: %w", err)
	}
	return res.LastInsertId()
}

// DeleteProviderKey soft-deletes a provider key.
func DeleteProviderKey(ctx context.Context, db *store.Store, id int64) (bool, error) {
	res, err := db.Exec(ctx, `UPDATE provider_keys SET is_active = 0 WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ProviderKey is a rotation-pool entry. The key material itself is never
// returned by list endpoints.
type ProviderKey struct {
	ID       int64 `json:"id"`
	Priority int   `json:"priority"`
	IsActive bool  `json:"is_active"`
}

// ListProviderKeys returns a model's key pool ordered by priority.
func ListProviderKeys(ctx context.Context, db *store.Store, modelID int64) ([]ProviderKey, error) {
	rows, err := db.Query(ctx,
		`SELECT id, priority, is_active FROM provider_keys WHERE model_id = ? ORDER BY priority, id`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProviderKey
	for rows.Next() {
		var k ProviderKey
		if err := rows.Scan(&k.ID, &k.Priority, &k.IsActive); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// --- helpers ---

func settingsInt(s map[string]string, key string, def int) int {
	if v, ok := s[key]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func settingsBool(s map[string]string, key string, def bool) bool {
	v, ok := s[key]
	if !ok {
		return def
	}
	return v != "0" && v != "false" && v != ""
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
