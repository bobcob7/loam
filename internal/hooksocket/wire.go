package hooksocket

// AgentIdentity is the pushing agent's identity as the hook process reads
// it from its own inherited environment (LOAM_AGENT_NAME/_ID/_ROLE --
// internal/handler/git's serveRPC doc comment: these, plus LOAM_REPO, are
// the only identity a receive-pack subprocess's environment ever carries),
// forwarded verbatim over the wire. EvaluatePush only ever reads Name
// (rule 2's author check); ID and Role travel for observability and
// future policy rules, not because today's evaluation needs them.
type AgentIdentity struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	Role string `json:"role"`
}

// RefUpdateWire is the wire encoding of one pre-receive stdin line
// ("<old-sha> <new-sha> <ref>", one per proposed ref in the push).  Named
// distinctly from internal/refpolicy.RefUpdate (identical fields) because
// this package owns the JSON tags -- refpolicy stays transport-free per
// its own package doc comment, so it never wears a json tag itself.
type RefUpdateWire struct {
	OldSHA string `json:"old_sha"`
	NewSHA string `json:"new_sha"`
	Ref    string `json:"ref"`
}

// Request is one whole pre-receive hook invocation's worth of proposed ref
// updates, sent as exactly one JSON object per connection (docs/git-
// spec.md "Enforcement Mechanics": "it sends one request over the socket
// ... and gets back a per-ref verdict" -- one invocation, one connection,
// one request, is what gives the whole push its atomicity: every ref
// update in a single push is evaluated together, in the same call to
// EvaluatePush).
type Request struct {
	Repo    string          `json:"repo"`
	Agent   AgentIdentity   `json:"agent"`
	Updates []RefUpdateWire `json:"updates"`
}

// VerdictWire is one ref's decision, the wire encoding of
// internal/refpolicy.RefVerdict.
type VerdictWire struct {
	Ref     string `json:"ref"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// Response is the server's one reply to a Request. Accepted is the
// authoritative, already-aggregated atomicity verdict --
// refpolicy.EvaluatePush's own allAllowed return value, carried across the
// wire as-is -- specifically so the hook client trusts THIS field to
// decide whether to accept or reject the whole push, rather than
// re-deriving "were all of these individually allowed" itself from
// Verdicts: a client-side aggregation bug (e.g. only checking the last
// entry) can then never accidentally accept a partially-rejected push,
// because the client never has to perform that aggregation at all.
type Response struct {
	Accepted bool          `json:"accepted"`
	Verdicts []VerdictWire `json:"verdicts"`
}
