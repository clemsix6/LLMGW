package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"gopkg.in/yaml.v3"
)

const (
	// defaultPostgresDSNEnv is the environment variable used for the PostgreSQL DSN when no override is configured.
	defaultPostgresDSNEnv = "LLMGW_POSTGRES_DSN"

	// defaultKeyPepperEnv is the environment variable used for the key pepper when no override is configured.
	defaultKeyPepperEnv = "LLMGW_KEY_PEPPER"

	// defaultDiscordWebhookURLEnv is the environment variable used for the Discord webhook URL when no override is configured.
	defaultDiscordWebhookURLEnv = "LLMGW_DISCORD_WEBHOOK_URL"

	// defaultUsageRetentionDays limits persisted usage data to a little over one month.
	defaultUsageRetentionDays = 35

	// minimumUsageRetentionDays preserves enough history for the daily budget window.
	minimumUsageRetentionDays = 2

	// minimumKeyPepperBytes is the minimum entropy input for stable keyed identifiers.
	minimumKeyPepperBytes = 32

	// defaultUsageOutstandingCapacity bounds admitted generation groups awaiting
	// their FIFO SDK usage barrier.
	defaultUsageOutstandingCapacity = 64
	// maximumUsageOutstandingCapacity prevents an operator typo from defeating
	// the process memory bound.
	maximumUsageOutstandingCapacity = 1024
	// maximumUsageRecordsPerRequest is the reviewed v7.2.102 safety ceiling.
	maximumUsageRecordsPerRequest = 256
)

// LLMGW holds the LLMGW-owned values in the shared YAML configuration.
type LLMGW struct {
	PostgresDSNEnv       string `yaml:"postgres-dsn-env"`        // PostgresDSNEnv names the environment variable that holds the PostgreSQL DSN.
	KeyPepperEnv         string `yaml:"key-pepper-env"`          // KeyPepperEnv names the environment variable that holds the key pepper.
	DiscordWebhookURLEnv string `yaml:"discord-webhook-url-env"` // DiscordWebhookURLEnv names the environment variable that holds the Discord webhook URL.
	UsageRetentionDays   int    `yaml:"usage-retention-days"`    // UsageRetentionDays keeps usage history for this many whole days.
	// UsageOutstandingCapacity bounds generation groups whose SDK usage barrier
	// has not completed.
	UsageOutstandingCapacity int `yaml:"usage-outstanding-capacity"`
}

// Config is the complete runtime configuration shared by CLIProxyAPI and LLMGW.
type Config struct {
	Path           string            // Path is the source YAML path.
	Proxy          *sdkconfig.Config // Proxy is the native CLIProxyAPI configuration.
	LLMGW          LLMGW             // LLMGW contains gateway-specific configuration.
	UsageRetention time.Duration     // UsageRetention is the configured usage retention period.
	// MaxUsageRecords is the proven v7.2.102 usage-record bound for one
	// generation, excluding its one FIFO barrier.
	MaxUsageRecords int
}

// securityProjection contains the configuration fields LLMGW must validate before the SDK reads the file.
type securityProjection struct {
	LLMGW                  LLMGW                    `yaml:"llmgw"`
	APIKeys                []string                 `yaml:"api-keys"`
	AuthDir                string                   `yaml:"auth-dir"`
	RemoteManagement       remoteManagementSettings `yaml:"remote-management"`
	Home                   enabledSettings          `yaml:"home"`
	Pprof                  pprofSettings            `yaml:"pprof"`
	RequestRetry           int                      `yaml:"request-retry"`
	MaxRetryCredentials    int                      `yaml:"max-retry-credentials"`
	Routing                routingSettings          `yaml:"routing"`
	DisableImageGeneration any                      `yaml:"disable-image-generation"` // DisableImageGeneration must select the SDK's fully disabled mode.
}

// remoteManagementSettings contains security-relevant remote-management settings.
type remoteManagementSettings struct {
	AllowRemote         bool   `yaml:"allow-remote"`
	SecretKey           string `yaml:"secret-key"`
	DisableControlPanel *bool  `yaml:"disable-control-panel"`
}

// enabledSettings contains an optional enabled flag.
type enabledSettings struct {
	Enabled bool `yaml:"enabled"`
}

// pprofSettings contains the pprof enable flag.
type pprofSettings struct {
	Enable bool `yaml:"enable"`
}

type routingSettings struct {
	SessionAffinity bool `yaml:"session-affinity"`
}

// Load reads and validates one shared YAML configuration file.
func Load(path string, getenv func(string) string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read configuration file:\n%w", err)
	}

	projection, err := decodeSecurityProjection(data)
	if err != nil {
		return Config{}, err
	}
	if err := validateSecurity(projection, getenv); err != nil {
		return Config{}, err
	}

	proxy, err := sdkconfig.LoadConfig(path)
	if err != nil {
		return Config{}, fmt.Errorf("load CLIProxyAPI configuration:\n%w", err)
	}
	cfg := Config{
		Path:           path,
		Proxy:          proxy,
		LLMGW:          projection.LLMGW,
		UsageRetention: time.Duration(projection.LLMGW.UsageRetentionDays) * 24 * time.Hour,
	}
	maxUsageRecords, err := cfg.validateUsageBackpressure()
	if err != nil {
		return Config{}, err
	}
	cfg.MaxUsageRecords = maxUsageRecords
	return cfg, nil
}

// DatabaseDSN resolves the configured PostgreSQL DSN without retaining its value in Config.
func (c Config) DatabaseDSN(getenv func(string) string) (string, error) {
	dsn := getenv(c.LLMGW.PostgresDSNEnv)
	if dsn == "" {
		return "", fmt.Errorf("resolve PostgreSQL DSN from %s:\n%w", c.LLMGW.PostgresDSNEnv, errors.New("value is required"))
	}
	return dsn, nil
}

// KeyPepper resolves the configured key pepper without retaining its value in Config.
func (c Config) KeyPepper(getenv func(string) string) ([]byte, error) {
	pepper := []byte(getenv(c.LLMGW.KeyPepperEnv))
	if len(pepper) == 0 {
		return nil, fmt.Errorf("resolve key pepper from %s:\n%w", c.LLMGW.KeyPepperEnv, errors.New("value is required"))
	}
	if len(pepper) < minimumKeyPepperBytes {
		return nil, fmt.Errorf("resolve key pepper from %s:\n%w", c.LLMGW.KeyPepperEnv, fmt.Errorf("value must be at least %d bytes", minimumKeyPepperBytes))
	}
	return pepper, nil
}

// ValidateUsageBackpressure rechecks the effective startup-only SDK inputs.
// Composition calls it again so manually constructed Config values cannot
// bypass the loader's finite queue proof.
func (c Config) ValidateUsageBackpressure() error {
	bound, err := c.validateSnapshotUsageBackpressure()
	if err != nil {
		return err
	}
	if c.MaxUsageRecords != bound {
		return errors.New(
			"validate configuration usage backpressure:\nmaximum SDK usage record bound is stale",
		)
	}
	return nil
}

func (c Config) validateUsageBackpressure() (int, error) {
	if c.LLMGW.UsageOutstandingCapacity < 1 ||
		c.LLMGW.UsageOutstandingCapacity > maximumUsageOutstandingCapacity {
		return 0, fmt.Errorf(
			"validate configuration usage backpressure:\nusage-outstanding-capacity must be between 1 and %d",
			maximumUsageOutstandingCapacity,
		)
	}
	if c.Proxy == nil {
		return 0, errors.New(
			"validate configuration usage backpressure:\nCLIProxyAPI configuration is required",
		)
	}
	if c.Proxy.RequestRetry < 0 {
		return 0, errors.New(
			"validate configuration usage backpressure:\nrequest-retry must be nonnegative",
		)
	}
	if c.Proxy.MaxRetryCredentials <= 0 {
		return 0, errors.New(
			"validate configuration usage backpressure:\nmax-retry-credentials must be positive",
		)
	}
	if c.Proxy.Routing.SessionAffinity {
		return 0, errors.New(
			"validate configuration usage backpressure:\nrouting.session-affinity must be false",
		)
	}
	if c.Proxy.DisableImageGeneration.String() != "true" {
		return 0, errors.New(
			"validate configuration usage backpressure:\ndisable-image-generation must be true",
		)
	}
	payload := c.Proxy.Payload
	if len(payload.Default) != 0 || len(payload.DefaultRaw) != 0 ||
		len(payload.Override) != 0 || len(payload.OverrideRaw) != 0 {
		return 0, errors.New(
			"validate configuration usage backpressure:\npayload write rules must be empty",
		)
	}
	bound, err := usageRecordBound(c.Proxy)
	if err != nil {
		return 0, fmt.Errorf("validate configuration usage backpressure:\n%w", err)
	}
	return bound, nil
}

// validateSnapshotUsageBackpressure adds checks that may inspect only the
// immutable private auth snapshot created during service composition.
func (c Config) validateSnapshotUsageBackpressure() (int, error) {
	bound, err := c.validateUsageBackpressure()
	if err != nil {
		return 0, err
	}
	if err := validateAuthRetryOverrides(c.Proxy.AuthDir, c.Proxy.RequestRetry); err != nil {
		return 0, fmt.Errorf("validate configuration usage backpressure:\n%w", err)
	}
	return bound, nil
}

// decodeSecurityProjection decodes the LLMGW-owned and security-sensitive YAML fields.
func decodeSecurityProjection(data []byte) (securityProjection, error) {
	var projection securityProjection
	if err := yaml.Unmarshal(data, &projection); err != nil {
		return securityProjection{}, fmt.Errorf("parse configuration security settings:\n%w", err)
	}
	applyLLMGWDefaults(&projection.LLMGW)
	return projection, nil
}

// applyLLMGWDefaults applies defaults that keep commands independent of database credentials until needed.
func applyLLMGWDefaults(settings *LLMGW) {
	if strings.TrimSpace(settings.PostgresDSNEnv) == "" {
		settings.PostgresDSNEnv = defaultPostgresDSNEnv
	}
	if strings.TrimSpace(settings.KeyPepperEnv) == "" {
		settings.KeyPepperEnv = defaultKeyPepperEnv
	}
	if strings.TrimSpace(settings.DiscordWebhookURLEnv) == "" {
		settings.DiscordWebhookURLEnv = defaultDiscordWebhookURLEnv
	}
	if settings.UsageRetentionDays == 0 {
		settings.UsageRetentionDays = defaultUsageRetentionDays
	}
	if settings.UsageOutstandingCapacity == 0 {
		settings.UsageOutstandingCapacity = defaultUsageOutstandingCapacity
	}
}

// validateSecurity rejects settings that would provide another inbound control or authentication surface.
func validateSecurity(projection securityProjection, getenv func(string) string) error {
	if len(projection.APIKeys) > 0 {
		return errors.New("validate configuration security:\napi-keys must be empty")
	}
	if projection.RemoteManagement.AllowRemote {
		return errors.New("validate configuration security:\nremote-management.allow-remote must be false")
	}
	if projection.RemoteManagement.SecretKey != "" {
		return errors.New("validate configuration security:\nremote-management.secret-key must be empty")
	}
	if projection.RemoteManagement.DisableControlPanel == nil || !*projection.RemoteManagement.DisableControlPanel {
		return errors.New("validate configuration security:\nremote-management.disable-control-panel must be true")
	}
	if projection.Home.Enabled {
		return errors.New("validate configuration security:\nhome.enabled must be false")
	}
	if projection.Pprof.Enable {
		return errors.New("validate configuration security:\npprof.enable must be false")
	}
	if getenv("MANAGEMENT_PASSWORD") != "" {
		return errors.New("validate configuration security:\nMANAGEMENT_PASSWORD must be empty")
	}
	if strings.TrimSpace(projection.AuthDir) == "" {
		return errors.New("validate configuration security:\nauth-dir is required")
	}
	if projection.LLMGW.UsageRetentionDays < minimumUsageRetentionDays {
		return fmt.Errorf("validate configuration security:\nusage-retention-days must be at least %d", minimumUsageRetentionDays)
	}
	if projection.LLMGW.UsageOutstandingCapacity < 1 ||
		projection.LLMGW.UsageOutstandingCapacity > maximumUsageOutstandingCapacity {
		return fmt.Errorf(
			"validate configuration security:\nusage-outstanding-capacity must be between 1 and %d",
			maximumUsageOutstandingCapacity,
		)
	}
	if projection.RequestRetry < 0 {
		return errors.New("validate configuration security:\nrequest-retry must be nonnegative")
	}
	if projection.MaxRetryCredentials <= 0 {
		return errors.New("validate configuration security:\nmax-retry-credentials must be positive")
	}
	if projection.Routing.SessionAffinity {
		return errors.New("validate configuration security:\nrouting.session-affinity must be false")
	}
	if !imageGenerationDisabled(projection.DisableImageGeneration) {
		return errors.New("validate configuration security:\ndisable-image-generation must be true")
	}
	return nil
}

// imageGenerationDisabled accepts only the SDK's semantic true modes.
func imageGenerationDisabled(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

// usageRecordBound mirrors the finite execution loops in the pinned SDK. Each
// executor call publishes exactly one terminal record: auxiliary image-model
// records cannot occur because disable-image-generation strips the built-in
// tool on every endpoint and payload write rules are rejected above, so nothing
// can reinject it.
func usageRecordBound(cfg *sdkconfig.Config) (int, error) {
	if cfg == nil {
		return 0, errors.New("CLIProxyAPI configuration is required")
	}
	if cfg.QuotaExceeded.AntigravityCredits {
		return 0, errors.New("quota-exceeded.antigravity-credits must be false")
	}
	modelPool := maxEffectiveOpenAICompatModelPool(cfg)
	factors := []int{
		cfg.RequestRetry + 1,
		cfg.MaxRetryCredentials,
		modelPool + 1,
	}
	bound := 1
	for _, factor := range factors {
		if factor <= 0 || bound > math.MaxInt/factor {
			return 0, errors.New("maximum SDK usage records per request overflows")
		}
		bound *= factor
	}
	if bound > maximumUsageRecordsPerRequest {
		return 0, fmt.Errorf(
			"maximum SDK usage records per request %d exceeds %d",
			bound,
			maximumUsageRecordsPerRequest,
		)
	}
	return bound, nil
}

// maxEffectiveOpenAICompatModelPool reproduces the SDK's case-insensitive,
// distinct-upstream-name alias pool size. Every other v7.2.102 execution path
// has one model candidate.
func maxEffectiveOpenAICompatModelPool(cfg *sdkconfig.Config) int {
	maximum := 1
	for _, provider := range cfg.OpenAICompatibility {
		if provider.Disabled {
			continue
		}
		pools := make(map[string]map[string]struct{})
		for _, model := range provider.Models {
			alias := strings.ToLower(strings.TrimSpace(model.Alias))
			if alias == "" {
				continue
			}
			upstream := strings.ToLower(strings.TrimSpace(model.Name))
			if upstream == "" {
				upstream = alias
			}
			if pools[alias] == nil {
				pools[alias] = make(map[string]struct{})
			}
			pools[alias][upstream] = struct{}{}
			if len(pools[alias]) > maximum {
				maximum = len(pools[alias])
			}
		}
	}
	return maximum
}

// validateAuthRetryOverrides makes auth-dir startup-only retry metadata no more
// permissive than the reviewed shared configuration. The embedded service uses
// a no-op SDK watcher, so files added later require a restart.
func validateAuthRetryOverrides(authDir string, configured int) error {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return nil
	}
	var err error
	authDir, err = resolveAuthDir(authDir)
	if err != nil {
		return err
	}
	info, err := os.Stat(authDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("inspect auth directory: unavailable")
	}
	if !info.IsDir() {
		return errors.New("inspect auth directory: not a directory")
	}
	err = filepath.WalkDir(authDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("inspect auth file: unavailable")
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return errors.New("read auth file: unavailable")
		}
		var value any
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.UseNumber()
		if decodeErr := decoder.Decode(&value); decodeErr != nil {
			return errors.New("parse auth file: invalid JSON")
		}
		override, found, overrideErr := findAuthRetryOverride(value)
		if overrideErr != nil {
			return overrideErr
		}
		if found && override > configured {
			return errors.New("auth retry override exceeds request-retry")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// resolveAuthDir mirrors CLIProxyAPI's public runtime behavior for startup
// validation without importing its internal util package.
func resolveAuthDir(authDir string) (string, error) {
	if !strings.HasPrefix(authDir, "~") {
		return filepath.Clean(authDir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("resolve auth directory: unavailable")
	}
	remainder := strings.TrimPrefix(authDir, "~")
	remainder = strings.TrimLeft(remainder, "/\\")
	if remainder == "" {
		return filepath.Clean(home), nil
	}
	normalized := strings.ReplaceAll(remainder, "\\", "/")
	return filepath.Clean(filepath.Join(home, filepath.FromSlash(normalized))), nil
}

func findAuthRetryOverride(value any) (int, bool, error) {
	maximum := 0
	found := false
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if key == "request_retry" || key == "request-retry" {
				override, err := parseAuthRetryOverride(nested)
				if err != nil {
					return 0, false, err
				}
				if !found || override > maximum {
					maximum = override
				}
				found = true
				continue
			}
			override, nestedFound, err := findAuthRetryOverride(nested)
			if err != nil {
				return 0, false, err
			}
			if nestedFound && (!found || override > maximum) {
				maximum = override
				found = true
			}
		}
	case []any:
		for _, nested := range typed {
			override, nestedFound, err := findAuthRetryOverride(nested)
			if err != nil {
				return 0, false, err
			}
			if nestedFound && (!found || override > maximum) {
				maximum = override
				found = true
			}
		}
	}
	return maximum, found, nil
}

func parseAuthRetryOverride(value any) (int, error) {
	var text string
	switch typed := value.(type) {
	case json.Number:
		text = typed.String()
	case string:
		text = strings.TrimSpace(typed)
	default:
		return 0, errors.New("auth retry override is invalid")
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil || parsed < 0 || parsed > int64(math.MaxInt) {
		return 0, errors.New("auth retry override is invalid")
	}
	return int(parsed), nil
}
