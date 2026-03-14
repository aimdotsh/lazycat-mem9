# lazycat-mem9

`lazycat-mem9` is a single-user mem9 deployment for Lazycat.

It keeps the mem9 server API and OpenClaw integration flow, but replaces the original TiDB-centric hosted model with:

- built-in MySQL on Lazycat
- single memory pool
- install-time API key
- runtime-generated docs based on the current app domain

## What It Includes

- Go server with mem9-compatible `v1alpha2` APIs
- built-in web console at `/`
- runtime `SKILL.md` at `/SKILL.md`
- Lazycat packaging files:
  - `lzc-manifest.yml`
  - `lzc-build.yml`
  - `lzc-deploy-params.yml`
  - `build.sh`
  - `run.sh`

## Build

```bash
lzc-cli project build .
```

The build script will:

- compile `./cmd/mnemo-server`
- place artifacts in `./dist`
- package `run.sh` together with the binary

If local Go is unavailable, `build.sh` can fall back to Docker.

## Install Parameters

- `api_key`: the single-user API key used by OpenClaw and other clients
- `tenant_name`: display name for the default memory space
- `ingest_mode`: `raw` or `smart`
- `llm_api_key`: optional, only needed for smart ingest
- `llm_base_url`: optional OpenAI-compatible endpoint
- `llm_model`: model name for smart ingest

## Runtime Behavior

- App homepage is public.
- Actual memory APIs are protected by `X-API-Key`.
- `/SKILL.md` is generated from the current request domain, so the repo does not hardcode a specific deployed hostname.
- Multiple OpenClaw instances using the same `apiUrl + apiKey` will share the same memory pool.

## Project Layout

```text
.
├── cmd/mnemo-server
├── internal
├── build.sh
├── run.sh
├── lzc-build.yml
├── lzc-manifest.yml
├── lzc-deploy-params.yml
└── icon.png
```

## Upstream

This project is derived from `mem9-ai/mem9`, then simplified for Lazycat single-user deployment.
