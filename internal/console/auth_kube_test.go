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

package console

import (
	"context"
	"fmt"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestKubeTokenReviewer(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "tokenreviews",
		func(action clienttesting.Action) (bool, runtime.Object, error) {
			review := action.(clienttesting.CreateAction).GetObject().(*authnv1.TokenReview)
			if review.Spec.Token == "good" {
				return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{
					Authenticated: true,
					User:          authnv1.UserInfo{Username: "priya", Groups: []string{"platform"}},
				}}, nil
			}
			return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{
				Authenticated: false, Error: "invalid",
			}}, nil
		})

	r := &KubeTokenReviewer{Client: cs}
	id, err := r.Review(context.Background(), "good")
	if err != nil || id.Username != "priya" || len(id.Groups) != 1 {
		t.Fatalf("review = %+v, %v", id, err)
	}
	if _, err := r.Review(context.Background(), "bad"); err == nil {
		t.Error("an unauthenticated token must error")
	}

	// An apiserver failure surfaces as an error, never as authenticated.
	cs2 := fake.NewClientset()
	cs2.PrependReactor("create", "tokenreviews",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("apiserver down")
		})
	if _, err := (&KubeTokenReviewer{Client: cs2}).Review(context.Background(), "good"); err == nil {
		t.Error("an apiserver failure must error")
	}
}

func TestKubeAuthorizer(t *testing.T) {
	cs := fake.NewClientset()
	var gotAttrs *authzv1.ResourceAttributes
	cs.PrependReactor("create", "subjectaccessreviews",
		func(action clienttesting.Action) (bool, runtime.Object, error) {
			sar := action.(clienttesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
			gotAttrs = sar.Spec.ResourceAttributes
			allowed := sar.Spec.User == "priya" && gotAttrs.Namespace == "team-a"
			return true, &authzv1.SubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
		})

	a := &KubeAuthorizer{Client: cs}
	ok, err := a.Allowed(context.Background(), Identity{Username: "priya", Groups: []string{"g"}},
		"list", "kaalm.io", "agents", "team-a")
	if err != nil || !ok {
		t.Fatalf("allowed = %v, %v", ok, err)
	}
	if gotAttrs.Verb != "list" || gotAttrs.Group != "kaalm.io" || gotAttrs.Resource != "agents" {
		t.Errorf("attributes = %+v", gotAttrs)
	}
	if ok, _ := a.Allowed(context.Background(), Identity{Username: "dev"},
		"list", "kaalm.io", "agents", "team-a"); ok {
		t.Error("a denied SAR must report false")
	}

	cs2 := fake.NewClientset()
	cs2.PrependReactor("create", "subjectaccessreviews",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("apiserver down")
		})
	if _, err := (&KubeAuthorizer{Client: cs2}).Allowed(context.Background(), Identity{},
		"list", "kaalm.io", "agents", "team-a"); err == nil {
		t.Error("an apiserver failure must error, never default-allow")
	}
}
