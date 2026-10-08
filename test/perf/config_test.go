//go:build perftest

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
	"reflect"
	"strings"
	"testing"
)

func TestDefaultPhases(t *testing.T) {
	c, err := parseRunFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gateway", "ramp", "hold", "restart", "teardown", "churn", "tasks", "tools", "stream"}
	if !reflect.DeepEqual(c.Phases, want) {
		t.Errorf("default phases = %v, want %v", c.Phases, want)
	}
}

func TestPhasesRunInTheOrderGiven(t *testing.T) {
	c, err := parseRunFlags([]string{"-phases", "tools, gateway"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"tools", "gateway"}; !reflect.DeepEqual(c.Phases, want) {
		t.Errorf("phases = %v, want %v", c.Phases, want)
	}
}

func TestUnknownPhaseListsTheKnownOnes(t *testing.T) {
	_, err := parseRunFlags([]string{"-phases", "gateway,nope"})
	if err == nil || !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), "tools") {
		t.Errorf("err = %v", err)
	}
}

func TestOptInPhasesAreAccepted(t *testing.T) {
	c, err := parseRunFlags([]string{"-phases", "namespaces"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"namespaces"}; !reflect.DeepEqual(c.Phases, want) {
		t.Errorf("phases = %v, want %v", c.Phases, want)
	}
	_, err = parseRunFlags([]string{"-phases", "nope"})
	if err == nil || !strings.Contains(err.Error(), "namespaces") {
		t.Errorf("the unknown-phase error lists the opt-in phases too: %v", err)
	}
}

func TestSpreadFlagsMustBePositive(t *testing.T) {
	for _, args := range [][]string{
		{"-namespaces-count", "0"},
		{"-namespaces-agents", "0"},
		{"-namespaces-count", "-3"},
	} {
		if _, err := parseRunFlags(args); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
	c, err := parseRunFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.NamespacesCount != 20 || c.NamespacesAgents != 400 {
		t.Errorf("defaults: %d namespaces, %d agents", c.NamespacesCount, c.NamespacesAgents)
	}
}
