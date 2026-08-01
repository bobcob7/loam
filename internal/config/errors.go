package config

import "errors"

var (
	errMissingEnv             = errors.New("missing required environment variable")
	errInvalidEncryptionKey   = errors.New("invalid encryption key")
	errInvalidDuration        = errors.New("invalid duration")
	errInvalidBool            = errors.New("invalid boolean")
	errInvalidInt             = errors.New("invalid integer")
	errInvalidLogLevel        = errors.New("invalid log level")
	errInvalidDatabaseURL     = errors.New("invalid database URL")
	errDatabaseConfigConflict = errors.New("database configuration conflict")
	errDataDirNotWritable     = errors.New("data directory not writable")
	errSyncIntervalRange      = errors.New("sync interval must be positive")
	errIngestWorkersRange     = errors.New("ingest workers out of range")
)
