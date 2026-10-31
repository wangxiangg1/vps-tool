# Go control plane

This is an experimental parallel Go implementation. It is not feature-complete
or ready for production. Keep the existing Python control plane and never run
both servers against the same database. Its separate Dockerfile is built by CI
as a candidate; the published production image still uses `server/Dockerfile`.
The `Publish Go Control Plane Candidate` workflow verifies the code and publishes
`ghcr.io/wangxiangg1/vps-tool:go-candidate` plus a commit-specific `go-<sha>` tag.
Candidate validation must use a separate data volume or a backup copy. The
optional `compose.go.yaml` override selects the candidate image and its built-in
health check; it does not replace the current Python production configuration.

From this directory:

```powershell
$env:GOPROXY = "https://goproxy.cn,direct"
go mod download
go test ./...
go build -trimpath -ldflags "-s -w -buildid=" ./cmd/vps-server
```
