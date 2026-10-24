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

// fit is a least-squares line y = Slope*x + Intercept over Points points.
// With x the Secret count, Slope is the figure's cost per Secret.
type fit struct {
	Slope     float64 `json:"slope"`
	Intercept float64 `json:"intercept"`
	R2        float64 `json:"r2"`
	Points    int     `json:"points"`
}

// leastSquares fits a line through every (x, y) point; repeats at one x
// pool, which gives the same slope as fitting their mean. ok is false with
// fewer than two distinct x values.
func leastSquares(xs, ys []float64) (fit, bool) {
	n := len(xs)
	if n != len(ys) || n < 2 {
		return fit{}, false
	}
	var sx, sy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
	}
	mx, my := sx/float64(n), sy/float64(n)
	var sxx, sxy, syy float64
	for i := range xs {
		dx, dy := xs[i]-mx, ys[i]-my
		sxx += dx * dx
		sxy += dx * dy
		syy += dy * dy
	}
	if sxx == 0 {
		return fit{}, false
	}
	slope := sxy / sxx
	r2 := 1.0
	if syy > 0 {
		r2 = sxy * sxy / (sxx * syy)
	}
	return fit{Slope: slope, Intercept: my - slope*mx, R2: r2, Points: n}, true
}

// figure is one recorded value the table fits against the Secret count.
// Value returns false when a sample lacks it.
type figure struct {
	Name  string
	Unit  string
	Value func(s sample) (float64, bool)
}

// unitKiB is the unit of every memory figure.
const unitKiB = "KiB"

func always(v float64) (float64, bool) { return v, true }

// kib converts bytes to KiB; a figure of 0 bytes was not recorded.
func kib(v int64) (float64, bool) { return float64(v) / 1024, v > 0 }

func figures() []figure {
	return []figure{
		{"heap", unitKiB, func(s sample) (float64, bool) { return kib(s.HeapInuseBytes) }},
		{"rss", unitKiB, func(s sample) (float64, bool) { return kib(s.RSSBytes) }},
		{"goroutines", "", func(s sample) (float64, bool) { return always(float64(s.Goroutines - s.GoroutinesBefore)) }},
		{"GET", "", func(s sample) (float64, bool) { return always(float64(s.Requests.Get)) }},
		{"LIST", "", func(s sample) (float64, bool) { return always(float64(s.Requests.List)) }},
		{"WATCH", "", func(s sample) (float64, bool) { return always(float64(s.Requests.Watch)) }},
		{"429", "", func(s sample) (float64, bool) { return always(float64(s.Requests.Throttled)) }},
		{"served", "ms", func(s sample) (float64, bool) { return always(s.ServedSec * 1000) }},
		{"settled", "ms", func(s sample) (float64, bool) { return s.SettledSec * 1000, s.Settled }},
		{"updateP50", "ms", func(s sample) (float64, bool) { return s.UpdateP50Ms, s.Secrets > 0 && s.UpdateMissed == 0 }},
		{"apiWatches", "", func(s sample) (float64, bool) { return always(s.APIServerSecretWatches) }},
		{"apiserverRSS", unitKiB, func(s sample) (float64, bool) { return kib(s.APIServerRSSBytes) }},
		{"etcdRSS", unitKiB, func(s sample) (float64, bool) { return kib(s.EtcdRSSBytes) }},
	}
}

// seriesKey groups samples by variant and layout.
func seriesKey(s sample) string { return s.Variant + "/" + s.Layout }

// fitAll fits every figure against the Secret count for each variant and
// layout, pooling every repeat.
func fitAll(samples []sample) map[string]map[string]fit {
	type points struct{ xs, ys []float64 }
	series := map[string]map[string]*points{}
	for _, s := range samples {
		key := seriesKey(s)
		if series[key] == nil {
			series[key] = map[string]*points{}
		}
		for _, f := range figures() {
			v, ok := f.Value(s)
			if !ok {
				continue
			}
			p := series[key][f.Name]
			if p == nil {
				p = &points{}
				series[key][f.Name] = p
			}
			p.xs = append(p.xs, float64(s.Secrets))
			p.ys = append(p.ys, v)
		}
	}
	out := map[string]map[string]fit{}
	for key, byFigure := range series {
		for name, p := range byFigure {
			if f, ok := leastSquares(p.xs, p.ys); ok {
				if out[key] == nil {
					out[key] = map[string]fit{}
				}
				out[key][name] = f
			}
		}
	}
	return out
}
