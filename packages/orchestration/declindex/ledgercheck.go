package declindex

import (
	"context"
	"database/sql"
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
	Contracts(ctx context.Context, repo string) ([]ledger.Contract, error)
	RegisterContract(ctx context.Context, repo, name, owner, hash string, pinned bool) (ledger.Contract, error)
}

// Owners maps a contract's directory to its owner key (a graph node id such as function:open_file).
type Owners map[string]string

// LoadOwners reads an owner map: {"format": F, "owners": [{"id": <owner key>, "contract": <directory>|[<directory>,
// …]|null}, …]}. A list names every directory of one owner, such as a header helper's directory under each file
// that includes it. format is the F the deployment expects (its graph tool's format name); empty accepts any.
func LoadOwners(path, format string) (Owners, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Format string `json:"format"`
		Owners []struct {
			ID       string          `json:"id"`
			Contract json.RawMessage `json:"contract"`
		} `json:"owners"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if format != "" && f.Format != format {
		return nil, fmt.Errorf("%s: format %q, the configuration expects %q", path, f.Format, format)
	}
	out := Owners{}
	for _, o := range f.Owners {
		dirs, err := contractDirs(o.Contract)
		if err != nil {
			return nil, fmt.Errorf("%s: owner %s: %w", path, o.ID, err)
		}
		for _, dir := range dirs {
			if prev, ok := out[dir]; ok && prev != o.ID {
				return nil, fmt.Errorf("%s: directory %s has two owners, %s and %s", path, dir, prev, o.ID)
			}
			out[dir] = o.ID
		}
	}
	return out, nil
}

// contractDirs reads an owner's contract field: null, one directory, or a list of directories.
func contractDirs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil, nil
		}
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("contract must be a directory, a list of directories, or null: %s", raw)
	}
	for _, dir := range many {
		if dir == "" {
			return nil, errors.New("contract list has an empty directory")
		}
	}
	return many, nil
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
	var out []Found
	rows, err := s.db.QueryContext(ctx, `SELECT d.name, d.short, d.module, d.kind, d.line, d.stmt_hash, COALESCE(m.rel, '')
		FROM decls d LEFT JOIN modules m ON m.module = d.module WHERE d.kind IN (`+placeholders(len(rule.Kinds))+`)
		ORDER BY d.name, d.module`, anys(rule.Kinds)...)
	err = eachRow(rows, err, func(r *sql.Rows) error {
		var f Found
		var short, rel string
		if err := r.Scan(&f.Name, &short, &f.Module, &f.Kind, &f.Line, &f.StmtHash, &rel); err != nil {
			return err
		}
		f.Path = sourcePath(rel, f.Module)
		if !name.MatchString(short) {
			return nil
		}
		for _, g := range globs {
			if g.MatchString(f.Path) {
				out = append(out, f)
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
	Commit     string         `json:"-"`
	Contracts  int            `json:"contracts"`
	Registered []string       `json:"registered"`
	Matching   int            `json:"matching"`
	Unowned    []string       `json:"unowned"`
	Ambiguous  []string       `json:"ambiguous"`
	Defects    []LedgerDefect `json:"defects"`
	// Gone are Lean-named ledger contracts that the index no longer declares at any module.
	Gone []string `json:"gone"`
}

// Defective reports whether the contract portion of a ledger check found a condition that makes the command exit 1.
func (r LedgerReport) Defective() bool {
	return len(r.Defects) != 0 || len(r.Gone) != 0 || len(r.Unowned) != 0 || len(r.Ambiguous) != 0
}

type ledgerDeclaration struct {
	module string
	hash   string
}

func (s *Store) ledgerDeclarations(ctx context.Context, names []string) (map[string][]ledgerDeclaration, error) {
	out := make(map[string][]ledgerDeclaration, len(names))
	for i := 0; i < len(names); i += 500 {
		chunk := names[i:min(i+500, len(names))]
		rows, err := s.db.QueryContext(ctx, `SELECT name, module, stmt_hash FROM decls WHERE name IN (`+
			placeholders(len(chunk))+`)`, anys(chunk)...)
		err = eachRow(rows, err, func(r *sql.Rows) error {
			var name string
			var declaration ledgerDeclaration
			if err := r.Scan(&name, &declaration.module, &declaration.hash); err != nil {
				return err
			}
			out[name] = append(out[name], declaration)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CheckLedger runs after a landing, on the index of the landed build. It registers every contract the rule
// selects that the ledger lacks and that has an owner, at version 1 with the index's statement hash. It then
// checks every Lean-named ledger contract against declarations of that exact name in the index. It never advances
// a contract: only a landing does. The hash is the statement hash (StatementHash), the hash a change lock records
// for its reads.
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
	all, err := l.Contracts(ctx, repo)
	if err != nil {
		return r, fmt.Errorf("list the ledger's contracts: %w", err)
	}
	inLedger := make(map[string]ledger.Contract, len(all))
	for _, c := range all {
		inLedger[c.Name] = c
	}
	count := map[string]int{}
	for _, c := range contracts {
		count[c.Name]++
	}
	selected := make(map[string]Found, len(contracts))
	for _, c := range contracts {
		if count[c.Name] > 1 {
			// One ledger record per name: a name declared twice cannot be told apart there.
			r.Ambiguous = append(r.Ambiguous, c.Name+" "+c.At())
			continue
		}
		r.Contracts++
		selected[c.Name] = c
		if _, ok := inLedger[c.Name]; ok {
			continue
		}
		owner := owners[dirOf(c.Path)]
		if owner == "" {
			r.Unowned = append(r.Unowned, c.Name)
			continue
		}
		if c.StmtHash == "" {
			return r, fmt.Errorf("contract %s has no statement hash: its source file is missing or newer than its build", c.Name)
		}
		if _, err := l.RegisterContract(ctx, repo, c.Name, owner, c.StmtHash, false); err != nil {
			return r, fmt.Errorf("register %s: %w", c.Name, err)
		}
		r.Registered = append(r.Registered, c.Name)
	}
	names := make([]string, 0, len(all))
	for _, c := range all {
		if !strings.Contains(c.Name, ":") {
			names = append(names, c.Name)
		}
	}
	declarations, err := s.ledgerDeclarations(ctx, names)
	if err != nil {
		return r, err
	}
	for _, c := range all {
		if strings.Contains(c.Name, ":") {
			continue
		}
		declarations := declarations[c.Name]
		switch len(declarations) {
		case 0:
			r.Gone = append(r.Gone, c.Name)
		case 1:
			if c.StatementHash == declarations[0].hash {
				r.Matching++
				continue
			}
			at := sourcePath("", declarations[0].module)
			if found, ok := selected[c.Name]; ok {
				at = found.At()
			}
			r.Defects = append(r.Defects, LedgerDefect{Contract: c.Name, At: at, LedgerHash: c.StatementHash,
				Version: c.Version, IndexHash: declarations[0].hash})
		default:
			r.Ambiguous = append(r.Ambiguous, c.Name)
		}
	}
	sort.Strings(r.Ambiguous)
	return r, nil
}
