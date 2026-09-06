# zstd

Zstandard response compression for the RoadRunner HTTP plugin. The implementation uses [`klauspost/compress/gzhttp`](https://pkg.go.dev/github.com/klauspost/compress/gzhttp) and does not require CGO.

## Usage

Include `github.com/roadrunner-server/zstd/v6` in your RoadRunner build. Register `&zstd.Plugin{}` with Endure alongside the HTTP and server plugins. Configuration alone cannot load a plugin that is absent from the build.

Add `zstd` to the middleware list in your HTTP configuration:

```yaml
http:
  middleware: ["zstd"]
```

The plugin does not require a separate configuration section. A client requests compression with `Accept-Encoding: zstd`. Eligible responses use `Content-Encoding: zstd`.

## Behavior

- Only zstd response compression is enabled. The plugin does not provide gzip fallback or request-body decompression.
- Clients must request `zstd` explicitly with a nonzero quality value. Missing or unsupported encodings and `zstd;q=0` leave the response uncompressed.
- The default compression level is `zstd.SpeedFastest`. Encoders are pooled.
- The normal minimum response size is 1,024 bytes. A flush can start compression below this limit.
- The wrapper skips HEAD requests, empty bodies, existing `Content-Encoding`, `Content-Range`, and content types excluded by the library.
- The wrapper adds `Vary: Accept-Encoding`. It removes the original `Content-Length` when it compresses a response.
- HTTP behavior follows the same library as the gzip plugin. The wrapper does not change ETags by default.
- Tracing follows the RoadRunner middleware convention when the request contains the RoadRunner tracer context key.

If both gzip and zstd middleware are enabled, their order can determine the selected encoding. The separate plugins do not compare quality values with each other.

## Tests

Run the unit tests and static checks from the repository root:

```sh
go test -race -tags=debug ./...
go vet ./...
```

The integration tests use a separate Go module and a real PHP worker. They require Composer and PHP with the extensions required by the worker packages, including `sockets`. The worker uses the `php` executable from `PATH`.

```sh
composer --working-dir=tests/php_test_files install
go -C tests test -race -tags=debug ./...
go -C tests vet ./...
```

The integration tests register the plugin through Endure and select it through `http.middleware`. They check the encoded response and decode it with zstd. They also check unsupported clients and a registered plugin that is not selected in the middleware list.
