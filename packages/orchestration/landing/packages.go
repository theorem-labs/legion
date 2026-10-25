package landing

import "context"

// manifestPath is Lake's manifest; a member that changes it can change the
// packages a build needs.
const manifestPath = "lake-manifest.json"

// dropManifestChanges returns, with packages_changed, each candidate member
// whose diff touches lake-manifest.json, after the package store had no entry
// for the candidate's manifest. It reports whether it returned any; with none,
// the batch cannot be built and stops as unable to check.
func (r Runner) dropManifestChanges(ctx context.Context, batch *Batch) (bool, error) {
	reasons := map[string]string{}
	for _, member := range candidateMembers(*batch) {
		for _, changed := range member.Changed {
			if changed == manifestPath {
				reasons[member.Migration] = "packages_changed"
			}
		}
	}
	if len(reasons) == 0 {
		return false, nil
	}
	return true, r.dropMembers(ctx, batch, reasons)
}
