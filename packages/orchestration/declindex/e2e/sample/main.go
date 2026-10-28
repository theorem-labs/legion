// Command sample writes the declaration sample that timings.sh measures: declarations drawn at random with a
// fixed seed, and the declarations that most statements use. It reads the index's schema directly, because no
// command of declindex lists declarations.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/orchestration/declindex"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sample", flag.ContinueOnError)
	flags.SetOutput(stderr)
	config := flags.String("config", "", "the index configuration, for its index: name")
	index := flags.String("index", "", "the index's schema, instead of a configuration")
	dsnFile := flags.String("index-dsn-file", "", "the connection string; else DECLINDEX_DSN")
	out := flags.String("out", "", "where to write the sample; - is stdout")
	seed := flags.Float64("seed", 0.145, "the seed of the random draw, in [-1, 1]")
	random := flags.Int("random", 50, "how many declarations to draw at random")
	used := flags.Int("used", 50, "how many of the most used declarations to take")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *out == "" || (*config == "") == (*index == "") {
		fmt.Fprintln(stderr, "usage: sample (--config FILE | --index NAME) [--index-dsn-file FILE] --out FILE [--seed F] [--random N] [--used N]")
		return 2
	}
	if *seed < -1 || *seed > 1 {
		fmt.Fprintln(stderr, "sample: --seed is a number in [-1, 1]")
		return 2
	}
	if *random < 0 || *used < 0 || *random+*used == 0 {
		fmt.Fprintln(stderr, "sample: --random and --used are counts, and not both zero")
		return 2
	}

	schema := *index
	if *config != "" {
		cfg, err := declindex.LoadConfig(*config)
		if err != nil {
			fmt.Fprintf(stderr, "sample: %v\n", err)
			return 2
		}
		schema = cfg.Index
	}
	if err := declindex.ValidIndexName(schema); err != nil {
		fmt.Fprintf(stderr, "sample: %v\n", err)
		return 2
	}
	dsn := strings.TrimSpace(getenv("DECLINDEX_DSN"))
	if *dsnFile != "" {
		raw, err := os.ReadFile(*dsnFile)
		if err != nil {
			fmt.Fprintf(stderr, "sample: %v\n", err)
			return 2
		}
		dsn = strings.TrimSpace(string(raw))
	}
	if dsn == "" {
		fmt.Fprintln(stderr, "sample: no connection string for the index: set DECLINDEX_DSN or pass --index-dsn-file FILE")
		return 2
	}

	names, err := draw(ctx, dsn, schema, *seed, *random, *used)
	if err != nil {
		fmt.Fprintf(stderr, "sample: %v\n", err)
		return 1
	}
	if len(names) == 0 {
		fmt.Fprintf(stderr, "sample: the index %s has no declaration\n", schema)
		return 1
	}
	body := strings.Join(names, "\n") + "\n"
	if *out == "-" {
		if _, err := io.WriteString(stdout, body); err != nil {
			fmt.Fprintf(stderr, "sample: %v\n", err)
			return 1
		}
	} else if err := os.WriteFile(*out, []byte(body), 0o644); err != nil {
		fmt.Fprintf(stderr, "sample: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "sample: %d declarations of the index %s\n", len(names), schema)
	return 0
}

// draw gives the sample: `random` declarations that setseed and random() pick, so that one seed gives one sample
// of one index, then the `used` declarations that most statements use, each name once and in that order.
//
// The whole draw is one read-only transaction that names the index's schema first: the shared server pools
// transactions on every route, so a startup parameter would be refused, and the seed of setseed, which is a
// session's, would reach the random() of another backend.
func draw(ctx context.Context, dsn, schema string, seed float64, random, used int) ([]string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	delete(cfg.RuntimeParams, "search_path")
	if cfg.DefaultQueryExecMode == pgx.QueryExecModeCacheStatement {
		cfg.DefaultQueryExecMode = pgx.QueryExecModeExec
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = "+pgx.Identifier{schema}.Sanitize()); err != nil {
		return nil, err
	}

	var names []string
	seen := map[string]bool{}
	add := func(rows pgx.Rows, err error) error {
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
		return rows.Err()
	}

	if random > 0 {
		if _, err := tx.Exec(ctx, "SELECT setseed($1)", seed); err != nil {
			return nil, err
		}
		drawn := len(names)
		if err := add(tx.Query(ctx,
			`SELECT name FROM (SELECT DISTINCT name FROM decls) d ORDER BY random() LIMIT $1`, random)); err != nil {
			return nil, err
		}
		// One seed, one index, one sample: the draw is sorted so that the file does not depend on the order the
		// server happened to return the rows in.
		sort.Strings(names[drawn:])
	}
	if used > 0 {
		// edges.owner is the declaration that a used constant belongs to, and edges.stmt marks a use in the
		// user's statement: these are the declarations that most statements use. A dependency's constant
		// (reference_roots) is its own owner, of a module the index did not read: declindex declares no such name.
		if err := add(tx.Query(ctx,
			`SELECT e.owner FROM edges e WHERE e.stmt AND e.owner <> ''
			   AND EXISTS (SELECT 1 FROM modules m WHERE m.module = e.owner_module)
			 GROUP BY e.owner ORDER BY count(*) DESC, e.owner LIMIT $1`,
			used)); err != nil {
			return nil, err
		}
	}
	return names, nil
}
