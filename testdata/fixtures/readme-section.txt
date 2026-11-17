-- message --
readme: explain how to run the tests
-- expect --
low
-- base/README.md --
# app

A small service.
-- head/README.md --
# app

A small service.

## Tests

Run `go test ./...`. The integration tests need Docker.
