//go:build secretbench

/*
Copyright 2026 The Kaalm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// summary is one run's output: its settings, every sample, and the slope
// fits over them, keyed by layout.
type summary struct {
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	Config     config    `json:"config"`
	Kubernetes string    `json:"kubernetes"`
	// BaselineSecretWatches is the API server's open Secret watches before
	// any Watcher ran, envtest's own; sampled watch counts subtract it.
	BaselineSecretWatches float64                   `json:"baselineSecretWatches"`
	Samples               []sample                  `json:"samples"`
	Fits                  map[string]map[string]fit `json:"fits"`
	Notes                 []string                  `json:"notes,omitempty"`
}

// write refits and writes the summary to path through a temporary file, so
// an interrupted run never leaves a half-written file.
func (s *summary) write(path string) error {
	s.Fits = fitAll(s.Samples)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// printTable prints two tables with one row per layout: the slope per
// Secret of every figure, then the mean of every figure at the largest
// Secret count.
func printTable(w io.Writer, s *summary) {
	fits := fitAll(s.Samples)
	figs := figures()
	header := []string{"layout"}
	for _, f := range figs {
		h := f.Name
		if f.Unit != "" {
			h += " (" + f.Unit + ")"
		}
		header = append(header, h)
	}
	largest := 0
	for _, smp := range s.Samples {
		largest = max(largest, smp.Secrets)
	}
	var layouts []string
	for _, layout := range s.Config.Layouts {
		if slices.ContainsFunc(s.Samples, func(smp sample) bool { return smp.Layout == layout }) {
			layouts = append(layouts, layout)
		}
	}
	if len(layouts) == 0 {
		return
	}

	_, _ = fmt.Fprintf(w, "\nslope per Secret (least squares over every count and repeat)\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t")+"\t")
	for _, layout := range layouts {
		row := []string{layout}
		for _, f := range figs {
			ft, ok := fits[layout][f.Name]
			if !ok {
				row = append(row, "-")
				continue
			}
			row = append(row, fmt.Sprintf("%.4g", ft.Slope))
		}
		_, _ = fmt.Fprintln(tw, strings.Join(row, "\t")+"\t")
	}
	_ = tw.Flush()

	_, _ = fmt.Fprintf(w, "\nmean at %d Secrets\n", largest)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t")+"\t")
	for _, layout := range layouts {
		row := []string{layout}
		for _, f := range figs {
			var sum float64
			var n int
			for _, smp := range s.Samples {
				if smp.Layout != layout || smp.Secrets != largest {
					continue
				}
				if val, ok := f.Value(smp); ok {
					sum += val
					n++
				}
			}
			if n == 0 {
				row = append(row, "-")
				continue
			}
			row = append(row, fmt.Sprintf("%.4g", sum/float64(n)))
		}
		_, _ = fmt.Fprintln(tw, strings.Join(row, "\t")+"\t")
	}
	_ = tw.Flush()
}
