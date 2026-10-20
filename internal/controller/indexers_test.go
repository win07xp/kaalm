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

package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// withReferrerIndexes registers every referrer index SetupIndexers registers,
// from the same table, so a fake client's referrer lookups follow the
// manager's rules. b must already have its scheme set. The fake builder
// panics on a field registered twice, so a caller must not also register a
// referrer index by hand.
func withReferrerIndexes(b *fake.ClientBuilder) *fake.ClientBuilder {
	for _, r := range referrerFieldIndexes {
		b = b.WithIndex(r.obj, r.f.key, r.f.extract)
	}
	return b
}
