package ownerharness

import "regexp"

// MintedDedupeKeyPattern is Envoy's rule for a dedupe key that names its message, so that a host drops a second copy
// of it: the listener's publish.<id> and agent.<session>.<id> around 32 lowercase hex digits or a UUID, also behind
// the role arbiter's forward mark. It is MINTED_DEDUPE_KEY_PATTERN of packages/contracts (src/envelope.ts), which
// scripts/gen-go.ts generates into packages/envoy/internal/contracts as MintedDedupeKeyPattern; dedupe_test.go holds
// this copy to that file. A dedupe key of any other shape is no dedupe key to a host (dedupeKeyNamesItsEvent), so a
// second publish under it reaches the session again.
const MintedDedupeKeyPattern = `^(?:envoy\.role\.forward\.)?(?:publish|agent\.[^.]+)\.(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`

var mintedDedupeKey = regexp.MustCompile(MintedDedupeKeyPattern)
