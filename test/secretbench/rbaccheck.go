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
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	rbacNamespace = "secretbench-rbac"
	// rbacUser is a client-certificate user with no group but
	// system:authenticated, so only its Role grants it anything.
	rbacUser = "secretbench-reader"
	// checkWatchTimeout shortens the readers' watches. An open watch
	// outlives its grant, because the authorizer runs when a request
	// starts, so a revoked grant shows at the next LIST or WATCH.
	checkWatchTimeout = 5 * time.Second
	// revokeBound covers RBAC propagation, one watch timeout, and a
	// backoff step.
	revokeBound = 30 * time.Second
	// revokePoll spaces the reads after revocation: each one costs a
	// refused LIST.
	revokePoll      = 500 * time.Millisecond
	grantWait       = 10 * time.Second
	checkUpdateWait = 5 * time.Second
)

var (
	grantedNames   = []string{"granted-0", "granted-1", "granted-2"}
	ungrantedNames = []string{"ungranted-0", "ungranted-1"}
)

// checkResult is one check's outcome for one reader, or for the user when
// the check does not depend on a reader.
type checkResult struct {
	Reader, Check, Detail string
	Pass                  bool
}

// checker records results and prints each as it lands.
type checker struct {
	out     io.Writer
	results []checkResult
}

func (c *checker) record(reader, check string, pass bool, format string, args ...any) {
	r := checkResult{Reader: reader, Check: check, Pass: pass, Detail: fmt.Sprintf(format, args...)}
	c.results = append(c.results, r)
	printChecks(c.out, []checkResult{r})
}

// rbacCheck runs the C-gw and lean readers as a user bound to a Role that
// grants get, list, and watch on named Secrets only, through the API
// server's RBAC authorizer, and checks that (a) granted names are served
// from the watch, (b) a LIST or WATCH without a name selector is refused,
// (c) a name-selected LIST or WATCH on an ungranted name is refused, and (d)
// once a name leaves the Role the reader stops serving it and answers the
// refusal. It returns an error when any check failed.
func rbacCheck(ctx context.Context, env *envtest.Environment, base *rest.Config, out io.Writer) error {
	adminCfg := rest.CopyConfig(base)
	adminCfg.QPS, adminCfg.Burst = -1, 0
	admin, err := kubernetes.NewForConfig(adminCfg)
	if err != nil {
		return fmt.Errorf("admin client: %w", err)
	}
	if err := ensureRBACFixture(ctx, admin); err != nil {
		return err
	}
	authed, err := env.AddUser(envtest.User{Name: rbacUser}, base)
	if err != nil {
		return fmt.Errorf("add user %s: %w", rbacUser, err)
	}
	userCfg := authed.Config()
	user, err := kubernetes.NewForConfig(userCfg)
	if err != nil {
		return fmt.Errorf("user client: %w", err)
	}
	if err := waitGranted(ctx, user, grantedNames[0]); err != nil {
		return err
	}

	c := &checker{out: out}
	identityChecks(ctx, c, user)
	readers := []struct {
		name  string
		build func(context.Context, *rest.Config, kubernetes.Interface) (secretReader, error)
	}{
		{"C-gw", func(ctx context.Context, _ *rest.Config, client kubernetes.Interface) (secretReader, error) {
			return newNameInformerReader(ctx, client, checkWatchTimeout), nil
		}},
		{"lean", func(ctx context.Context, rc *rest.Config, client kubernetes.Interface) (secretReader, error) {
			return newLeanReader(ctx, rc, client, checkWatchTimeout)
		}},
	}
	for _, rd := range readers {
		if err := readerChecks(ctx, c, admin, user, userCfg, rd.name, rd.build); err != nil {
			return err
		}
	}
	return summarize(c.results)
}

// ensureRBACFixture creates the namespace, the granted and ungranted
// Secrets, and the user's Role and RoleBinding, or resets the Role when it
// exists.
func ensureRBACFixture(ctx context.Context, admin kubernetes.Interface) error {
	labels := map[string]string{labelPhase: phaseValue}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: rbacNamespace, Labels: labels}}
	if _, err := admin.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); ignoreExists(err) != nil {
		return fmt.Errorf("create namespace %s: %w", rbacNamespace, err)
	}
	secrets := admin.CoreV1().Secrets(rbacNamespace)
	for _, name := range append(append([]string{}, grantedNames...), ungrantedNames...) {
		if _, err := secrets.Create(ctx, benchSecret(rbacNamespace, name), metav1.CreateOptions{}); ignoreExists(err) != nil {
			return fmt.Errorf("create Secret %s: %w", name, err)
		}
	}
	if err := setGrant(ctx, admin, grantedNames); err != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: rbacNamespace, Name: rbacUser, Labels: labels},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: rbacUser},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: rbacUser}},
	}
	_, err := admin.RbacV1().RoleBindings(rbacNamespace).Create(ctx, binding, metav1.CreateOptions{})
	if ignoreExists(err) != nil {
		return fmt.Errorf("create RoleBinding: %w", err)
	}
	return nil
}

// setGrant creates the user's Role, or rewrites its resourceNames. names
// must not be empty: an empty list grants every Secret.
func setGrant(ctx context.Context, admin kubernetes.Interface, names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("an empty resourceNames list would grant every Secret")
	}
	roles := admin.RbacV1().Roles(rbacNamespace)
	role, err := roles.Get(ctx, rbacUser, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		role = &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Namespace: rbacNamespace, Name: rbacUser,
				Labels: map[string]string{labelPhase: phaseValue}},
			Rules: []rbacv1.PolicyRule{secretRule(names)},
		}
		_, err = roles.Create(ctx, role, metav1.CreateOptions{})
	case err == nil:
		role.Rules = []rbacv1.PolicyRule{secretRule(names)}
		_, err = roles.Update(ctx, role, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("set Role %s: %w", rbacUser, err)
	}
	return nil
}

// secretRule grants get, list, and watch on the named Secrets.
func secretRule(names []string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{
		APIGroups: []string{""}, Resources: []string{secretsResource},
		Verbs: []string{"get", "list", "watch"}, ResourceNames: names,
	}
}

// waitGranted waits until the authorizer lets the user GET name.
func waitGranted(ctx context.Context, user kubernetes.Interface, name string) error {
	var last error
	ok := waitFor(ctx, time.Now().Add(grantWait), 100*time.Millisecond, func() bool {
		_, last = user.CoreV1().Secrets(rbacNamespace).Get(ctx, name, metav1.GetOptions{})
		return last == nil
	})
	if !ok {
		return fmt.Errorf("the grant on %s did not reach the authorizer within %s: %v", name, grantWait, last)
	}
	return nil
}

// identityChecks are (b) and the direct part of (c): what the API server
// answers the user, whatever the reader.
func identityChecks(ctx context.Context, c *checker, user kubernetes.Interface) {
	secrets := user.CoreV1().Secrets(rbacNamespace)
	byName := fields.OneTermEqualSelector("metadata.name", ungrantedNames[0]).String()

	_, err := secrets.List(ctx, metav1.ListOptions{})
	c.record("user", "(b) LIST without a name selector is refused", apierrors.IsForbidden(err), "%v", errText(err))
	err = tryWatch(ctx, secrets, metav1.ListOptions{})
	c.record("user", "(b) WATCH without a name selector is refused", apierrors.IsForbidden(err), "%v", errText(err))
	_, err = secrets.List(ctx, metav1.ListOptions{FieldSelector: byName})
	c.record("user", "(c) LIST by name of an ungranted Secret is refused", apierrors.IsForbidden(err), "%v", errText(err))
	err = tryWatch(ctx, secrets, metav1.ListOptions{FieldSelector: byName})
	c.record("user", "(c) WATCH by name of an ungranted Secret is refused", apierrors.IsForbidden(err), "%v", errText(err))
}

// tryWatch opens a watch and closes it at once; the error is the refusal.
func tryWatch(ctx context.Context, secrets corev1client.SecretInterface, opts metav1.ListOptions) error {
	w, err := secrets.Watch(ctx, opts)
	if err == nil {
		w.Stop()
	}
	return err
}

// readerChecks runs (a), the reader part of (c), and (d) for one reader
// built fresh as the user at the gateway's limit, then restores the grant
// and drains the reader.
func readerChecks(ctx context.Context, c *checker, admin, user kubernetes.Interface, userCfg *rest.Config,
	name string, build func(context.Context, *rest.Config, kubernetes.Interface) (secretReader, error)) error {
	rc, counter := countedConfig(userCfg, gatewayQPS, gatewayBurst)
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return fmt.Errorf("%s client: %w", name, err)
	}
	reader, err := build(ctx, rc, client)
	if err != nil {
		return fmt.Errorf("%s reader: %w", name, err)
	}
	defer func() {
		reader.Stop()
		waitFor(ctx, time.Now().Add(grantWait), 100*time.Millisecond, func() bool {
			return counter.openWatches.Load() == 0
		})
		counter.CloseIdleConnections()
	}()

	checkServed(ctx, c, admin, reader, counter, name)

	_, err = reader.Get(ctx, rbacNamespace, ungrantedNames[0])
	got := counter.snapshot().Get
	c.record(name, "(c) Get of an ungranted Secret is refused", apierrors.IsForbidden(err) && got == 0,
		"%v; GET %d", errText(err), got)

	if err := checkRevoked(ctx, c, admin, reader, name); err != nil {
		return err
	}
	if err := setGrant(ctx, admin, grantedNames); err != nil {
		return err
	}
	return waitGranted(ctx, user, grantedNames[0])
}

// checkServed is (a): every granted name is served, an update arrives over
// the watch, and no read fell back to a live GET.
func checkServed(ctx context.Context, c *checker, admin kubernetes.Interface, reader secretReader,
	counter *countingTransport, name string) {
	for _, n := range grantedNames {
		want, err := admin.CoreV1().Secrets(rbacNamespace).Get(ctx, n, metav1.GetOptions{})
		if err != nil {
			c.record(name, "(a) granted Secrets are served from the watch", false, "admin GET %s: %v", n, err)
			return
		}
		got, err := reader.Get(ctx, rbacNamespace, n)
		if err != nil || got.ResourceVersion != want.ResourceVersion {
			c.record(name, "(a) granted Secrets are served from the watch", false, "Get %s: %v", n, errText(err))
			return
		}
	}
	start := time.Now()
	rv, err := updateSecret(ctx, admin, rbacNamespace, grantedNames[1])
	if err != nil {
		c.record(name, "(a) granted Secrets are served from the watch", false, "update: %v", err)
		return
	}
	seen := waitFor(ctx, start.Add(checkUpdateWait), 10*time.Millisecond, func() bool {
		sec, err := reader.Get(ctx, rbacNamespace, grantedNames[1])
		return err == nil && sec.ResourceVersion == rv
	})
	counts := counter.snapshot()
	pass := seen && counts.Get == 0 && counts.List == int64(len(grantedNames))
	c.record(name, "(a) granted Secrets are served from the watch", pass,
		"%d served; update seen %t in %s; GET %d, LIST %d, WATCH %d",
		len(grantedNames), seen, time.Since(start).Round(time.Millisecond), counts.Get, counts.List, counts.Watch)
}

// checkRevoked is (d): the Role drops granted-0, and the reader must answer
// Forbidden for it within revokeBound while it still serves granted-1.
func checkRevoked(ctx context.Context, c *checker, admin kubernetes.Interface, reader secretReader, name string) error {
	if err := setGrant(ctx, admin, grantedNames[1:]); err != nil {
		return err
	}
	start := time.Now()
	var last error
	refused := waitFor(ctx, start.Add(revokeBound), revokePoll, func() bool {
		_, last = reader.Get(ctx, rbacNamespace, grantedNames[0])
		return apierrors.IsForbidden(last)
	})
	_, stillErr := reader.Get(ctx, rbacNamespace, grantedNames[1])
	detail := fmt.Sprintf("refused after %s; still-granted Get: %v", time.Since(start).Round(time.Millisecond),
		errText(stillErr))
	if !refused {
		detail = fmt.Sprintf("still served after %s (last: %v)", revokeBound, errText(last))
	}
	c.record(name, "(d) a revoked Secret stops being served", refused && stillErr == nil, "%s", detail)
	return nil
}

// printChecks writes one line per result.
func printChecks(out io.Writer, results []checkResult) {
	for _, r := range results {
		verdict := "PASS"
		if !r.Pass {
			verdict = "FAIL"
		}
		_, _ = fmt.Fprintf(out, "rbac-check  %-5s  %s: %s  %s\n", r.Reader, r.Check, verdict, r.Detail)
	}
}

// summarize fails when any check failed.
func summarize(results []checkResult) error {
	failed := 0
	for _, r := range results {
		if !r.Pass {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("rbac-check: %d of %d checks failed", failed, len(results))
	}
	return nil
}

func ignoreExists(err error) error {
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func errText(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}
