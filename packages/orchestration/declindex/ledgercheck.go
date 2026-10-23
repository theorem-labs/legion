package declindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/sjawhar/legion/orchestration/ledger"
)

// ContractLedger is the part of the contract ledger (package ledger) that the index uses after a landing. A
// contract's hash and version change only when the landing program lands it (ledger.Store.Land); the index
// registers the contracts that are new and checks the others.
type ContractLedger interface {
	Contract(ctx context.Context, repo, name string) (ledger.Contract, error)
	RegisterContract(ctx context.Context, repo, name, owner, hash string, pinned bool) (ledger.Contract, error)
}

// Owners maps a contract's directory to its owner key (a graph node id such as function:do_mount_setattr).
type Owners map[string]string

// LoadOwners reads an owners/v1 file: {"format":"owners/v1","owners":[{"id":…,"contract":<directory>|null},…]}.
func LoadOwners(path string) (Owners, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Format string `json:"format"`
		Owners []struct {
			ID       string  `json:"id"`
			Contract *string `json:"contract"`
		} `json:"owners"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Format != "owners/v1" {
		return nil, fmt.Errorf("%s: format %q, this reader reads owners/v1", path, f.Format)
	}
	out := Owners{}
	for _, o := range f.Owners {
		if o.Contract == nil || *o.Contract == "" {
			continue
		}
		if prev, ok := out[*o.Contract]; ok && prev != o.ID {
			return nil, fmt.Errorf("%s: directory %s has two owners, %s and %s", path, *o.Contract, prev, o.ID)
		}
		out[*o.Contract] = o.ID
	}
	return out, nil
}

// globRegexp turns a path glob into a regular expression: `**/` matches any number of directories, `*` any
// characters but `/`, `?` one of them.
func globRegexp(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case glob[i] == '*':
			b.WriteString("[^/]*")
		case glob[i] == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// Contracts lists the declarations that the rule selects.
func (s *Store) Contracts(ctx context.Context, rule ContractRule) ([]Found, error) {
	if len(rule.Paths) == 0 || len(rule.Kinds) == 0 {
		return nil, errors.New("the contract rule needs paths and kinds")
	}
	var globs []*regexp.Regexp
	for _, p := range rule.Paths {
		re, err := globRegexp(p)
		if err != nil {
			return nil, err
		}
		globs = append(globs, re)
	}
	name := regexp.MustCompile(".*")
	if rule.Name != "" {
		var err error
		if name, err = regexp.Compile(rule.Name); err != nil {
			return nil, fmt.Errorf("contract name rule: %w", err)
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.name, d.short, d.module, d.kind, d.line, d.stmt_hash, COALESCE(m.rel, '')
		FROM decls d LEFT JOIN modules m ON m.module = d.module WHERE d.kind IN (`+placeholders(len(rule.Kinds))+`)
		ORDER BY d.name, d.module`, anys(rule.Kinds)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Found
	for rows.Next() {
		var f Found
		var short, rel string
		if err := rows.Scan(&f.Name, &short, &f.Module, &f.Kind, &f.Line, &f.StmtHash, &rel); err != nil {
			return nil, err
		}
		f.Path = sourcePath(rel, f.Module)
		if !name.MatchString(short) {
			continue
		}
		for _, g := range globs {
			if g.MatchString(f.Path) {
				out = append(out, f)
				break
			}
		}
	}
	return out, rows.Err()
}

// LedgerDefect is a contract whose hash in the ledger is not the index's hash at the index's commit: a landing
// changed the contract's statement without advancing it, or advanced it to a hash the build does not have.
type LedgerDefect struct {
	Contract   string `json:"contract"`
	At         string `json:"at"`
	LedgerHash string `json:"ledger_hash"`
	Version    int64  `json:"version"`
	IndexHash  string `json:"index_hash"`
}

// LedgerReport is what one check of the ledger against the index found.
type LedgerReport struct {
	Commit     string         `json:"commit"`
	Contracts  int            `json:"contracts"`
	Registered []string       `json:"registered"`
	Matching   int            `json:"matching"`
	Unowned    []string       `json:"unowned"`
	Ambiguous  []string       `json:"ambiguous"`
	Defects    []LedgerDefect `json:"defects"`
}

// CheckLedger runs after a landing, on the index of the landed build. It registers every contract the rule
// selects that the ledger lacks and that has an owner, at version 1 with the index's statement hash, and reports
// every registered contract whose ledger hash differs from the index's. It never advances a contract: only a
// landing does. The hash is the statement hash (StatementHash), the hash a change lock records for its reads.
func (s *Store) CheckLedger(ctx context.Context, l ContractLedger, repo string, rule ContractRule, owners Owners) (LedgerReport, error) {
	var r LedgerReport
	var err error
	if r.Commit, err = s.Meta(ctx, "commit"); err != nil {
		return r, err
	}
	contracts, err := s.Contracts(ctx, rule)
	if err != nil {
		return r, err
	}
	count := map[string]int{}
	for _, c := range contracts {
		count[c.Name]++
	}
	for _, c := range contracts {
		if count[c.Name] > 1 {
			// One ledger record per name: a name declared twice cannot be told apart there.
			r.Ambiguous = append(r.Ambiguous, c.Name+" "+c.At())
			continue
		}
		r.Contracts++
		cur, err := l.Contract(ctx, repo, c.Name)
		var nf *ledger.NotFoundError
		switch {
		case errors.As(err, &nf):
			owner := owners[dirOf(c.Path)]
			if owner == "" {
				r.Unowned = append(r.Unowned, c.Name)
				continue
			}
			if c.StmtHash == "" {
				return r, fmt.Errorf("contract %s has no statement hash: its source file is missing", c.Name)
			}
			if _, err := l.RegisterContract(ctx, repo, c.Name, owner, c.StmtHash, false); err != nil {
				return r, fmt.Errorf("register %s: %w", c.Name, err)
			}
			r.Registered = append(r.Registered, c.Name)
		case err != nil:
			return r, fmt.Errorf("read contract %s: %w", c.Name, err)
		case cur.StatementHash == c.StmtHash:
			r.Matching++
		default:
			r.Defects = append(r.Defects, LedgerDefect{Contract: c.Name, At: c.At(), LedgerHash: cur.StatementHash,
				Version: cur.Version, IndexHash: c.StmtHash})
		}
	}
	sort.Strings(r.Ambiguous)
	return r, nil
}
