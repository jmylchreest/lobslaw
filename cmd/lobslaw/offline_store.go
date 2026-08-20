package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	bolterrors "go.etcd.io/bbolt/errors"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
)

// offlineStore holds the flags every `memory` / `session` subcommand
// needs to find and decrypt state.db, and opens it.
//
// Resolution deliberately matches `audit verify` and
// cmd/backfill-embeddings rather than introducing a third convention:
// a config file locates the data dir, an explicit path overrides it,
// and the encryption key comes from a secret ref.
type offlineStore struct {
	configPath string
	dataDir    string
	statePath  string
	keyRef     string

	// cfg caches the loaded config so path and key resolution don't
	// parse it twice.
	cfg       *config.Config
	cfgLoaded bool
	cfgErr    error
}

// bind registers the shared flags on fs. Call before fs.Parse.
func (o *offlineStore) bind(fs *flag.FlagSet) {
	fs.StringVar(&o.configPath, "config", envOr("LOBSLAW_CONFIG", ""),
		"path to config.toml; supplies [cluster] data_dir and [memory.encryption] key_ref")
	fs.StringVar(&o.dataDir, "data-dir", "",
		"data dir holding state.db; overrides --config")
	fs.StringVar(&o.statePath, "state-db", envOr("LOBSLAW_STATE_DB", ""),
		"explicit path to state.db; overrides --data-dir and --config")
	fs.StringVar(&o.keyRef, "memory-key-ref", "",
		"memory encryption key ref (env:VAR | file:/path); defaults to [memory.encryption] key_ref, then $LOBSLAW_MEMORY_KEY")
}

// open locates, unlocks and opens state.db. The caller closes the
// returned store; the resolved path comes back too so error and
// status messages can name the file the operator actually hit.
func (o *offlineStore) open() (*memory.Store, string, error) {
	// Mirror the run path: the memory key is normally an env: ref and
	// `lobslaw init` writes it into .env, so an operator who can start
	// the node can run these without re-exporting anything by hand.
	// Missing .env is a no-op.
	if err := config.LoadDotenv(envOr("LOBSLAW_ENV", "")); err != nil {
		return nil, "", fmt.Errorf("load .env: %w", err)
	}

	path, err := o.resolveStatePath()
	if err != nil {
		return nil, "", err
	}

	// memory.OpenStore creates the file when it is missing, which
	// would turn a mistyped path into a brand-new empty database
	// reported as "0 records" instead of an error. Offline inspection
	// never wants that.
	if _, err := os.Stat(path); err != nil {
		return nil, "", fmt.Errorf("state.db at %s: %w", path, err)
	}

	key, err := o.resolveKey()
	if err != nil {
		return nil, "", err
	}

	store, err := memory.OpenStore(path, key)
	if err != nil {
		return nil, "", translateOpenError(path, err)
	}
	return store, path, nil
}

func (o *offlineStore) resolveStatePath() (string, error) {
	if o.statePath != "" {
		return o.statePath, nil
	}
	if o.dataDir != "" {
		return filepath.Join(o.dataDir, "state.db"), nil
	}
	cfg, err := o.loadConfig()
	if err != nil {
		return "", err
	}
	if cfg == nil || cfg.Cluster.DataDir == "" {
		return "", errors.New("cannot locate state.db: pass --state-db, --data-dir, " +
			"or --config pointing at a config.toml with [cluster] data_dir set")
	}
	return filepath.Join(cfg.Cluster.DataDir, "state.db"), nil
}

// modelsDir is where builtin embedding models are cached, resolved the
// same way state.db is so `embed-eval` looks in the directory the node
// would use rather than somewhere of its own.
func (o *offlineStore) modelsDir() (string, error) {
	if o.dataDir != "" {
		return o.dataDir, nil
	}
	cfg, err := o.loadConfig()
	if err != nil {
		return "", err
	}
	if cfg == nil || cfg.Cluster.DataDir == "" {
		return "", errors.New("cannot locate the models directory: pass --data-dir, " +
			"or --config pointing at a config.toml with [cluster] data_dir set")
	}
	return cfg.Cluster.DataDir, nil
}

func (o *offlineStore) resolveKey() (crypto.Key, error) {
	var zero crypto.Key

	ref := o.keyRef
	source := "--memory-key-ref"
	if ref == "" {
		cfg, err := o.loadConfig()
		if err != nil {
			return zero, err
		}
		if cfg != nil {
			ref = cfg.Memory.Encryption.KeyRef
			source = "[memory.encryption] key_ref"
		}
	}

	var raw string
	if ref != "" {
		v, err := config.ResolveSecret(ref)
		if err != nil {
			return zero, fmt.Errorf("resolve %s %q: %w", source, ref, err)
		}
		raw = v
	}
	if raw == "" {
		// Last resort, and the only source the standalone offline
		// tools ever read — cmd/backfill-embeddings still requires it,
		// so keeping it here means one exported variable serves both.
		raw = os.Getenv("LOBSLAW_MEMORY_KEY")
	}
	if raw == "" {
		return zero, errors.New("no memory encryption key: pass --memory-key-ref, " +
			"set [memory.encryption] key_ref in --config, or export LOBSLAW_MEMORY_KEY")
	}

	key, err := crypto.ParseKey(raw)
	if err != nil {
		return zero, fmt.Errorf("parse memory key: %w", err)
	}
	return key, nil
}

// loadConfig loads the config at most once. A nil config with a nil
// error means no --config was given and none was discovered — the
// caller decides whether it needed one.
func (o *offlineStore) loadConfig() (*config.Config, error) {
	if o.cfgLoaded {
		return o.cfg, o.cfgErr
	}
	o.cfgLoaded = true
	cfg, err := config.Load(config.LoadOptions{Path: o.configPath})
	if err != nil {
		if o.configPath == "" {
			// Nothing was asked for explicitly, so a broken or absent
			// ambient config is not this command's problem — as long
			// as the flags supplied everything else.
			o.cfg, o.cfgErr = nil, nil
			return nil, nil
		}
		o.cfgErr = fmt.Errorf("load config %q: %w", o.configPath, err)
		return nil, o.cfgErr
	}
	o.cfg = cfg
	return cfg, nil
}

// translateOpenError turns bbolt's file-lock timeout into the sentence
// the operator needs.
//
// memory.OpenStore passes bolt.Options{Timeout: 5s}. When a running
// node already holds the exclusive lock, the open blocks for five
// seconds and then fails with a bare "timeout" — which reads like a
// network or disk problem and says nothing about the actual cause. The
// filesystem lock is what enforces the stopped-node requirement for
// every one of these subcommands, so it deserves to say so.
func translateOpenError(path string, err error) error {
	if errors.Is(err, bolterrors.ErrTimeout) {
		return fmt.Errorf("state.db at %s is locked by another process — the node is running; "+
			"stop it first. These subcommands read and write the database file directly and "+
			"cannot share it with a live node", path)
	}
	return err
}
