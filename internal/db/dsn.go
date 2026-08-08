package db

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// errMalformedKeywordValueDSN is returned when a keyword/value DSN cannot be
// tokenized. Unreachable in practice on the MigrationDSN path -- pgx has
// already parsed the same string successfully by the time the rewriter runs
// -- but returned rather than ignored so a future divergence between this
// tokenizer and pgconn's surfaces as an error instead of a silently
// truncated DSN.
var errMalformedKeywordValueDSN = errors.New("malformed keyword/value connection string")

// errDSNRewriteChangedTarget is returned when the DSN left after removing
// pgxpool's parameters no longer describes the same connection, which would
// mean this package's rewriter is wrong. Failing the boot here is the point:
// the alternative is connecting somewhere unintended.
var errDSNRewriteChangedTarget = errors.New("rewritten connection string no longer matches the original")

// asciiSpace is the whitespace set pgconn's keyword/value parser treats as a
// token separator (pgconn/config.go's asciiSpace table). Mirrored here
// because MigrationDSN's keyword/value rewriter must tokenize the DSN the
// same way pgx does, or it would cut a token in the wrong place.
const asciiSpace = " \t\n\r\v\f"

// MigrationDSN returns dsn with every connection parameter pgxpool consumes
// for itself (pool_max_conns and friends) removed, leaving a DSN that is
// safe to hand to a database/sql driver. Callers keep the ORIGINAL dsn for
// pgxpool -- NewPool needs those parameters, they are what they are for.
//
// WHY THIS EXISTS. One operator-supplied LOAM_DATABASE_URL feeds two
// consumers with different parsers: migrations.Migrate (database/sql, via
// pgx's stdlib driver) and NewPool (pgxpool). Only pgxpool knows what
// pool_max_conns means. pgx's plain ParseConfig files every key it does not
// recognize into Config.RuntimeParams, which are sent to the server in the
// startup packet, and the server rejects them:
//
//	FATAL: unrecognized configuration parameter "pool_max_conns" (SQLSTATE 42704)
//
// So a documented, entirely reasonable DSN stops the server booting, and --
// on any path that builds the pool anyway after migrations failed -- the
// operator's next line is "vector type not found in the database", which
// sends them to inspect a pgvector extension that is fine (loam-lhc9).
// internal/ingest's test harness hit the same defect first (loam-9v9s).
//
// WHICH KEYS GET STRIPPED, AND WHY THAT DOES NOT ROT. Not a hand-written
// deny-list: a list would be correct only until the next pgx release adds a
// pool parameter, which is exactly how this class of bug survives its own
// fix. Instead the set is derived from pgx itself, at runtime, from the
// version actually linked in. pgxpool.ParseConfig calls pgx.ParseConfig and
// then DELETES each parameter it claims from ConnConfig.RuntimeParams
// (pgxpool/pool.go) -- because pgxpool has the same problem this function
// does and solves it for its own connections. Parsing dsn both ways and
// taking the difference therefore asks pgxpool which keys it owns rather
// than asserting an answer. A new pool parameter is picked up by upgrading
// pgx and nothing else.
//
// That deletion is not an implementation detail this function got lucky
// coupling to: it is LOAD-BEARING FOR PGXPOOL ITSELF. A pgxpool that stopped
// deleting its own keys would leave them in RuntimeParams and send them to
// the server in its own startup packet, so every connection pgxpool opened
// would be refused with the very error above. The behaviour this function
// reads is one pgx must maintain to keep working at all, which is a stronger
// guarantee than "true in the version we tested against".
//
// A DSN with no such parameters is returned byte-for-byte unchanged, so the
// common case never depends on the rewriting below being faithful.
func MigrationDSN(dsn string) (string, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("parsing database url: %w", err)
	}
	fullCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("parsing database url: %w", err)
	}
	owned := poolOwnedKeys(fullCfg, poolCfg)
	if len(owned) == 0 {
		return dsn, nil
	}
	stripped, err := stripParams(dsn, owned)
	if err != nil {
		return "", err
	}
	if err := verifyStripped(stripped, poolCfg.ConnConfig); err != nil {
		return "", err
	}
	return stripped, nil
}

// poolOwnedKeys returns the runtime parameters pgxpool.ParseConfig consumed
// for itself: the keys pgx's plain parser kept that pgxpool's parser
// deleted. See MigrationDSN's doc comment for why the set is computed this
// way rather than enumerated.
func poolOwnedKeys(fullCfg *pgx.ConnConfig, poolCfg *pgxpool.Config) map[string]struct{} {
	owned := make(map[string]struct{})
	for key := range fullCfg.RuntimeParams {
		if _, kept := poolCfg.ConnConfig.RuntimeParams[key]; !kept {
			owned[key] = struct{}{}
		}
	}
	return owned
}

// stripParams removes the named parameters from dsn, dispatching on the two
// forms pgx accepts (pgconn/config.go decides between them with exactly this
// prefix test).
func stripParams(dsn string, owned map[string]struct{}) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return stripURLParams(dsn, owned)
	}
	return stripKeywordValueParams(dsn, owned)
}

// stripURLParams rewrites only the query component of a postgres:// URL,
// deliberately without round-tripping the whole string through url.Parse:
// the host component of a libpq URL may carry a comma-separated multi-host
// list, and re-serializing that is a risk taken for no benefit when the
// parameters being removed can only ever live in the query.
func stripURLParams(dsn string, owned map[string]struct{}) (string, error) {
	head, rest, found := strings.Cut(dsn, "?")
	if !found {
		return dsn, nil
	}
	query, fragment, hasFragment := strings.Cut(rest, "#")
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", fmt.Errorf("parsing database url query: %w", err)
	}
	for key := range owned {
		values.Del(key)
	}
	out := head
	if encoded := values.Encode(); encoded != "" {
		out += "?" + encoded
	}
	if hasFragment {
		out += "#" + fragment
	}
	return out, nil
}

// stripKeywordValueParams rewrites a libpq keyword/value DSN
// (`host=x user=y pool_max_conns=8`). It tokenizes exactly the way
// pgconn's parseKeywordValueSettings does -- including single-quoted values
// and backslash escapes -- and rebuilds the DSN from the RAW text of the
// tokens it keeps, so an operator's quoting and escaping survive untouched
// rather than being re-serialized by rules of this package's own invention.
// Only the whitespace between tokens is normalized, which is a separator
// and carries no meaning.
func stripKeywordValueParams(dsn string, owned map[string]struct{}) (string, error) {
	var kept []string
	rest := strings.TrimLeft(dsn, asciiSpace)
	for rest != "" {
		key, valueEnd, err := scanKeywordValue(rest)
		if err != nil {
			return "", err
		}
		if _, drop := owned[key]; !drop {
			kept = append(kept, rest[:valueEnd])
		}
		rest = strings.TrimLeft(rest[valueEnd:], asciiSpace)
	}
	return strings.Join(kept, " "), nil
}

// scanKeywordValue reads one `key=value` token off the front of s and
// returns the key plus the offset in s just past the token's value.
func scanKeywordValue(s string) (string, int, error) {
	eq := strings.IndexRune(s, '=')
	if eq < 0 {
		return "", 0, fmt.Errorf("parsing database url: %w", errMalformedKeywordValueDSN)
	}
	key := strings.Trim(s[:eq], asciiSpace)
	value := strings.TrimLeft(s[eq+1:], asciiSpace)
	consumed := len(s) - len(value)
	end, err := scanKeywordValueValue(value)
	if err != nil {
		return "", 0, err
	}
	return key, consumed + end, nil
}

// scanKeywordValueValue returns the length of the value token at the front
// of value, which is either single-quoted or terminated by whitespace, with
// backslash escapes honored in both cases.
func scanKeywordValueValue(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	if value[0] == '\'' {
		for i := 1; i < len(value); i++ {
			if value[i] == '\'' {
				return i + 1, nil
			}
			if value[i] == '\\' {
				i++
			}
		}
		return 0, fmt.Errorf("parsing database url: %w", errMalformedKeywordValueDSN)
	}
	for i := 0; i < len(value); i++ {
		if strings.IndexByte(asciiSpace, value[i]) >= 0 {
			return i, nil
		}
		if value[i] == '\\' {
			i++
			if i == len(value) {
				return 0, fmt.Errorf("parsing database url: %w", errMalformedKeywordValueDSN)
			}
		}
	}
	return len(value), nil
}

// verifyStripped re-parses the rewritten DSN and checks it still describes
// the same connection pgxpool will make, differing only in the pool
// parameters that were the point of the exercise. This is the guard that
// keeps the string surgery above from being taken on trust: a rewriter that
// mangled a quoted password or dropped a host would otherwise produce a DSN
// that fails much later, somewhere else, with a worse message -- which is
// the failure mode this whole function exists to prevent.
func verifyStripped(stripped string, want *pgx.ConnConfig) error {
	got, err := pgx.ParseConfig(stripped)
	if err != nil {
		return fmt.Errorf("re-parsing database url after removing pool parameters: %w", err)
	}
	sameTarget := got.Host == want.Host &&
		got.Port == want.Port &&
		got.Database == want.Database &&
		got.User == want.User &&
		got.Password == want.Password &&
		got.ConnectTimeout == want.ConnectTimeout &&
		len(got.Fallbacks) == len(want.Fallbacks) &&
		(got.TLSConfig == nil) == (want.TLSConfig == nil) &&
		maps.Equal(got.RuntimeParams, want.RuntimeParams)
	if !sameTarget {
		return fmt.Errorf("removing pool parameters from database url: %w", errDSNRewriteChangedTarget)
	}
	return nil
}
