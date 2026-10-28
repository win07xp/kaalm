module kaalm.io/starter-go

go 1.26.0

// This template imports github.com/win07xp/kaalm/agentruntime/v2. Inside the
// Kaalm repo the repo's go.work resolves that import, so there is deliberately
// no require line here. In your copy, pin the module to the version of the
// chart your cluster runs, then write go.sum:
//
//	go get github.com/win07xp/kaalm/agentruntime/v2@vX.Y.Z
//	go mod tidy
//
// A bare `go mod tidy` picks the newest published version instead.
