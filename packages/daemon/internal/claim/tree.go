package claim

// IsTreeRoot is whether the issue is its tree's root. A tree is named for the issue it grew from,
// so the root is the one issue whose key is the tree's, whichever role is on it.
func IsTreeRoot(issue, tree string) bool { return issue == tree }

// IsTreeArchitect is whether a claim is the architect of its tree's own issue: the tree's root
// claim, one per tree, which ends only when the tree closes. An architect on a child issue is a
// sub-architect, and a worker on the root issue is not the root claim.
func IsTreeArchitect(role Role, issue, tree string) bool {
	return role == RoleArchitect && IsTreeRoot(issue, tree)
}

// IsController is whether a claim is the project controller's: on the controller role, with no
// issue and no tree. IsTreeRoot alone would call such a claim its own tree's root, since its empty
// issue equals its empty tree, so every caller that sorts claims by tree asks this first.
func IsController(role Role, issue, tree string) bool {
	return role == RoleController && issue == "" && tree == ""
}

// IsStandingOwner is whether a claim is a standing owner, with no issue or tree.
func IsStandingOwner(role Role, issue, tree string) bool {
	return role == RoleOwner && issue == "" && tree == ""
}

// IsTreeIndependent is whether a claim has no tree lifecycle to bind or check before launching.
func IsTreeIndependent(role Role, issue, tree string) bool {
	return IsController(role, issue, tree) || IsStandingOwner(role, issue, tree)
}
