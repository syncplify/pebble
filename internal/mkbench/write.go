package main

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cockroachdb/errors/oserror"
	"github.com/spf13/cobra"
)

// A note to the reader on nomenclature used in this command.
//
// The write-throughput benchmark is generated by a roachtest with a number of
// independent worker VMs running the same benchmark (to allow for an average
// value to be recorded).
//
// An instance of the roachtest on a given day, for a given workload type (e.g.
// values of size 1024B, values of size 64B, etc.) is modelled as a `writeRun`.
// Each worker VM in a `writeRun` produces data modelled as a `rawWriteRun`.
// Each `rawWriteRun` contains the raw data points emitted periodically by the
// VM and are modelled as `writePoint`s.
//
// A `writeWorkload` (i.e. singular) models all data for a particular type of
// benchmark run (e.g. values of size 1024B), across all days. It is a mapping
// of day to `writeRun`, which is a collection of `rawWriteRun`s.
//
// The `writeWorkloads` (i.e. plural) is a mapping from workload name to its
// `writeWorkload`.
//
// The data can be thought of being modelled as follows:
//
//                                     `writeWorkloads`---------\
// - workload-name-A:                  `writeWorkload`-------\  |
//   - day-1:                          `writeRun`---------\  |  |
//      - VM-1:                        `rawWriteRun`----\ |  |  |
//        [ ... raw data point ... ]   `writePoint`     x |  |  |
//        ...                                             |  |  |
//      - VM-N:                                           |  |  |
//     	  [ ... raw data point ... ]                      x  |  |
//     ...                                                   |  |
//   - day-N:                                                |  |
//      - VM-1:                                              |  |
//     	  [ ... raw data point ... ]                         |  |
//        ...                                                |  |
//      - VM-N:                                              |  |
//     	  [ ... raw data point ... ]                         x  |
//   ...                                                        |
// - workload-name-Z:                                           |
//   - day-1:                                                   |
//      - VM-1:                                                 |
//     	  [ ... raw data point ... ]                            |
//        ...                                                   |
//      - VM-N:                                                 |
//     	  [ ... raw data point ... ]                            |
//     ...                                                      |
//   - day-N:                                                   |
//      - VM-1:                                                 |
//     	  [ ... raw data point ... ]                            |
//        ...                                                   |
//      - VM-N:                                                 |
//     	  [ ... raw data point ... ]                            x

const (
	// summaryFilename is the filename for the top-level summary output.
	summaryFilename = "summary.json"

	// rawRunFmt is the format string for raw benchmark data.
	rawRunFmt = "BenchmarkRaw%s %d ops/sec %v pass %s elapsed %d bytes %d levels %f writeAmp"
)

func getWriteCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "write",
		Short: "parse write throughput benchmark data",
		Long: `
Parses write-throughput benchmark data into two sets of JSON "summary" files:

1. A top-level summary.json file. Data in this file is reported per-day, per
workload (i.e. values=1024, etc.), and is responsible for the top-level
write-throughput visualizations on the Pebble benchmarks page.

Each data-point for a time-series contains an ops/sec figure (measured as a
simple average over all data points for that workload run), and a relative path
to a per-run summary JSON file, containing the raw data for the run.

2. A per-run *-summary.json file. Data in this file contains the raw data for
each of the benchmark instances participating in the workload run on the given
day. Each key in the file is the relative path to the original raw data file.
Each data point contains the calculated optimal ops/sec for the instance of the
run (see split.go for more detail on the algorithm), in addition to the raw data
in CSV format.

This command can be run without flags at the root of the directory containing
the raw data. By default the raw data will be pulled from "data", and the
resulting top-level and per-run summary files are written to "write-throughput".
Both locations can be overridden with the --data-dir and --summary-dir flags,
respectively.
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir, err := cmd.Flags().GetString("data-dir")
			if err != nil {
				return err
			}

			summaryDir, err := cmd.Flags().GetString("summary-dir")
			if err != nil {
				return err
			}

			return parseWrite(dataDir, summaryDir)
		},
	}

	c.Flags().String("data-dir", "data", "path to the raw data directory")
	c.Flags().String("summary-dir", "write-throughput", "output directory containing the summary files")
	c.SilenceUsage = true

	return c
}

// writePoint is a raw datapoint from an individual write-throughput benchmark
// run.
type writePoint struct {
	elapsedSecs int
	opsSec      int
	passed      bool
	size        uint64
	levels      int
	writeAmp    float64
}

// formatCSV returns a comma-separated string representation of the datapoint.
func (p writePoint) formatCSV() string {
	return fmt.Sprintf(
		"%d,%d,%v,%d,%d,%.2f",
		p.elapsedSecs, p.opsSec, p.passed, p.size, p.levels, p.writeAmp)
}

// rawWriteRun is a collection of datapoints from a single instance of a
// benchmark run (i.e. datapoints comprising a single roachtest instance of a
// write-throughput benchmark).
type rawWriteRun struct {
	points []writePoint
	split  int // memoized
}

// opsPerSecSplit returns an optimal-split point that divides the passes and
// fails from the datapoints in a rawWriteRun.
func (r *rawWriteRun) opsPerSecSplit() int {
	if r.split > 0 {
		return r.split
	}

	// Pre-process by partitioning the datapoint into passes and fails.
	var passes, fails []int
	for _, p := range r.points {
		if p.passed {
			passes = append(passes, p.opsSec)
		} else {
			fails = append(fails, p.opsSec)
		}
	}

	// Compute and cache the split point as we only need to calculate it once.
	split := findOptimalSplit(passes, fails)
	r.split = split

	return split
}

// writeAmp returns the value of the write-amplification at the end of the run.
func (r *rawWriteRun) writeAmp() float64 {
	return r.points[len(r.points)-1].writeAmp
}

// formatCSV returns a comma-separated string representation of the rawWriteRun.
// The value itself is a newline-delimited string value comprised of the CSV
// representation of the individual writePoints.
func (r rawWriteRun) formatCSV() string {
	var b bytes.Buffer
	for _, p := range r.points {
		_, _ = fmt.Fprintf(&b, "%s\n", p.formatCSV())
	}
	return b.String()
}

// writeRunSummary represents a single summary datapoint across all rawWriteRuns
// that comprise a writeRun. The datapoint contains a summary ops-per-second
// value, in addition to a path to the summary.json file with the combined data
// for the run.
type writeRunSummary struct {
	Name        string  `json:"name"`
	Date        string  `json:"date"`
	OpsSec      int     `json:"opsSec"`
	WriteAmp    float64 `json:"writeAmp"`
	SummaryPath string  `json:"summaryPath"`
}

// writeWorkloadSummary is an alias for a slice of writeRunSummaries.
type writeWorkloadSummary []writeRunSummary

// writeRun is a collection of one or more rawWriteRuns (i.e. the union of all
// rawWriteRuns from each worker participating in the roachtest cluster used for
// running the write-throughput benchmarks).
type writeRun struct {
	// name is the benchmark workload name (i.e. "values=1024").
	name string

	// date is the date on which the writeRun took place.
	date string

	// dir is path to the directory containing the raw data. The path is
	// relative to the data-dir.
	dir string

	// rawRuns is a map from input data filename to its rawWriteRun data.
	rawRuns map[string]rawWriteRun
}

// summaryFilename returns the filename to be used for storing the summary
// output for the writeRun. The filename preserves the original data source path
// for ease of debugging / data-provenance.
func (r writeRun) summaryFilename() string {
	parts := strings.Split(r.dir, string(os.PathSeparator))
	parts = append(parts, summaryFilename)
	return strings.Join(parts, "-")
}

// summarize computes a writeRunSummary datapoint for the writeRun.
func (r writeRun) summarize() writeRunSummary {
	var (
		sumOpsSec   int
		sumWriteAmp float64
	)
	for _, rr := range r.rawRuns {
		sumOpsSec += rr.opsPerSecSplit()
		sumWriteAmp += rr.writeAmp()
	}
	l := len(r.rawRuns)

	return writeRunSummary{
		Name:        r.name,
		Date:        r.date,
		SummaryPath: r.summaryFilename(),
		// Calculate an average across all raw runs in this run.
		// TODO(travers): test how this works in practice, after we have
		// gathered enough data.
		OpsSec:   sumOpsSec / l,
		WriteAmp: math.Round(100*sumWriteAmp/float64(l)) / 100, // round to 2dp.
	}
}

// cookedWriteRun is a representation of a previously parsed (or "cooked")
// writeRun.
type cookedWriteRun struct {
	OpsSec int    `json:"opsSec"`
	Raw    string `json:"rawData"`
}

// formatSummaryJSON returns a JSON representation of the combined raw data from
// all rawWriteRuns that comprise the writeRun. It has the form:
//   {
//     "original-raw-write-run-log-file-1.gz": {
//       "opsSec": ...,
//       "raw": ...,
//     },
//      ...
//     "original-raw-write-run-log-file-N.gz": {
//       "opsSec": ...,
//       "raw": ...,
//     },
//   }
func (r writeRun) formatSummaryJSON() ([]byte, error) {
	m := make(map[string]cookedWriteRun)
	for name, data := range r.rawRuns {
		m[name] = cookedWriteRun{
			OpsSec: data.opsPerSecSplit(),
			Raw:    data.formatCSV(),
		}
	}
	return prettyJSON(&m), nil
}

// write workload is a map from "day" to corresponding writeRun, for a given
// write-throughput benchmark workload (i.e. values=1024).
type writeWorkload struct {
	days map[string]*writeRun // map from day to runs for the given workload
}

// writeWorkloads is an alias for a map from workload name to its corresponding
// map from day to writeRun.
type writeWorkloads map[string]*writeWorkload

// nameDay is a (name, day) tuple, used as a map key.
type nameDay struct {
	name, day string
}

type writeLoader struct {
	// rootDir is the path to the root directory containing the data.
	dataDir string

	// summaryFilename is the name of the file containing the summary data.
	summaryDir string

	// workloads is a map from workload name to its corresponding data.
	workloads writeWorkloads

	// cooked is a "set" of (workload, day) tuples representing whether
	// previously parsed data was present for the (workload, day).
	cooked map[nameDay]bool

	// cookedSummaries is a map from workload name to previously generated data
	// for the workload. This data is "mixed-in" with new data when the summary
	// files are written out.
	cookedSummaries map[string]writeWorkloadSummary
}

// newWriteLoader returns a new writeLoader that can be used to generate the
// summary files for write-throughput benchmarking data.
func newWriteLoader(dataDir, summaryDir string) *writeLoader {
	return &writeLoader{
		dataDir:         dataDir,
		summaryDir:      summaryDir,
		workloads:       make(writeWorkloads),
		cooked:          make(map[nameDay]bool),
		cookedSummaries: make(map[string]writeWorkloadSummary),
	}
}

// loadCooked loads previously summarized write throughput benchmark data.
func (l *writeLoader) loadCooked() error {
	b, err := os.ReadFile(filepath.Join(l.summaryDir, summaryFilename))
	if err != nil {
		// The first ever run will not find the summary file. Return early in
		// this case, and we'll start afresh.
		if oserror.IsNotExist(err) {
			return nil
		}
		return err
	}

	// Reconstruct the summary.
	summaries := make(map[string]writeWorkloadSummary)
	err = json.Unmarshal(b, &summaries)
	if err != nil {
		return err
	}

	// Populate the cooked map.
	l.cookedSummaries = summaries

	// Populate the set used for determining whether we can skip a raw file.
	for name, workloadSummary := range summaries {
		for _, runSummary := range workloadSummary {
			l.cooked[nameDay{name, runSummary.Date}] = true
		}
	}

	return nil
}

// loadRaw loads the raw data from the root data directory.
func (l *writeLoader) loadRaw() error {
	walkFn := func(path, pathRel string, info os.FileInfo) error {
		// The relative directory structure is of the form:
		//   $day/pebble/write/$name/$run/$file
		parts := strings.Split(pathRel, string(os.PathSeparator))
		if len(parts) < 6 {
			return nil // stumble forward on invalid paths
		}

		// Filter out files that aren't in write benchmark directories.
		if parts[2] != "write" {
			return nil
		}
		day := parts[0]

		f, err := os.Open(path)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "%+v\n", err)
			return nil // stumble forward on error
		}
		defer func() { _ = f.Close() }()

		rd := io.Reader(f)
		if strings.HasSuffix(path, ".bz2") {
			rd = bzip2.NewReader(f)
		} else if strings.HasSuffix(path, ".gz") {
			var err error
			rd, err = gzip.NewReader(f)
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "%+v\n", err)
				return nil // stumble forward on error
			}
		}

		// Parse the data for this file and add to the appropriate workload.
		s := bufio.NewScanner(rd)
		r := rawWriteRun{}
		var name string
		for s.Scan() {
			line := s.Text()
			if !strings.HasPrefix(line, "BenchmarkRaw") {
				continue
			}

			var p writePoint
			var nameInner, elapsed string
			n, err := fmt.Sscanf(line, rawRunFmt,
				&nameInner, &p.opsSec, &p.passed, &elapsed, &p.size, &p.levels, &p.writeAmp)
			if err != nil || n != 7 {
				// Stumble forward on error.
				_, _ = fmt.Fprintf(os.Stderr, "%s: %v\n", s.Text(), err)
				continue
			}

			// The first datapoint we see in the file is assumed to be the same
			// for all datapoints.
			if name == "" {
				name = nameInner

				// Skip files for (workload, day) pairs that have been parsed
				// previously. Note that this relies on loadCooked having been
				// called previously to seed the map with cooked data.
				if ok := l.cooked[nameDay{name, day}]; ok {
					_, _ = fmt.Fprintf(os.Stderr,
						"skipping previously cooked data in file %s (workload=%q, day=%q)\n",
						pathRel, name, day)
					return nil
				}
			} else if name != nameInner {
				_, _ = fmt.Fprintf(os.Stderr,
					"WARN: benchmark name %q differs from previously seen name %q: %s",
					nameInner, name, s.Text())
			}

			// Convert the elapsed time into seconds.
			secs, err := time.ParseDuration(elapsed)
			if err != nil {
				// Stumble forward on error.
				_, _ = fmt.Fprintf(os.Stderr, "%s: %v\n", s.Text(), err)
				continue
			}
			p.elapsedSecs = int(secs.Seconds())

			// Add this data point to the collection of points for this run.
			r.points = append(r.points, p)
		}

		// Add the raw run to the map.
		l.addRawRun(name, day, pathRel, r)

		return nil
	}
	return walkDir(l.dataDir, walkFn)
}

// addRawRun adds a rawWriteRun to the corresponding datastructures by looking
// up the workload name (i.e. "values=1024"), then appending the rawWriteRun to
// the corresponding slice of all rawWriteRuns.
func (l *writeLoader) addRawRun(name, day, path string, raw rawWriteRun) {
	// Skip files with no points (i.e. files that couldn't be parsed).
	if len(raw.points) == 0 {
		return
	}

	_, _ = fmt.Fprintf(
		os.Stderr, "adding raw run: (workload=%q, day=%q); nPoints=%d; file=%s\n",
		name, day, len(raw.points), path)

	w := l.workloads[name]
	if w == nil {
		w = &writeWorkload{days: make(map[string]*writeRun)}
		l.workloads[name] = w
	}

	r := w.days[day]
	if r == nil {
		r = &writeRun{
			name:    name,
			date:    day,
			dir:     filepath.Dir(path),
			rawRuns: make(map[string]rawWriteRun),
		}
		w.days[day] = r
	}
	r.rawRuns[path] = raw
}

// cookSummary writes out the data in the loader to the summary file (new or
// existing).
func (l *writeLoader) cookSummary() error {
	summary := make(map[string]writeWorkloadSummary)
	for name, w := range l.workloads {
		summary[name] = cookWriteSummary(w)
	}

	// Mix in the previously cooked values.
	for name, cooked := range l.cookedSummaries {
		existing, ok := summary[name]
		if !ok {
			summary[name] = cooked
		} else {
			// We must merge and re-sort by date.
			existing = append(existing, cooked...)
			sort.Slice(existing, func(i, j int) bool {
				return existing[i].Date < existing[j].Date
			})
			summary[name] = existing
		}
	}
	b := prettyJSON(&summary)
	b = append(b, '\n')

	outputPath := filepath.Join(l.summaryDir, summaryFilename)
	err := os.WriteFile(outputPath, b, 0644)
	if err != nil {
		return err
	}

	return nil
}

// cookWriteSummary is a helper that generates the summary for a write workload
// by computing the per-day summaries across all runs.
func cookWriteSummary(w *writeWorkload) writeWorkloadSummary {
	days := make([]string, 0, len(w.days))
	for day := range w.days {
		days = append(days, day)
	}
	sort.Strings(days)

	var summary writeWorkloadSummary
	for _, day := range days {
		r := w.days[day]
		summary = append(summary, r.summarize())
	}

	return summary
}

// cookWriteRunSummaries writes out the per-run summary files.
func (l *writeLoader) cookWriteRunSummaries() error {
	for _, w := range l.workloads {
		for _, r := range w.days {
			// Write out files preserving the original directory structure for
			// ease of understanding / debugging.
			outputPath := filepath.Join(l.summaryDir, r.summaryFilename())
			if err := outputWriteRunSummary(r, outputPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// outputWriteRunSummary is a helper that generates the summary JSON for the
// writeRun and writes it to the given output path.
func outputWriteRunSummary(r *writeRun, outputPath string) error {
	f, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	b, err := r.formatSummaryJSON()
	if err != nil {
		return err
	}
	b = append(b, '\n')

	_, err = f.Write(b)
	return err
}

// parseWrite parses the raw write-throughput benchmark data and writes out the
// summary files.
func parseWrite(dataDir, summaryDir string) error {
	l := newWriteLoader(dataDir, summaryDir)
	if err := l.loadCooked(); err != nil {
		return err
	}

	if err := l.loadRaw(); err != nil {
		return err
	}

	if err := l.cookSummary(); err != nil {
		return err
	}

	return l.cookWriteRunSummaries()
}
