package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/atulsinha/partitionctl/engine/executor"
	"github.com/atulsinha/partitionctl/engine/protocol"
	"github.com/atulsinha/partitionctl/engine/state"
)

// StateBackend names which [state.StateStore] implementation to use
// (FR-STATE-2).
type StateBackend string

// The state backends.
const (
	// StateSQL keeps execution state in a dedicated schema in the target
	// database (FR-STATE-3). It is the default, mirroring where Liquibase keeps
	// DATABASECHANGELOG.
	StateSQL StateBackend = "sql"
	// StateFile keeps execution state on the local filesystem, outside the
	// target database's blast radius (TRD §7.2.5). It is also what makes
	// `status` answerable with the target unreachable (FR-CLI-12, AC-25).
	StateFile StateBackend = "file"
)

// Valid reports whether b names a backend.
func (b StateBackend) Valid() bool { return b == StateSQL || b == StateFile }

// Config is the resolved runtime configuration.
//
// Resolution order is flags, then environment, then the configuration file
// (FR-CLI-7). A value set by a flag is never overwritten by a lower source,
// which is what makes a one-off `--lock-timeout 30s` mean what an operator
// expects.
//
// # Credentials are not here by accident
//
// [Config.DSN] can carry a password, so it has no flag: it is reachable only
// from the environment or the configuration file (FR-CLI-8, NFR-SEC-2). The
// discrete connection fields carry no secret and therefore do have flags. There
// is no Password field at all: the driver resolves one through PGPASSWORD,
// ~/.pgpass or a service file, which are PostgreSQL's own mechanisms and are
// what NFR-SEC-2 points at. Nothing in this struct is ever logged
// (NFR-SEC-3); see [Config.Redacted].
type Config struct {
	// Driver is the database/sql driver name main registered.
	Driver string

	// DSN is the full data source name. It may carry a password, so it is
	// never a flag.
	DSN string

	// The discrete connection parameters, used to build a DSN when none is
	// given. None of them is secret.
	Host    string
	Port    int
	Dbname  string
	User    string
	SSLMode string

	// State selects the state store implementation.
	State StateBackend
	// StateDir is the file store's root, used when State is [StateFile].
	StateDir string
	// StateSchema is the SQL store's dedicated schema (FR-STATE-3).
	StateSchema string

	// LockTimeout is applied to every DDL statement (FR-EXEC-5).
	LockTimeout time.Duration
	// BuildLockTimeout replaces LockTimeout on the CONCURRENTLY statements,
	// which wait for the application's own transactions as part of their work
	// rather than only while taking their initial lock. Sized to sit above the
	// target's longest legitimate transaction, not to the short bound that
	// keeps ordinary DDL out of the way of application traffic (FR-EXEC-5).
	BuildLockTimeout time.Duration
	// StatementTimeout bounds DDL that is allowed a finite one. It is always
	// absent from index.create_concurrently, whatever this says (FR-EXEC-5).
	StatementTimeout time.Duration

	// MaxAttempts, RetryBaseDelay and RetryMaxDelay bound retries (FR-EXEC-4).
	MaxAttempts    int
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration

	// LeaseTTL and HeartbeatInterval govern orphan detection (FR-STATE-7,
	// FR-LOCK-3, INV-4).
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration

	// Actor is who ran the command, recorded in the audit trail.
	Actor string

	// ConfigFile is the file the file-sourced values came from, for the
	// operator's benefit. Empty when no file was read.
	ConfigFile string
}

// Defaults returns the configuration before any source is applied.
func Defaults() Config {
	return Config{
		Driver:            "postgres",
		Port:              5432,
		SSLMode:           "prefer",
		State:             StateSQL,
		StateSchema:       state.DefaultSchema,
		LockTimeout:       5 * time.Second,
		BuildLockTimeout:  executor.DefaultBuildLockTimeout,
		MaxAttempts:       5,
		RetryBaseDelay:    time.Second,
		RetryMaxDelay:     time.Minute,
		LeaseTTL:          state.DefaultLeaseTTL,
		HeartbeatInterval: state.DefaultHeartbeatInterval,
		Actor:             defaultActor(),
	}
}

func defaultActor() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	return "unknown"
}

// Validate checks the configuration is usable.
func (c Config) Validate() error {
	if c.Driver == "" {
		return protocol.ErrFailure.Detailf("no database driver name: set --driver or PARTITIONCTL_DRIVER")
	}
	if !c.State.Valid() {
		return protocol.ErrFailure.Detailf(
			"unknown state backend %q, want %q or %q", c.State, StateSQL, StateFile)
	}
	if c.State == StateFile && c.StateDir == "" {
		return protocol.ErrFailure.Detailf(
			"state backend %q needs a directory: set --state-dir", StateFile)
	}
	if c.State == StateSQL {
		if err := protocol.ValidateIdentifier(c.StateSchema); err != nil {
			return protocol.ErrFailure.Detailf("state schema: %v", err)
		}
	}
	if c.LockTimeout <= 0 {
		return protocol.ErrFailure.Detailf("lock_timeout must be positive (FR-EXEC-5), got %s", c.LockTimeout)
	}
	if c.BuildLockTimeout <= 0 {
		return protocol.ErrFailure.Detailf(
			"build_lock_timeout must be positive (FR-EXEC-5), got %s", c.BuildLockTimeout)
	}
	if c.StatementTimeout < 0 {
		return protocol.ErrFailure.Detailf("statement_timeout must not be negative, got %s", c.StatementTimeout)
	}
	if c.MaxAttempts < 1 {
		return protocol.ErrFailure.Detailf("max_attempts must be at least 1, got %d", c.MaxAttempts)
	}
	if c.RetryBaseDelay < 0 || c.RetryMaxDelay < 0 {
		return protocol.ErrFailure.Detailf("retry delays must not be negative")
	}
	if c.RetryMaxDelay > 0 && c.RetryMaxDelay < c.RetryBaseDelay {
		return protocol.ErrFailure.Detailf(
			"retry_max_delay %s is below retry_base_delay %s", c.RetryMaxDelay, c.RetryBaseDelay)
	}
	if c.LeaseTTL <= 0 {
		return protocol.ErrFailure.Detailf("lease_ttl must be positive (FR-STATE-7)")
	}
	if c.HeartbeatInterval <= 0 {
		return protocol.ErrFailure.Detailf("heartbeat_interval must be positive (FR-LOCK-3)")
	}
	if c.HeartbeatInterval >= c.LeaseTTL {
		return protocol.ErrFailure.Detailf(
			"heartbeat_interval %s is not shorter than lease_ttl %s, so a live run would look orphaned (INV-4)",
			c.HeartbeatInterval, c.LeaseTTL)
	}
	return nil
}

// DataSourceName returns the DSN to hand database/sql.
//
// An explicit [Config.DSN] wins. Otherwise a libpq keyword/value string is
// built from the discrete parameters, which both lib/pq and pgx's stdlib driver
// accept. No password is ever placed in it: the driver resolves one through
// PGPASSWORD, ~/.pgpass or a service file (NFR-SEC-2).
func (c Config) DataSourceName() string {
	if c.DSN != "" {
		return c.DSN
	}
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+quoteKeywordValue(v))
		}
	}
	add("host", c.Host)
	if c.Port > 0 {
		add("port", strconv.Itoa(c.Port))
	}
	add("dbname", c.Dbname)
	add("user", c.User)
	add("sslmode", c.SSLMode)
	return strings.Join(parts, " ")
}

// quoteKeywordValue escapes a libpq keyword/value element.
func quoteKeywordValue(v string) string {
	if !strings.ContainsAny(v, " '\\") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(v) + "'"
}

// Redacted returns the configuration as loggable key/value pairs, sorted by
// key. The DSN is reported only as present or absent, because it can carry a
// password and logs must not (NFR-SEC-3).
func (c Config) Redacted() map[string]string {
	dsn := "absent"
	if c.DSN != "" {
		dsn = "present (redacted)"
	}
	return map[string]string{
		"actor":              c.Actor,
		"config_file":        c.ConfigFile,
		"dbname":             c.Dbname,
		"driver":             c.Driver,
		"dsn":                dsn,
		"build_lock_timeout": c.BuildLockTimeout.String(),
		"heartbeat_interval": c.HeartbeatInterval.String(),
		"host":               c.Host,
		"lease_ttl":          c.LeaseTTL.String(),
		"lock_timeout":       c.LockTimeout.String(),
		"max_attempts":       strconv.Itoa(c.MaxAttempts),
		"port":               strconv.Itoa(c.Port),
		"sslmode":            c.SSLMode,
		"state":              string(c.State),
		"state_dir":          c.StateDir,
		"state_schema":       c.StateSchema,
		"statement_timeout":  c.StatementTimeout.String(),
		"user":               c.User,
	}
}

// ---------------------------------------------------------------------------
// Resolution: flags, then environment, then file (FR-CLI-7)
// ---------------------------------------------------------------------------

// envLookup is the environment, injected so resolution is testable without
// mutating the process.
type envLookup func(string) (string, bool)

// osEnv reads the real environment.
func osEnv(k string) (string, bool) { return os.LookupEnv(k) }

// configEnvKeys maps a configuration key to the environment variables that can
// supply it, in precedence order. The PARTITIONCTL_ names are ours; the PG names
// are libpq's and are honoured because an operator who has already exported them
// should not have to repeat themselves.
var configEnvKeys = map[string][]string{
	"driver":             {"PARTITIONCTL_DRIVER"},
	"dsn":                {"PARTITIONCTL_DSN", "DATABASE_URL"},
	"host":               {"PARTITIONCTL_HOST", "PGHOST"},
	"port":               {"PARTITIONCTL_PORT", "PGPORT"},
	"dbname":             {"PARTITIONCTL_DBNAME", "PGDATABASE"},
	"user":               {"PARTITIONCTL_USER", "PGUSER"},
	"sslmode":            {"PARTITIONCTL_SSLMODE", "PGSSLMODE"},
	"state":              {"PARTITIONCTL_STATE"},
	"state_dir":          {"PARTITIONCTL_STATE_DIR"},
	"state_schema":       {"PARTITIONCTL_STATE_SCHEMA"},
	"lock_timeout":       {"PARTITIONCTL_LOCK_TIMEOUT"},
	"statement_timeout":  {"PARTITIONCTL_STATEMENT_TIMEOUT"},
	"max_attempts":       {"PARTITIONCTL_MAX_ATTEMPTS"},
	"retry_base_delay":   {"PARTITIONCTL_RETRY_BASE_DELAY"},
	"retry_max_delay":    {"PARTITIONCTL_RETRY_MAX_DELAY"},
	"lease_ttl":          {"PARTITIONCTL_LEASE_TTL"},
	"heartbeat_interval": {"PARTITIONCTL_HEARTBEAT_INTERVAL"},
	"actor":              {"PARTITIONCTL_ACTOR"},
}

// knownConfigKeys is every key the configuration file may contain. An unknown
// key is an error rather than a shrug: a typo in a file that governs DDL should
// not be silently ignored.
var knownConfigKeys = func() map[string]bool {
	m := make(map[string]bool, len(configEnvKeys))
	for k := range configEnvKeys {
		m[k] = true
	}
	return m
}()

// resolveConfig applies the three sources in FR-CLI-7 order. flagSet reports
// which keys the operator set explicitly on the command line; those are never
// overwritten.
func resolveConfig(base Config, flags map[string]string, env envLookup, file map[string]string, fileName string) (Config, error) {
	c := base
	c.ConfigFile = fileName

	get := func(key string) (string, bool) {
		if v, ok := flags[key]; ok {
			return v, true
		}
		for _, name := range configEnvKeys[key] {
			if v, ok := env(name); ok && v != "" {
				return v, true
			}
		}
		if v, ok := file[key]; ok {
			return v, true
		}
		return "", false
	}

	str := func(key string, dst *string) {
		if v, ok := get(key); ok {
			*dst = v
		}
	}
	dur := func(key string, dst *time.Duration) error {
		v, ok := get(key)
		if !ok {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return protocol.ErrFailure.Detailf("%s: %v", key, err)
		}
		*dst = d
		return nil
	}
	num := func(key string, dst *int) error {
		v, ok := get(key)
		if !ok {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return protocol.ErrFailure.Detailf("%s: %v", key, err)
		}
		*dst = n
		return nil
	}

	str("driver", &c.Driver)
	str("dsn", &c.DSN)
	str("host", &c.Host)
	str("dbname", &c.Dbname)
	str("user", &c.User)
	str("sslmode", &c.SSLMode)
	str("state_dir", &c.StateDir)
	str("state_schema", &c.StateSchema)
	str("actor", &c.Actor)

	if v, ok := get("state"); ok {
		c.State = StateBackend(strings.ToLower(v))
	}
	if err := num("port", &c.Port); err != nil {
		return c, err
	}
	if err := num("max_attempts", &c.MaxAttempts); err != nil {
		return c, err
	}
	// Ordered, not a map range: two malformed durations must always report the
	// same one first. A tool whose contract is a reproducible plan should not
	// have a diagnostic that changes between runs of the same command.
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{
		{"heartbeat_interval", &c.HeartbeatInterval},
		{"lease_ttl", &c.LeaseTTL},
		{"build_lock_timeout", &c.BuildLockTimeout},
		{"lock_timeout", &c.LockTimeout},
		{"retry_base_delay", &c.RetryBaseDelay},
		{"retry_max_delay", &c.RetryMaxDelay},
		{"statement_timeout", &c.StatementTimeout},
	} {
		if err := dur(d.key, d.dst); err != nil {
			return c, err
		}
	}

	// A DSN reaching us from a flag would be a password on argv, which
	// FR-CLI-8 forbids outright. No flag defines it, so this can only fire if
	// a future edit adds one; the check is here so that edit fails loudly.
	if _, ok := flags["dsn"]; ok {
		return c, protocol.ErrFailure.Detailf(
			"a data source name may not be supplied on the command line, because it can carry a password " +
				"(FR-CLI-8, NFR-SEC-2); use PARTITIONCTL_DSN or the configuration file")
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// The configuration file
// ---------------------------------------------------------------------------

// DefaultConfigFiles are the paths searched when --config is not given, in
// order. The first that exists is read.
func DefaultConfigFiles() []string {
	paths := []string{"partitionctl.yaml", "partitionctl.yml"}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths,
			filepath.Join(home, ".config", "partitionctl", "config.yaml"),
			filepath.Join(home, ".partitionctl.yaml"))
	}
	return paths
}

// LoadConfigFile reads the configuration file at path.
//
// # Why this parses a restricted subset of YAML rather than all of it
//
// FR-CLI-7 names a YAML file, and M1 is standard library only (HANDOFF §3), so
// there is no YAML parser available. Rather than pretend, this reads the
// unambiguous subset a flat settings file needs: `key: value` one per line,
// `#` comments, blank lines, and optional single or double quotes around a
// value. Every configuration key is scalar, so nothing is lost.
//
// Anything outside that subset is an error naming the line, never a guess.
// Indentation, `-` list items, `|` and `>` block scalars, anchors and nested
// maps are all rejected. A file that governs DDL is the wrong place to silently
// misread a line, and a parser that accepts a subset loudly is safer than one
// that accepts a superset quietly.
func LoadConfigFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseConfigFile(path, string(data))
}

func parseConfigFile(name, body string) (map[string]string, error) {
	out := make(map[string]string)
	for i, raw := range strings.Split(body, "\n") {
		line := i + 1
		s := strings.TrimRight(raw, " \t\r")
		if s == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(s), "#") {
			continue
		}
		if s != strings.TrimLeft(s, " \t") {
			return nil, protocol.ErrFailure.Detailf(
				"%s:%d: indented line; this reader accepts flat `key: value` settings only, "+
					"not nested YAML", name, line)
		}
		if strings.HasPrefix(s, "-") {
			return nil, protocol.ErrFailure.Detailf(
				"%s:%d: list item; no configuration key takes a list", name, line)
		}
		key, value, ok := strings.Cut(s, ":")
		if !ok {
			return nil, protocol.ErrFailure.Detailf(
				"%s:%d: no `key: value` separator", name, line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if value == "|" || value == ">" || strings.HasPrefix(value, "&") || strings.HasPrefix(value, "*") {
			return nil, protocol.ErrFailure.Detailf(
				"%s:%d: block scalars and anchors are not supported", name, line)
		}
		if idx := strings.Index(value, " #"); idx >= 0 && !isQuoted(value) {
			value = strings.TrimSpace(value[:idx])
		}
		value = unquoteScalar(value)
		key = strings.ToLower(key)
		if !knownConfigKeys[key] {
			return nil, protocol.ErrFailure.Detailf(
				"%s:%d: unknown configuration key %q; known keys are %v", name, line, key, sortedKnownKeys())
		}
		if _, dup := out[key]; dup {
			return nil, protocol.ErrFailure.Detailf("%s:%d: duplicate key %q", name, line, key)
		}
		out[key] = value
	}
	return out, nil
}

func isQuoted(v string) bool {
	return len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'')
}

func unquoteScalar(v string) string {
	if isQuoted(v) {
		return v[1 : len(v)-1]
	}
	return v
}

func sortedKnownKeys() []string {
	keys := make([]string, 0, len(knownConfigKeys))
	for k := range knownConfigKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// findConfigFile returns the first default configuration file that exists.
func findConfigFile() string {
	for _, p := range DefaultConfigFiles() {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}
