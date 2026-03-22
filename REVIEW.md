# Review notes for `s3duck-tui`

## What looks good

- clear package split between config / model / controller / view
- practical feature set for an S3 TUI
- decent handling of long-running download/upload UI updates
- generally readable codebase

## Concrete issues found

1. **Config file stores secrets with executable permissions**
   - `internal/config/config.go` wrote the JSON config with mode `0700`
   - the file contains access keys and secret keys
   - executable permission is unnecessary; `0600` is a better default

2. **Config creation/writes ignored serialization and setup errors**
   - `CreateEmptyConfig` and `WriteConfig` ignored some errors
   - that can hide broken config creation or partial setup failures

3. **Upload loop accumulated deferred cancels**
   - `pkg/model/model.go` created a fresh child context per file and deferred every `cancel()`
   - on large uploads this unnecessarily stacks deferred calls until the whole function exits

4. **File-properties modal could nil-deref metadata fields**
   - `pkg/controller/controller.go` assumed `Size` and `Etag` were always non-nil
   - making that UI path defensive is safer when talking to varied S3-compatible backends

## Changes included in PR

- use `0600` for config files
- check and report config serialization/write errors properly
- simplify empty-config creation
- cancel upload subcontexts immediately after each upload finishes
- make file-properties rendering nil-safe
