# Contributing

This repository is generated: everything in it except `api/` is written by
[protoc-gen-sdk](https://github.com/the-protobuf-project/sdk) from the API's
protos and `api/sdk.yaml`. Changes to generated files are overwritten by the
next regeneration.

- To change the SDK's shape (names, services, docs, options), edit
  `api/sdk.yaml` and regenerate.
- To change hand-written helpers or examples, edit them under
  `api/custom`, the source the generator copies them from.
- To change the API, change its protos; the regenerate workflow picks up the
  new release.

## Regenerate locally

```sh
go install github.com/googleapis/gapic-generator-go/cmd/protoc-gen-go_gapic@v0.65.0
go install github.com/the-protobuf-project/sdk/plugin/cmd/protoc-gen-sdk@v0.1.1
bash scripts/generate.sh
```

`scripts/generate.sh` runs `buf generate` in `api/`, then `go mod tidy`,
`go build`, `go vet` and `go test`.
