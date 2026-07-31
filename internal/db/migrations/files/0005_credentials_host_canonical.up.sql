-- Rewrites credentials.host rows stored under a scheme-qualified https
-- form ("https://git.example.com") to the bare canonical form
-- (loam-0hjq) that internal/handler/repoadmin's forgeHostOf --
-- ProbeRepo/EnrollRepo's credential lookup -- has always derived for an
-- https upstream, and that internal/forgehost.Canonicalize now enforces
-- at every future SetUpstreamToken write. Before this bead, SetUpstreamToken
-- stored req.Msg.GetHost() verbatim after only strings.TrimSpace: a
-- credential entered as "https://git.example.com" would VALIDATE
-- (internal/forge's apiBaseURL tolerates a scheme-qualified host) and
-- report validated=true, yet could never be found by the bare-host
-- derivation ProbeRepo/EnrollRepo actually use --
-- credentialstore.GetByHost is an exact string match with no
-- normalization of its own. Confirmed on a live deployment: a row with
-- host = 'https://git.bobcob7.com', validated = t, that ProbeRepo/EnrollRepo
-- reported as "no usable credential for host git.bobcob7.com".
--
-- Only rows matching scheme + bare host, with at most one trailing slash,
-- are touched (the shape Canonicalize itself tolerates as "differently
-- spelled, not wrong") -- a host carrying a path, query, fragment, or
-- userinfo was already a malformed row Canonicalize would now reject
-- outright at write time, and there is no safe rewrite for one that
-- predates this migration; none is known to exist (the live incident this
-- migration was written from carries no path component), so this is
-- deliberately narrow rather than a best-effort guess at one.
--
-- TWO THINGS TO GET RIGHT, verified before writing this migration:
--
-- 1. internal/crypto's Encryptor.Encrypt/Decrypt pass nil as the AES-GCM
--    additional authenticated data (Seal(buf, nonce, plaintext, nil) /
--    Open(nil, nonce, sealed, nil), internal/crypto/crypto.go) -- the
--    encryption does NOT bind the host string. Rewriting the host column
--    is therefore safe on its own: token_ciphertext is untouched by this
--    migration and remains decryptable exactly as before, with no
--    re-encryption step required.
--
-- 2. A rewrite CAN collide with an existing bare-host row for the same
--    forge: credentials_host_key is a real UNIQUE(host) constraint
--    (0001_init.up.sql), and an operator who set a credential under BOTH
--    spellings -- plausible, and specifically documented as the incident's
--    own workaround ("set the token again ... using the bare host") --
--    leaves exactly this pair of rows. Silently dropping one without a
--    stated rule would risk discarding the admin's most recent, working
--    token in favor of a stale or since-rotated one. The rule applied
--    below is LAST WRITE WINS BY updated_at: whichever of the two rows
--    (scheme-qualified or bare) was written or re-validated more recently
--    survives under the bare key, and the older duplicate is deleted. This
--    is chosen over "always prefer the bare row" because the order in
--    which an operator created the two rows is not recoverable from the
--    data alone, and the most recently written token is the best
--    available signal for which one is still live. Ties (equal
--    updated_at) keep the scheme-qualified row, an arbitrary but
--    deterministic choice.

-- Step 1: resolve collisions before the rename below, so the UPDATE in
-- step 2 never violates credentials_host_key. For every scheme-qualified
-- row whose bare-host counterpart already exists, delete whichever of the
-- pair has the OLDER updated_at.
WITH scheme_qualified AS (
    SELECT id, updated_at,
           regexp_replace(substring(host FROM 9), '/+$', '') AS bare_host
    FROM credentials
    WHERE host ~* '^https://[^/]+/?$'
),
collisions AS (
    SELECT sq.id AS scheme_id, sq.updated_at AS scheme_updated_at,
           bare.id AS bare_id, bare.updated_at AS bare_updated_at
    FROM scheme_qualified sq
    JOIN credentials bare ON bare.host = sq.bare_host
)
DELETE FROM credentials
WHERE id IN (
    SELECT CASE WHEN scheme_updated_at >= bare_updated_at THEN bare_id ELSE scheme_id END
    FROM collisions
);

-- Step 2: rewrite every remaining scheme-qualified row (now collision-free
-- by construction) to the bare canonical form.
UPDATE credentials
SET host = regexp_replace(regexp_replace(host, '^https://', '', 'i'), '/+$', '')
WHERE host ~* '^https://[^/]+/?$';
