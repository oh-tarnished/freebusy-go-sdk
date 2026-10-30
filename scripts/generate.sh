#!/usr/bin/env bash
# Regenerates this repository from api/: builds the reference images
# api/sdk.yaml lists, runs protoc-gen-sdk over the API with buf, then tidies,
# builds, vets and tests the module.
#
# Needs buf, protoc-gen-sdk and protoc-gen-go_gapic on PATH (CONTRIBUTING.md).
set -euo pipefail
cd "$(dirname "$0")/../api"
buf build buf.build/the-protobuf-project/rfc:3aba0422074b4341aa0f8df07c3263b7 -o rfc.binpb
buf generate
cd ..
go mod tidy
# SDK_SKIP_TESTS=1 stops after generating (the GitHub Action's test: false).
if [ -n "${SDK_SKIP_TESTS:-}" ]; then exit 0; fi
go build ./...
go vet ./...
go test ./...
