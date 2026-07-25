package config

import "errors"

var (
	errMissingEnv           = errors.New("missing required environment variable")
	errInvalidEncryptionKey = errors.New("invalid encryption key")
	errInvalidDuration      = errors.New("invalid duration")
	errInvalidBool          = errors.New("invalid boolean")
	errInvalidInt           = errors.New("invalid integer")
	errInvalidLogLevel      = errors.New("invalid log level")
	errInvalidDatabaseURL   = errors.New("invalid database URL")
	errDataDirNotWritable   = errors.New("data directory not writable")
)
