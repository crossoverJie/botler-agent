// Command covercheck enforces the Go test-coverage floor, so a PR cannot quietly add untested
// code (the "add an untested file and the average stays put" trap).
//
// It reads a Go coverprofile produced exactly the way CI produces it:
//
//	go test -count=1 -covermode=atomic -coverpkg=./internal/... -coverprofile=coverage.out ./...
//
// and compares it against coverage.baseline.json. Two checks:
//
//   - total statements coverage must not fall below the baseline (minus a small tolerance)
//   - each package must not fall more than -pkg-tolerance points below its own baseline; with
//     -coverpkg a merged profile lists every block once per test binary, so the naive sum is
//     wrong and the blocks are merged by location here first
//
// Usage:
//
//	go run ./scripts/covercheck                    # check against coverage.baseline.json
//	go run ./scripts/covercheck -v                 # also list every package
//	go run ./scripts/covercheck -update            # rewrite the baseline from this profile
//	go run ./scripts/covercheck -min 78            # ad-hoc absolute floor, no baseline needed
//
// Regenerate the baseline from a CI run, not from a dev machine: a populated ~/.botler-agent (or a
// local .env) makes a few env-dependent tests take extra branches, so a local profile can be ~0.7pt
// higher than what CI measures. The floor must describe the lower of the two, or the gate flaps.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

type block struct {
	stmts int
	count int
}

type packageStat struct {
	Name  string  `json:"name"`
	Stmts int     `json:"stmts"`
	Hit   int     `json:"hit"`
	Pct   float64 `json:"pct"`
}

type baseline struct {
	// Total is the statements-weighted coverage of the whole denominator.
	Total float64 `json:"total"`
	// Packages is the per-package floor; a package missing here is not checked.
	Packages map[string]float64 `json:"packages"`
}

func main() {
	profile := flag.String("profile", "coverage.out", "coverprofile to read")
	baselinePath := flag.String("baseline", "coverage.baseline.json", "baseline file to compare against")
	min := flag.Float64("min", 0, "absolute total floor, overrides the baseline total when set")
	tolerance := flag.Float64("tolerance", 0.2, "allowed total drop in percentage points")
	pkgTolerance := flag.Float64("pkg-tolerance", 2.0, "allowed per-package drop in percentage points")
	pkgMinStmts := flag.Int("pkg-min-stmts", 50, "skip the per-package check for packages smaller than this")
	update := flag.Bool("update", false, "rewrite the baseline from this profile")
	verbose := flag.Bool("v", false, "list every package")
	flag.Parse()

	stats, total, totalStmts, err := summarise(*profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "covercheck: %v\n", err)
		os.Exit(2)
	}

	sort.Slice(stats, func(i, j int) bool {
		if stats[i].Pct != stats[j].Pct {
			return stats[i].Pct < stats[j].Pct
		}
		return stats[i].Name < stats[j].Name
	})

	base := readBaseline(*baselinePath)
	if *update {
		next := baseline{Total: round1(total), Packages: map[string]float64{}}
		for _, s := range stats {
			next.Packages[s.Name] = round1(s.Pct)
		}
		if err := writeBaseline(*baselinePath, next); err != nil {
			fmt.Fprintf(os.Stderr, "covercheck: %v\n", err)
			os.Exit(2)
		}
		fmt.Printf("covercheck: baseline written to %s (total %.1f%%, %d packages)\n",
			*baselinePath, next.Total, len(next.Packages))
		return
	}

	if !*verbose {
		// Default output: only what needs attention, so CI logs stay readable.
		for _, s := range stats {
			floor, ok := base.Packages[s.Name]
			if ok && base.Total > 0 && s.Pct >= floor-*pkgTolerance {
				continue
			}
			if !ok && s.Pct >= 90 {
				continue
			}
			fmt.Printf("  %-58s %5.1f%%  (%d/%d statements)\n", s.Name, s.Pct, s.Hit, s.Stmts)
		}
	} else {
		for _, s := range stats {
			fmt.Printf("  %-58s %5.1f%%  (%d/%d statements)\n", s.Name, s.Pct, s.Hit, s.Stmts)
		}
	}
	fmt.Printf("covercheck: total %.1f%% of %d statements\n", total, totalStmts)

	floor := base.Total - *tolerance
	source := "baseline"
	if *min > 0 {
		floor = *min
		source = "-min"
	}
	if base.Total == 0 && *min == 0 {
		fmt.Fprintln(os.Stderr, "covercheck: no baseline found and no -min set; nothing to enforce")
		fmt.Fprintf(os.Stderr, "covercheck: run `go run ./scripts/covercheck -update` to create %s\n", *baselinePath)
		os.Exit(2)
	}

	failed := false
	if total < floor {
		fmt.Fprintf(os.Stderr, "covercheck: FAIL total %.1f%% is below the %s floor %.1f%% (-%0.1f allowed)\n",
			total, source, floor, *tolerance)
		failed = true
	}
	for _, s := range stats {
		pkgFloor, ok := base.Packages[s.Name]
		if !ok || s.Stmts < *pkgMinStmts {
			continue
		}
		if s.Pct < pkgFloor-*pkgTolerance {
			fmt.Fprintf(os.Stderr, "covercheck: FAIL %s fell to %.1f%% (baseline %.1f%%, -%0.1f allowed)\n",
				s.Name, s.Pct, pkgFloor, *pkgTolerance)
			failed = true
		}
	}
	if failed {
		fmt.Fprintln(os.Stderr, "covercheck: either add tests, or (if the drop is intended) raise the floor with -update")
		os.Exit(1)
	}
	fmt.Printf("covercheck: ok (floor %.1f%% from %s)\n", floor, source)
}

// summarise parses the profile and aggregates it per package.
//
// A merged profile produced with -coverpkg repeats every block once per test binary, so blocks are
// keyed by location and their counts merged; summing the raw lines would count the same statement
// many times over.
func summarise(path string) ([]packageStat, float64, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	blocks := map[string]*block{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		// <file>:<startLine>.<startCol>,<endLine>.<endCol> <numStmts> <count>
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, 0, 0, fmt.Errorf("malformed profile line: %q", line)
		}
		stmts, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, 0, 0, fmt.Errorf("malformed statement count in %q", line)
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, 0, 0, fmt.Errorf("malformed hit count in %q", line)
		}
		key := fields[0]
		if existing, ok := blocks[key]; ok {
			// set mode: OR the hits; count/atomic: only "was it hit" matters for coverage.
			if count > existing.count {
				existing.count = count
			}
			continue
		}
		blocks[key] = &block{stmts: stmts, count: count}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, 0, fmt.Errorf("read %s: %w", path, err)
	}

	per := map[string]*packageStat{}
	var totalStmts, totalHit int
	for loc, b := range blocks {
		pkg := packageOf(loc)
		s, ok := per[pkg]
		if !ok {
			s = &packageStat{Name: pkg}
			per[pkg] = s
		}
		s.Stmts += b.stmts
		if b.count > 0 {
			s.Hit += b.stmts
			totalHit += b.stmts
		}
		totalStmts += b.stmts
	}

	stats := make([]packageStat, 0, len(per))
	for _, s := range per {
		if s.Stmts > 0 {
			s.Pct = round1(100 * float64(s.Hit) / float64(s.Stmts))
		}
		stats = append(stats, *s)
	}
	if totalStmts == 0 {
		return nil, 0, 0, fmt.Errorf("%s contains no statements", path)
	}
	return stats, 100 * float64(totalHit) / float64(totalStmts), totalStmts, nil
}

// packageOf maps a profile location ("<module>/internal/tools/run.go:61.41,63.2") to the import
// path of its package, so the report and the baseline are stable across machines.
func packageOf(loc string) string {
	file := loc
	if i := strings.LastIndex(loc, ":"); i > 0 {
		file = loc[:i]
	}
	if i := strings.LastIndex(file, "/"); i > 0 {
		return file[:i]
	}
	return file
}

func readBaseline(path string) baseline {
	b := baseline{Packages: map[string]float64{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return b
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		fmt.Fprintf(os.Stderr, "covercheck: %s is not valid JSON (%v); ignoring it\n", path, err)
		return baseline{Packages: map[string]float64{}}
	}
	if b.Packages == nil {
		b.Packages = map[string]float64{}
	}
	return b
}

func writeBaseline(path string, b baseline) error {
	body, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(path, body, 0o644)
}

func round1(v float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 1, 64), 64)
	return r
}
