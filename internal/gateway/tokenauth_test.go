/*
Copyright 2026.

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

package gateway

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestReview_Error(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, (*authnv1.TokenReview)(nil), errors.New("apiserver down")
	})
	r := &KubeTokenReviewer{Client: client}
	if _, _, err := r.Review(context.Background(), "tok"); err == nil {
		t.Error("Review must surface apiserver errors")
	}
}

func TestJWTExpiry(t *testing.T) {
	// Not three parts.
	if _, ok := jwtExpiry("abc"); ok {
		t.Error("non-JWT must not parse")
	}
	// Bad base64 payload.
	if _, ok := jwtExpiry("a.$$$.c"); ok {
		t.Error("bad base64 payload must not parse")
	}
	// Valid payload with exp.
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1700000000}`))
	exp, ok := jwtExpiry("h." + payload + ".s")
	if !ok || !exp.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("valid exp = %v ok=%v", exp, ok)
	}
	// Payload with no exp claim.
	noExp := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`))
	if _, ok := jwtExpiry("h." + noExp + ".s"); ok {
		t.Error("missing exp must not parse")
	}
}
