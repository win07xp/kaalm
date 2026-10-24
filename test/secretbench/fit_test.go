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
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestLeastSquaresFitsALine(t *testing.T) {
	xs := []float64{0, 250, 500, 1000, 5000}
	var ys []float64
	for _, x := range xs {
		ys = append(ys, 3*x+7)
	}
	f, ok := leastSquares(xs, ys)
	if !ok {
		t.Fatal("fit failed")
	}
	if !near(f.Slope, 3) || !near(f.Intercept, 7) || !near(f.R2, 1) || f.Points != 5 {
		t.Fatalf("fit = %+v, want slope 3, intercept 7, R2 1, 5 points", f)
	}
}

func TestLeastSquaresPoolsRepeats(t *testing.T) {
	// Two repeats at each count: their means lie on y = 2x + 1.
	xs := []float64{0, 0, 10, 10, 20, 20}
	ys := []float64{0, 2, 20, 22, 40, 42}
	f, ok := leastSquares(xs, ys)
	if !ok {
		t.Fatal("fit failed")
	}
	if !near(f.Slope, 2) || !near(f.Intercept, 1) {
		t.Fatalf("fit = %+v, want slope 2, intercept 1", f)
	}
}

func TestLeastSquaresNeedsTwoDistinctCounts(t *testing.T) {
	if _, ok := leastSquares([]float64{500, 500, 500}, []float64{1, 2, 3}); ok {
		t.Fatal("fit with one distinct count succeeded")
	}
	if _, ok := leastSquares([]float64{1}, []float64{1}); ok {
		t.Fatal("fit with one point succeeded")
	}
}

func TestFitAllGroupsByVariantAndLayout(t *testing.T) {
	var samples []sample
	for _, n := range []int{0, 100, 200} {
		samples = append(samples,
			sample{Variant: "A", Layout: "one", Secrets: n, Requests: requestCounts{List: 1, Watch: 1}},
			sample{Variant: "today-gw", Layout: "one", Secrets: n, Requests: requestCounts{Get: int64(2 * n), Watch: int64(n)}},
		)
	}
	fits := fitAll(samples)
	if got := fits["today-gw/one"]["GET"].Slope; !near(got, 2) {
		t.Fatalf("today-gw GET slope = %v, want 2", got)
	}
	if got := fits["A/one"]["WATCH"].Slope; !near(got, 0) {
		t.Fatalf("A WATCH slope = %v, want 0", got)
	}
	if _, ok := fits["A/one"]["rss"]; ok {
		t.Fatal("rss fitted although no sample recorded it")
	}
}
