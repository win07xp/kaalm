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

package chart

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// rbacRender is the RBAC part of a full chart render.
type rbacRender struct {
	clusterRoles        map[string]rbacv1.ClusterRole        // by name
	clusterRoleBindings map[string]rbacv1.ClusterRoleBinding // by name
	roleBindings        map[string]rbacv1.RoleBinding        // by namespace/name
}

var personaRoles = []string{"kaalm-platform-admin", "kaalm-catalog-reader", "kaalm-developer", "kaalm-secrets-admin"}

var aggregateRoles = []string{"kaalm-aggregate-to-view", "kaalm-aggregate-to-edit"}

var catalogKinds = []string{"agentclasses", "modelproviders", "toolproviders"}

const aggregateLabelPrefix = "rbac.authorization.k8s.io/aggregate-to-"

// runFullTemplate renders the whole chart. It uses no -s flag, because helm
// fails with -s when the named template renders nothing.
func runFullTemplate(args ...string) ([]byte, error) {
	full := append([]string{
		"template", "kaalm", filepath.Join("..", "..", "charts", "kaalm"), "-n", releaseNamespace,
	}, args...)
	return exec.Command("helm", full...).CombinedOutput()
}

// renderRBAC renders the full chart and returns its RBAC objects.
func renderRBAC(t *testing.T, args ...string) rbacRender {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	out, err := runFullTemplate(args...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	r := rbacRender{
		clusterRoles:        map[string]rbacv1.ClusterRole{},
		clusterRoleBindings: map[string]rbacv1.ClusterRoleBinding{},
		roleBindings:        map[string]rbacv1.RoleBinding{},
	}
	for _, doc := range strings.Split(string(out), "\n---\n") {
		var head struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil || head.APIVersion != "rbac.authorization.k8s.io/v1" {
			continue
		}
		switch head.Kind {
		case "ClusterRole":
			var o rbacv1.ClusterRole
			strict(t, doc, &o)
			r.clusterRoles[o.Name] = o
		case "ClusterRoleBinding":
			var o rbacv1.ClusterRoleBinding
			strict(t, doc, &o)
			r.clusterRoleBindings[o.Name] = o
		case "RoleBinding":
			var o rbacv1.RoleBinding
			strict(t, doc, &o)
			r.roleBindings[o.Namespace+"/"+o.Name] = o
		}
	}
	return r
}

func strict(t *testing.T, doc string, into any) {
	t.Helper()
	if err := yaml.UnmarshalStrict([]byte(doc), into); err != nil {
		t.Fatalf("decode: %v\n%s", err, doc)
	}
}

// ruleSet flattens rules to sorted "group|resource|verbs" strings.
func ruleSet(rules []rbacv1.PolicyRule) []string {
	var out []string
	for _, r := range rules {
		verbs := slices.Clone(r.Verbs)
		sort.Strings(verbs)
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				out = append(out, fmt.Sprintf("%s|%s|%s", g, res, strings.Join(verbs, ",")))
			}
		}
	}
	sort.Strings(out)
	return out
}

func rulesFor(group string, resources []string, verbs ...string) []string {
	v := slices.Clone(verbs)
	sort.Strings(v)
	var out []string
	for _, res := range resources {
		out = append(out, fmt.Sprintf("%s|%s|%s", group, res, strings.Join(v, ",")))
	}
	return out
}

func sorted(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	sort.Strings(out)
	return out
}

func namesResource(rules []rbacv1.PolicyRule, pred func(string) bool) bool {
	for _, r := range rules {
		if slices.ContainsFunc(r.Resources, pred) {
			return true
		}
	}
	return false
}

func hasAggregateLabel(labels map[string]string) bool {
	for k := range labels {
		if strings.HasPrefix(k, aggregateLabelPrefix) {
			return true
		}
	}
	return false
}

var (
	namespacedKinds = []string{"agents", "agenttasks", "agentchannels"}
	readVerbs       = []string{"get", "list", "watch"}
)

func wantNoPersonaOrAggregateObjects(t *testing.T, r rbacRender, names []string) {
	t.Helper()
	for _, name := range names {
		if _, ok := r.clusterRoles[name]; ok {
			t.Errorf("ClusterRole %s rendered, want none", name)
		}
		if _, ok := r.clusterRoleBindings[name]; ok {
			t.Errorf("ClusterRoleBinding %s rendered, want none", name)
		}
		for key := range r.roleBindings {
			if strings.HasSuffix(key, "/"+name) {
				t.Errorf("RoleBinding %s rendered, want none", key)
			}
		}
	}
}

// A default install renders no persona or aggregate object: the RBAC set is
// the controller's and the gateway's only.
func TestPersonas_DefaultRendersNone(t *testing.T) {
	r := renderRBAC(t)
	var names []string
	for name := range r.clusterRoles {
		names = append(names, name)
	}
	sort.Strings(names)
	if want := []string{"kaalm-controller", "kaalm-gateway"}; !reflect.DeepEqual(names, want) {
		t.Errorf("ClusterRoles = %v, want %v", names, want)
	}
	wantNoPersonaOrAggregateObjects(t, r, append(slices.Clone(personaRoles), aggregateRoles...))
}

// Enabling personas with no subjects renders the four roles and no binding.
func TestPersonas_EnabledRendersFourRolesAndNoBindings(t *testing.T) {
	r := renderRBAC(t, "--set", "rbac.personas.enabled=true")
	for _, name := range personaRoles {
		if _, ok := r.clusterRoles[name]; !ok {
			t.Errorf("ClusterRole %s missing", name)
		}
		if _, ok := r.clusterRoleBindings[name]; ok {
			t.Errorf("ClusterRoleBinding %s rendered with no subjects", name)
		}
	}
	for key, rb := range r.roleBindings {
		if slices.Contains(personaRoles, rb.RoleRef.Name) {
			t.Errorf("RoleBinding %s rendered with no subjects", key)
		}
	}
	wantNoPersonaOrAggregateObjects(t, r, aggregateRoles)
}

// Each persona role grants exactly the rules in docs/src/security/rbac.md.
func TestPersonas_RoleRules(t *testing.T) {
	r := renderRBAC(t, "--set", "rbac.personas.enabled=true")
	want := map[string][]string{
		"kaalm-platform-admin": sorted(
			rulesFor("kaalm.io", catalogKinds, "*"),
			rulesFor("kaalm.io", namespacedKinds, readVerbs...),
		),
		"kaalm-catalog-reader": sorted(rulesFor("kaalm.io", catalogKinds, readVerbs...)),
		"kaalm-developer": sorted(
			rulesFor("kaalm.io", namespacedKinds, "*"),
			rulesFor("", []string{"pods", "persistentvolumeclaims", "services", "configmaps", "events"}, readVerbs...),
			rulesFor("", []string{"pods/log"}, "get"),
		),
		"kaalm-secrets-admin": sorted(rulesFor("", []string{"secrets"},
			"get", "list", "watch", "create", "update", "patch", "delete")),
	}
	for name, w := range want {
		cr, ok := r.clusterRoles[name]
		if !ok {
			t.Errorf("ClusterRole %s missing", name)
			continue
		}
		if got := ruleSet(cr.Rules); !reflect.DeepEqual(got, w) {
			t.Errorf("%s rules =\n  %v\nwant\n  %v", name, got, w)
		}
		if name != "kaalm-secrets-admin" && namesResource(cr.Rules, func(s string) bool { return s == "secrets" }) {
			t.Errorf("%s names secrets", name)
		}
		if namesResource(cr.Rules, func(s string) bool { return strings.HasSuffix(s, "/status") }) {
			t.Errorf("%s names a status subresource", name)
		}
		if hasAggregateLabel(cr.Labels) {
			t.Errorf("%s carries an aggregate-to label: %v", name, cr.Labels)
		}
		if cr.Labels["app.kubernetes.io/name"] != "kaalm" {
			t.Errorf("%s labels = %v, want the kaalm labels", name, cr.Labels)
		}
	}
	if dev, ok := r.clusterRoles["kaalm-developer"]; ok {
		if namesResource(dev.Rules, func(s string) bool { return slices.Contains(catalogKinds, s) }) {
			t.Error("kaalm-developer names a catalog kind")
		}
		if namesResource(dev.Rules, func(s string) bool { return s == "pods/exec" }) {
			t.Error("kaalm-developer grants pods/exec by default")
		}
	}
}

// developerExec adds pods/exec get and create to kaalm-developer only.
func TestPersonas_DeveloperExec(t *testing.T) {
	base := renderRBAC(t, "--set", "rbac.personas.enabled=true")
	r := renderRBAC(t, "--set", "rbac.personas.enabled=true", "--set", "rbac.personas.developerExec=true")
	execRule := rulesFor("", []string{"pods/exec"}, "get", "create")
	want := sorted(ruleSet(base.clusterRoles["kaalm-developer"].Rules), execRule)
	if got := ruleSet(r.clusterRoles["kaalm-developer"].Rules); !reflect.DeepEqual(got, want) {
		t.Errorf("kaalm-developer rules with developerExec =\n  %v\nwant\n  %v", got, want)
	}
	for _, name := range personaRoles {
		if name == "kaalm-developer" {
			continue
		}
		if got, w := ruleSet(r.clusterRoles[name].Rules), ruleSet(base.clusterRoles[name].Rules); !reflect.DeepEqual(got, w) {
			t.Errorf("developerExec changed %s: %v, want %v", name, got, w)
		}
	}
}

// Bindings come from the subject lists in values; developers get one
// RoleBinding per namespace and secrets admins one in the release namespace.
func TestPersonas_BindingsFromValues(t *testing.T) {
	r := renderRBAC(t, valuesFile(t, `
rbac:
  personas:
    enabled: true
    platformAdmins:
      - kind: Group
        apiGroup: rbac.authorization.k8s.io
        name: platform-team
    catalogReaders:
      - kind: Group
        apiGroup: rbac.authorization.k8s.io
        name: devs
    secretsAdmins:
      - kind: User
        apiGroup: rbac.authorization.k8s.io
        name: alice
    developers:
      team-a:
        - kind: Group
          apiGroup: rbac.authorization.k8s.io
          name: team-a-devs
      team-b:
        - kind: ServiceAccount
          name: ci
          namespace: team-b
      team-c: []
`)...)
	group := func(name string) rbacv1.Subject {
		return rbacv1.Subject{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: name}
	}
	ref := func(name string) rbacv1.RoleRef {
		return rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: name}
	}
	for name, subjects := range map[string][]rbacv1.Subject{
		"kaalm-platform-admin": {group("platform-team")},
		"kaalm-catalog-reader": {group("devs")},
	} {
		crb, ok := r.clusterRoleBindings[name]
		if !ok {
			t.Errorf("ClusterRoleBinding %s missing", name)
			continue
		}
		if crb.RoleRef != ref(name) || !reflect.DeepEqual(crb.Subjects, subjects) {
			t.Errorf("ClusterRoleBinding %s = %v %v, want %v %v", name, crb.RoleRef, crb.Subjects, ref(name), subjects)
		}
	}
	for key, w := range map[string]struct {
		role     string
		subjects []rbacv1.Subject
	}{
		releaseNamespace + "/kaalm-secrets-admin": {
			"kaalm-secrets-admin",
			[]rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "alice"}},
		},
		"team-a/kaalm-developer": {"kaalm-developer", []rbacv1.Subject{group("team-a-devs")}},
		"team-b/kaalm-developer": {
			"kaalm-developer",
			[]rbacv1.Subject{{Kind: "ServiceAccount", Name: "ci", Namespace: "team-b"}},
		},
	} {
		rb, ok := r.roleBindings[key]
		if !ok {
			t.Errorf("RoleBinding %s missing", key)
			continue
		}
		if rb.RoleRef != ref(w.role) || !reflect.DeepEqual(rb.Subjects, w.subjects) {
			t.Errorf("RoleBinding %s = %v %v, want %v %v", key, rb.RoleRef, rb.Subjects, ref(w.role), w.subjects)
		}
	}
	for key := range r.roleBindings {
		if strings.HasPrefix(key, "team-c/") {
			t.Errorf("RoleBinding %s rendered for an empty subject list", key)
		}
	}
	for name, crb := range r.clusterRoleBindings {
		if crb.RoleRef.Name == "kaalm-developer" || crb.RoleRef.Name == "kaalm-secrets-admin" {
			t.Errorf("ClusterRoleBinding %s binds %s cluster-wide", name, crb.RoleRef.Name)
		}
	}
	for _, crb := range r.clusterRoleBindings {
		if slices.Contains(personaRoles, crb.RoleRef.Name) && crb.Labels["app.kubernetes.io/name"] != "kaalm" {
			t.Errorf("ClusterRoleBinding %s labels = %v, want the kaalm labels", crb.Name, crb.Labels)
		}
	}
	for key, rb := range r.roleBindings {
		if slices.Contains(personaRoles, rb.RoleRef.Name) && rb.Labels["app.kubernetes.io/name"] != "kaalm" {
			t.Errorf("RoleBinding %s labels = %v, want the kaalm labels", key, rb.Labels)
		}
	}
}

// developers must be a map of namespace to subjects; a list fails the render.
func TestPersonas_DevelopersMustBeAMap(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	out, err := runFullTemplate(valuesFile(t, `
rbac:
  personas:
    enabled: true
    developers:
      - kind: Group
        name: team-a-devs
`)...)
	if err == nil {
		t.Fatalf("helm template succeeded with developers as a list\n%s", out)
	}
	msg := "rbac.personas.developers must be a map of namespace to a list of subjects"
	if !strings.Contains(string(out), msg) {
		t.Errorf("helm template output lacks %q:\n%s", msg, out)
	}
}

// aggregateToDefaultRoles adds the namespaced kinds to view, edit and admin,
// independent of personas, and never the catalog kinds or Secrets.
func TestPersonas_Aggregation(t *testing.T) {
	r := renderRBAC(t, "--set", "rbac.aggregateToDefaultRoles=true")
	want := map[string]struct {
		labels []string
		rules  []string
	}{
		"kaalm-aggregate-to-view": {
			[]string{aggregateLabelPrefix + "view"},
			sorted(rulesFor("kaalm.io", namespacedKinds, readVerbs...)),
		},
		"kaalm-aggregate-to-edit": {
			[]string{aggregateLabelPrefix + "edit", aggregateLabelPrefix + "admin"},
			sorted(rulesFor("kaalm.io", namespacedKinds,
				"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection")),
		},
	}
	for name, w := range want {
		cr, ok := r.clusterRoles[name]
		if !ok {
			t.Errorf("ClusterRole %s missing", name)
			continue
		}
		var labels []string
		for k, v := range cr.Labels {
			if strings.HasPrefix(k, aggregateLabelPrefix) {
				if v != "true" {
					t.Errorf("%s label %s = %q, want \"true\"", name, k, v)
				}
				labels = append(labels, k)
			}
		}
		sort.Strings(labels)
		wl := slices.Clone(w.labels)
		sort.Strings(wl)
		if !reflect.DeepEqual(labels, wl) {
			t.Errorf("%s aggregate labels = %v, want %v", name, labels, wl)
		}
		if got := ruleSet(cr.Rules); !reflect.DeepEqual(got, w.rules) {
			t.Errorf("%s rules = %v, want %v", name, got, w.rules)
		}
	}
	for name, cr := range r.clusterRoles {
		if !hasAggregateLabel(cr.Labels) {
			continue
		}
		if !slices.Contains(aggregateRoles, name) {
			t.Errorf("unexpected aggregating ClusterRole %s", name)
		}
		if namesResource(cr.Rules, func(s string) bool { return slices.Contains(catalogKinds, s) || s == "secrets" }) {
			t.Errorf("%s aggregates a catalog kind or secrets: %v", name, cr.Rules)
		}
		if namesResource(cr.Rules, func(s string) bool { return strings.HasSuffix(s, "/status") }) {
			t.Errorf("%s names a status subresource", name)
		}
	}
	wantNoPersonaOrAggregateObjects(t, r, personaRoles)

	off := renderRBAC(t)
	wantNoPersonaOrAggregateObjects(t, off, aggregateRoles)
}
