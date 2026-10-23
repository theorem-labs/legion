package claim

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// issueKey is a Dispatch issue key, the one shape an issue has (legion-roles.ts:30).
	issueKey = regexp.MustCompile(`^([A-Z][A-Z0-9]*)-([0-9]+)$`)
	// projectToken is a project as a token spells it (legion-roles.ts:28).
	projectToken = regexp.MustCompile(`^[a-z0-9]+$`)
	// notAlphanumeric is everything ProjectToken drops.
	notAlphanumeric = regexp.MustCompile(`[^a-z0-9]`)
	// envoyRoleToken is the shape of every Envoy role token (legion-roles.ts ENVOY_ROLE_TOKEN).
	envoyRoleToken = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// Roles are the six roles a claim can be on, in the order the workflow reaches them.
var Roles = []Role{RoleArchitect, RolePlanner, RoleImplementer, RoleTester, RoleReviewer, RoleMerger}

// ProjectToken is the project as every token, and the private tmux server, spell it: the
// operator's `project` lowercased with every character outside [a-z0-9] dropped — the shipped
// daemon's rule (`legionProjectToken`, packages/contracts/src/legion-roles.ts), so `sjawhar/legion`
// names the same role topics under either daemon.
func ProjectToken(project string) (string, error) {
	token := notAlphanumeric.ReplaceAllString(strings.ToLower(project), "")
	if token == "" {
		return "", fmt.Errorf("project %q must include at least one alphanumeric character", project)
	}
	return token, nil
}

// IsIssueKey reports whether key is a Dispatch issue key (`LEGION-208`).
func IsIssueKey(key string) bool { return issueKey.MatchString(key) }

// IsRole reports whether role is one of the six a claim can be on.
func IsRole(role Role) bool {
	for _, known := range Roles {
		if role == known {
			return true
		}
	}
	return false
}

// NewToken is the token of the claim on issue's role: `legion-<project>-<issue project,
// lowercased>-<number>-<role>`, byte for byte the shipped roleToken
// (packages/contracts/src/legion-roles.ts:48-55). The token is the Envoy role the agent claims, so
// it has Envoy's role-token shape, and a peer that derives it from the issue and role in either
// language reaches the same agent. project is already a ProjectToken.
func NewToken(project, issue string, role Role) (Token, error) {
	if !projectToken.MatchString(project) {
		return "", fmt.Errorf("claim token: project %q is not a project token", project)
	}
	match := issueKey.FindStringSubmatch(issue)
	if match == nil {
		return "", fmt.Errorf("claim token: %q is not an issue key", issue)
	}
	if !IsRole(role) {
		return "", fmt.Errorf("claim token: %q is not a role", role)
	}
	return Token(fmt.Sprintf("legion-%s-%s-%s-%s", project, strings.ToLower(match[1]), match[2], role)), nil
}

// Cut splits a role claim's token into the issue's part, `legion-<project>-<issue>`, which every
// role claim of the issue shares, and the role the token ends in; ok is false for a token that
// ends in no role, such as the controller's.
func (t Token) Cut() (issue string, role Role, ok bool) {
	for _, role := range Roles {
		if prefix, found := strings.CutSuffix(string(t), "-"+string(role)); found {
			return prefix, role, true
		}
	}
	return string(t), "", false
}

// ControllerToken is the project controller's role token, `legion-<project>-controller`
// (legion-roles.ts:66-69). project is already a ProjectToken.
func ControllerToken(project string) Token { return Token("legion-" + project + "-controller") }

// OwnerToken is the standing owner's token, `legion-<project>-owner-<EncodeOwnerName(key)>`, where
// key is the owner's ledger key: a graph node id such as `function:get_mountpoint`, or a named role
// such as `librarian`. The encoding is reversible (Token.OwnerKey), so two keys never share a token.
// project is already a ProjectToken.
func OwnerToken(project, key string) (Token, error) {
	if !projectToken.MatchString(project) {
		return "", fmt.Errorf("owner token: project %q is not a project token", project)
	}
	name, err := EncodeOwnerName(key)
	if err != nil {
		return "", fmt.Errorf("owner token: %w", err)
	}
	token := Token("legion-" + project + "-owner-" + name)
	if len(token) > MaxOwnerTokenLength {
		return "", fmt.Errorf("owner token: %q is %d bytes, past the %d an owner token may be", token, len(token), MaxOwnerTokenLength)
	}
	return token, nil
}

// OwnerKey is the ledger key an owner token of project encodes.
func (t Token) OwnerKey(project string) (string, bool) {
	name, ok := strings.CutPrefix(string(t), "legion-"+project+"-owner-")
	if !ok {
		return "", false
	}
	key, err := DecodeOwnerName(name)
	return key, err == nil
}

// Role is the role t names: one of the two tree-independent roles (owner and controller), or the
// workflow role it ends in. An owner token is read first: an owner key starts with a lowercase
// letter, and a workflow token's issue part after `owner-` would start with its number.
func (t Token) Role() (role Role, ok bool) {
	project, rest, prefixed := strings.Cut(strings.TrimPrefix(string(t), "legion-"), "-")
	if strings.HasPrefix(string(t), "legion-") && prefixed && projectToken.MatchString(project) {
		if _, isOwner := t.OwnerKey(project); isOwner && strings.HasPrefix(rest, "owner-") {
			return RoleOwner, true
		}
		if rest == string(RoleController) {
			return RoleController, true
		}
	}
	if _, role, ok := t.Cut(); ok {
		return role, true
	}
	return "", false
}
