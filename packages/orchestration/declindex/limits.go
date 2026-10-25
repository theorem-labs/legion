package declindex

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// indexEntryLimit is the most bytes of names that one entry of the index's B-tree indexes may hold. A B-tree
// entry holds at most 2,704 bytes on Postgres's 8 KB page; this leaves room for the entry's header and the length
// bytes of each column, and assumes no compression, since a declaration name is not a repeated text.
//
// The entry that holds the most names is edges_owner's, which the reach walk reads alone: the user's module and
// name, and the owner's module and name, and the kind (edgeEntryBytes). Two names of about 1.3 KB fill it. The
// longest edge of the copy's index on 2026-10-25 held 474 bytes of names, so a tree reaches the limit only with
// names several times longer than any it holds today. Every other index holds one name, or one name and a module.
const indexEntryLimit = 2600

// NameTooLongError is an index defect: the tree declares or uses a name that the index cannot hold in one entry
// of its indexes. The refresh refuses the tree (exit 3) rather than fail inside the database, and names what to
// shorten.
type NameTooLongError struct {
	// What holds the names: "the declaration", "the use of", ...
	What string
	// Names are the names of the entry, each with its module, in the entry's order.
	Names []string
	// Bytes is how many bytes of names the entry would hold.
	Bytes int
}

func (e *NameTooLongError) Error() string {
	msg := fmt.Sprintf("index defect: %s", e.What)
	for i, n := range e.Names {
		sep := " "
		if i > 0 {
			sep = ", "
		}
		msg += fmt.Sprintf("%s%s (%d bytes)", sep, n, len(n))
	}
	return msg + fmt.Sprintf(" holds %d bytes of names, and one entry of the index holds at most %d: shorten the longest name",
		e.Bytes, indexEntryLimit)
}

// edgeEntryBytes are the bytes of names that a row of edges puts in one entry of edges_owner: its key (the
// owner's module and name) and the columns it carries (the user's name and module, and the user's kind).
func edgeEntryBytes(ownerModule, owner, dst, module, kind string) int {
	return len(ownerModule) + len(owner) + len(dst) + len(module) + len(kind)
}

// checkEdge refuses an edge whose names do not fit one entry of edges_owner.
func checkEdge(target, ownerModule, owner, dst, module, kind string) error {
	if n := edgeEntryBytes(ownerModule, owner, dst, module, kind); n > indexEntryLimit {
		return &NameTooLongError{What: "the use of " + owner + " by", Names: []string{dst, target}, Bytes: n}
	}
	return checkName("the constant", target, module)
}

// checkName refuses a name that, with its module, does not fit one entry of an index of one name.
func checkName(what, name, module string) error {
	if n := len(name) + len(module); n > indexEntryLimit {
		return &NameTooLongError{What: what, Names: []string{name}, Bytes: n}
	}
	return nil
}

// checkReference refuses a reference read whose names the index cannot hold: each declaration with its module,
// and each use's constant and user. It runs before the reference write, so a refused tree writes nothing.
func checkReference(results []ModuleData, errs []error) error {
	for i, md := range results {
		if errs[i] != nil {
			continue
		}
		if err := checkName("the module", md.Module, ""); err != nil {
			return err
		}
		for _, d := range md.Decls {
			if err := checkName("the declaration", d.Name, md.Module); err != nil {
				return err
			}
		}
		for _, u := range md.Uses {
			if err := checkName("the constant", u.Target, u.TargetModule); err != nil {
				return err
			}
			if err := checkName("the declaration", u.Parent, md.Module); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkMovedOwners refuses a rewrite of the owner columns that would make an edge's names overflow its entry of
// edges_owner: a landing that gives a constant a longer owner moves every row of it, whoever the user is.
func checkMovedOwners(ctx context.Context, tx pgx.Tx, target, tmodule, owner, ownerModule []string) error {
	var dst, module, kind, newOwner string
	var n int
	err := tx.QueryRow(ctx, `
		SELECT e.dst, e.module, e.kind, u.owner,
		       octet_length(u.owner_module) + octet_length(u.owner) + octet_length(e.dst) + octet_length(e.module) + octet_length(e.kind)
		  FROM unnest($1::text[], $2::text[], $3::text[], $4::text[]) AS u (target, tmodule, owner, owner_module)
		  JOIN edges e ON e.target = u.target AND e.tmodule = u.tmodule
		 ORDER BY 5 DESC LIMIT 1`, target, tmodule, owner, ownerModule).Scan(&dst, &module, &kind, &newOwner, &n)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if n > indexEntryLimit {
		return &NameTooLongError{What: "the use of " + newOwner + " by", Names: []string{dst}, Bytes: n}
	}
	return nil
}
