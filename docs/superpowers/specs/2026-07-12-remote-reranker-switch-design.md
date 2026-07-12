# Remote Reranker Switch Design

## Goal

Add an explicit environment switch that decides whether examSystem uses the
cloud reranker or the existing local reranker.

## Configuration

The switch is named `RERANK_USE_REMOTE` and accepts case-insensitive boolean
values:

- `true`: use the cloud reranker.
- `false`: use the local reranker.
- unset or empty: behave as `false`.
- any other value: fail application startup with a configuration error.

When the switch is `true`, `RERANK_API_URL`, `RERANK_API_KEY`, and
`RERANK_MODEL` must all be non-empty. Missing values fail startup without
including the API key in the error message. The scoring service receives the
cloud scorer for text and code and sets `allow_model_load=False`.

When the switch is `false`, cloud settings are ignored and the scoring service
uses `allow_model_load=True`, preserving the existing local model behavior.

## Integration

The project root `.env` and `.env.example` expose the switch. Docker Compose
passes it through to the backend container. Existing environment precedence is
unchanged: process and Docker environment variables override `.env` values.

## Tests

Tests cover the default local behavior, explicit local behavior with cloud
credentials present, explicit remote behavior, missing remote settings, and
invalid boolean values. Existing client lifecycle and concurrency tests remain
in place.
