# 30 — Setup Wizard: Derived Database URL and a Comment-Free `.env`

Depends on: [`01-architecture-and-deployment.md`](01-architecture-and-deployment.md)
("Interactive setup"); implemented in `internal/config/setup.go`.

Amends: "Interactive setup" in `01` — it gains a pointer to this spec.

## Why this spec exists

Two defects in `docker compose run --rm setup` and the `.env.example` it reads,
found on the first real install on the Synology NAS.

**1. The database URL is asked for separately from the values it is built
from.** `.env.example` holds `POSTGRES_USER`, `POSTGRES_PASSWORD` and
`POSTGRES_DB` and, as a fourth, independent variable, `DATABASE_URL` with the
same three values typed into it again. The `db` container is initialised from
the first three; the app connects with the fourth. Changing the password at the
prompt and accepting the URL's default leaves the two disagreeing, and the only
symptom is an authentication error from the app. Nothing about that URL is a
real choice: the host is the compose service name, the port is fixed, and the
rest is the three answers just given.

**2. Compose reads an inline comment after an empty value as the value.**
Verified against Compose 2.24 and 2.31, reading the shipped `.env.example`
through an `env_file`:

```
STATIC_DIR=# empty = serve embedded assets; dev sets a path
GEMINI_IMAGE_MODEL=# optional segmentation model (e.g. gemini-2.5-flash);
```

`STATIC_DIR` is then a path that does not exist, and the server answers `404`
to every page, `/` included — with the log line `no route matches /`, which
points nowhere near the cause. `GEMINI_IMAGE_MODEL` becomes a bogus model name,
which switches background removal on with a model that cannot exist. A value
followed by a comment (`APP_ENV=prod   # …`) *is* read correctly, which is why
this went unnoticed; development never hits it because
`docker-compose.override.yml` sets `STATIC_DIR` itself. The wizard makes it
universal: `setup.go` writes each template line's trailing comment back after
the value.

## The database URL is derived, not prompted

- The wizard does **not** prompt for `DATABASE_URL`. Once `POSTGRES_USER`,
  `POSTGRES_PASSWORD` and `POSTGRES_DB` have their final answers, it computes
  `postgres://<user>:<password>@db:5432/<db>?sslmode=disable` and writes that.
  It prints the line the way it prints the generated secret
  (`DATABASE_URL     derived from POSTGRES_*`), so nobody wonders where it went.
- It is built with a URL builder (`net/url`, `UserPassword` and a path), **never
  by string concatenation**: user, password and database name are percent-encoded
  where they need it.
- Host `db`, port `5432` and `sslmode=disable` are fixed. They are what the
  compose network provides; the wizard is the setup for that stack.
- It is derived after all prompts, from the final answers, wherever the line sits
  in the template.
- `.env.example` keeps a `DATABASE_URL` line, with the same defaults as the
  `POSTGRES_*` lines, so the key list stays complete and a manual copy is still
  coherent. The wizard overwrites it and never shows its value as a default to
  accept.
- Someone who needs a different URL (an external Postgres) edits `.env` after
  setup. The wizard offers no override, on purpose: an override prompt is exactly
  the second source of truth this spec removes.
- The env-file download in the admin area (`GET /api/admin/settings/env-file`,
  `03`) is rendered from the loaded config and is untouched; it keeps producing
  the same file the wizard would.

## A generated `.env` has no inline comments

- The generated `.env` never carries a comment after a value. A variable's note
  is written on its **own line above** the variable.
- `.env.example` is rewritten the same way — notes above, none after a value — so
  a manual `cp .env.example .env` is safe too, not only the wizard's output.
- A template that still has an inline note is not an error: the wizard moves it
  above the variable when it writes. What must never happen is an inline comment
  in the output.

## Acceptance criteria

- [ ] The wizard never prompts for `DATABASE_URL`; run with all defaults it writes `postgres://inventory:changeme@db:5432/inventory?sslmode=disable`.
- [ ] Answering non-default `POSTGRES_USER`, `POSTGRES_PASSWORD` and `POSTGRES_DB` changes `DATABASE_URL` to match; a test asserts the exact string.
- [ ] Round trip: for passwords containing `@`, `:`, `/`, `?`, `#`, `%`, `&`, `=`, `+`, a space and a non-ASCII letter, parsing the written `DATABASE_URL` with `net/url` returns exactly the entered user, password and database name.
- [ ] No value from the template's `DATABASE_URL` line and none from a previous `.env` survives into the output: two runs with different `POSTGRES_*` answers write two different URLs.
- [ ] The generated `.env` contains no line in which whitespace followed by `#` appears after the first `=`, for the shipped `.env.example` and for a template that still has inline notes.
- [ ] Verified against Compose itself, not against the wizard's own parser: reading the generated `.env` through a Compose `env_file` (a throwaway service is enough) yields an **empty** `STATIC_DIR` and an empty `GEMINI_IMAGE_MODEL`. The PR shows the output.
- [ ] The existing guarantees hold: every key of `.env.example` reaches `.env`, `SESSION_SECRET` is generated, an existing `.env` is never overwritten without confirmation, and the file is written `0600`.
- [ ] The description of the wizard's prompts in `01` and in `README.md` is updated to match, and no doc still says `DATABASE_URL` is prompted for.

## Watch for

- The password reaches Postgres by two routes — as `POSTGRES_PASSWORD`, which
  `docker-compose.yml` interpolates from `.env` for the `db` service, and inside
  `DATABASE_URL`. Compose rewrites some characters when it reads `.env` (`$`
  starts an interpolation). A password that one route alters and the other does
  not recreates exactly the mismatch this spec removes. Either reject such
  characters at the `POSTGRES_PASSWORD` prompt with an explanation, or prove with
  a test that they survive both routes; do not ship it silently undecided.
- `splitTrailingComment` and the `trailing` field in `setup.go` are the bug path:
  the fix is in what `writeEnv` emits, not in how the template is parsed.
- Do not add `COMPOSE_FILE` or any other deployment-selector to `.env.example`
  (see the Synology section of `01`): the wizard prompts for every key there.

## Out of scope

- Preserving lines in an existing `.env` that are not in the template. The
  wizard rewrites the file from the template, so it drops such lines (the Synology
  section of `01` documents the consequence for the two `COMPOSE_` lines). A
  separate question with its own trade-offs.
- Choosing the database host or port, or supporting an external Postgres.
- Any variable other than the ones named above.
