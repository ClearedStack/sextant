<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/logo-dark.svg">
    <img src="brand/logo-light.svg" alt="Sextant" height="56">
  </picture>
</p>

<p align="center">
  <strong>Reads a candidate's public pages and writes down what job it does, copying nothing.</strong><br>
  Part of <a href="https://clearedstack.com">ClearedStack</a>
</p>

---

> **Status: ready to deploy.** Both `sextant observe` (offline single-page CLI) and `sextant work` (the always-on Muster `v1` worker with passive HTTPS fetching, `robots.txt` enforcement, atomic `/var/spool/cs` spooling, all four registered sub-tools, and hardened Kubernetes manifests in [`deploy/`](deploy)) build and pass `go vet ./...` and `go test -race ./...` with the Go standard library alone.

Sextant is stage 02 of the ClearedStack pipeline. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Sextant can also be built and run standalone on any saved page without the cluster.

## Commands

### 1. Offline inspection (`sextant observe`)

Give `sextant observe` a saved page (`-` for standard input), the public HTTPS address it came from, and an observation date (`YYYY-MM-DD`). It opens no network connection, reads no clock, and writes no file:

```sh
go build -o sextant ./cmd/sextant

./sextant observe \
  -item PA-0001 \
  -url https://calc.example-agency.gov/due-date \
  -page tools/gov-portal/fixtures/observed-calculator/page.html \
  -observed-on 2026-01-15
```

| Exit status | Meaning |
| --- | --- |
| `0` | A schema-validated observation record was printed on standard output. |
| `2` | Invalid arguments or configuration. Nothing is printed on standard output. |
| `10` | `E_NEEDS_SUBTOOL`. Valid but unfamiliar target; would pause via `/v1/needs_subtool`. |
| `11` | `E_PARSE`. Broken markup, corrupted PDF, or schema check failure; would halt via `/v1/fail`. |
| `12` | `E_TIMEOUT`. Deadline or transient upstream failure; would retry via `/v1/fail`. |

### 2. Cluster worker (`sextant work`)

In the cluster (`cs-workers` namespace), `sextant work` polls Muster over cluster-private HTTP using its projected Kubernetes ServiceAccount bearer token (`CONTRACT_VERSION=v1`):

```sh
./sextant work \
  -muster-url http://muster.clearedstack.svc.cluster.local:8080 \
  -spool /var/spool/cs
```

| Flag | Environment variable | Default | Purpose |
| --- | --- | --- | --- |
| `-muster-url` | `MUSTER_URL` | (required) | Cluster-private Muster base URL |
| `-token-file` | `SEXTANT_TOKEN_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | Projected ServiceAccount bearer token read fresh on each call |
| `-spool` | `SEXTANT_SPOOL_DIR` | `/var/spool/cs` | Atomic local spool directory for unacknowledged step outcomes |
| `-poll-every` | `SEXTANT_POLL_EVERY` | `2s` | Poll interval when Muster returns `204 No Content` |
| `-fetch-timeout` | `SEXTANT_FETCH_TIMEOUT` | `20s` | Per-item passive HTTPS fetch timeout |
| `-heartbeat-every` | `SEXTANT_HEARTBEAT_EVERY` | `20s` | Lease heartbeat interval while observing an item |
| `-observed-on` | `SEXTANT_OBSERVED_ON` | UTC date of lease `expires` | Optional fixed `YYYY-MM-DD` override for tests |
| `-drain` | (none) | `false` | Exit `0` once `/var/spool/cs` is empty and `/v1/lease` returns `204` |

## Sub-tools

Every sub-tool listed in [`component.json`](component.json) and [`tools/registry.yaml`](tools/registry.yaml) pairs a declarative [`match.yaml`](tools/gov-portal/match.yaml) rule file, deterministic Go extraction logic, and golden `fixtures/`:

| Sub-tool | What it matches | Fixtures |
| --- | --- | --- |
| [`gov-portal`](tools/gov-portal) | Single-form HTML calculators and lookup portals on `.gov` and `.mil` HTTPS hosts | 22 |
| [`saas-marketing`](tools/saas-marketing) | Public commercial landing pages with two-column HTML feature tables (`Stated` inputs/outputs, login gates, pricing, citations) | 6 |
| [`agency-pdf`](tools/agency-pdf) | Downloadable `.gov` and `.mil` procurement PDFs (uncompressed and `/FlateDecode` streams, AcroForm `/Tx`, `/Ch`, `/Btn` fields and `/Ff` flags, `publication_date`) | 7 |
| [`on-demand-browser-job`](tools/on-demand-browser-job) | Rendered DOM and client-side network log envelopes (`sextant-browser-capture/v1`) from JavaScript single-page applications | 6 |

## What the record holds

This is the internal observation record (`sextant observe` prints it). `sextant work` does not send it: it derives the four-key `results.sextant` Assay reads (`category`, `sub_tool`, `observed_on`, `facts`) from it, using the citation and template tables in [`internal/derive`](internal/derive). See `SPEC.md`, "Proposed changes", item 2.

Every field in [`internal/observation/record.go`](internal/observation/record.go) comes from a closed vocabulary, a normalized citation (`FAR`, `DFARS`, `NIST SP`), a validated host name, a `PA-` identifier, or a `YYYY-MM-DD` date:

- `item_id`, `source_url`, `sub_tool`, `observed_on`, and optional `publication_date` (`agency-pdf`)
- `core_job` (`date_calculation`, `amount_calculation`, `rate_calculation`, `eligibility_check`, `record_search`, `compliance_matrix`, `control_mapping`, `score_calculation`, `config_validation`)
- `inputs` (`text`, `number`, `date`, `choice`, `file`, with `required` boolean; `Seen` or `Stated`)
- `outputs` (`computed_value`, `structured_data`, `compliance_report`, `document_export`; `Seen` or `Stated`)
- `login_requirement` (`required`, `not_required`), `tracking` (sorted external host names), `license_or_copyright` (`public_domain`, `copyright_asserted`, `open_license`), `price_model` (`free`, `paid`), `cited_public_rules`, and `offline_behavior` (`works_offline`, `server_round_trip`)

Raw HTML and PDF bytes are wiped in memory immediately after extraction and are never written to `/var/spool/cs` or emitted in logs.

## What works, what does not, and what Sextant needs from others

### What works

- `sextant observe` and `sextant work` compile with `CGO_ENABLED=0` using only the Go standard library (`module github.com/ClearedStack/sextant`).
- Strict Muster `contract/v1` HTTP client and in-memory stand-in ([`internal/muster`](internal/muster)), verified against all 52 saved examples from `ClearedStack/muster` `contract/v1/examples/` in [`testdata/contract/`](testdata/contract).
- Passive public HTTPS fetcher ([`internal/fetch`](internal/fetch)) and RFC 9309 `robots.txt` evaluator ([`internal/robots`](internal/robots)) blocking private, loopback, link-local, CGNAT, multicast, and cluster hosts, stopping cleanly on `401`/`402`/`403` paywalls and CAPTCHA/anti-bot challenges (`E_NEEDS_SUBTOOL`), and enforcing the 2 MiB capture ceiling and 30 MiB pod RAM budget (`GOMEMLIMIT=20MiB`).
- Crash-safe atomic local spool ([`internal/spool`](internal/spool)) using temp-file write, `fsync`, and rename before acknowledging `complete`, `fail`, or `needs_subtool`.
- All four sub-tools (`gov-portal`, `saas-marketing`, `agency-pdf`, `on-demand-browser-job`) with 41 golden test cases, canary leak tests, and sanitized error reasons ([`internal/errcode`](internal/errcode)).
- Hardened `ko` config ([`.ko.yaml`](.ko.yaml)) and Kubernetes manifests ([`deploy/`](deploy)) in namespace `cs-workers` with digest pinning, `runAsNonRoot`, `readOnlyRootFilesystem`, dropped `ALL` capabilities, `arm64` / `NotIn cm3588-plus` node affinity, and default-deny `NetworkPolicy`.

### What does not work yet

- **Live in-cluster headless browser execution:** `tools/on-demand-browser-job` extracts deterministically from `sextant-browser-capture/v1` envelopes and `fetch.Fetcher` detects JavaScript-only SPA shells (`NeedsBrowserJob`), but `sextant work` defaults `BrowserRunner` to `nil` in-cluster and pauses SPA shells with `E_NEEDS_SUBTOOL` (`browser_job_runner_not_configured`). Spawning Kubernetes `batch/v1` `Job` pods directly from Sextant is blocked by Sextant's own `NetworkPolicy` (no Kubernetes API egress) and the absence of a digest-pinned headless browser image in Lazarette.
- **Encrypted PDFs and non-Flate PDF stream filters:** `agency-pdf` pauses encrypted PDFs and unfamiliar AcroForm field types (such as `/Sig`) with `E_NEEDS_SUBTOOL`.
- **Re-observation from `parked` or `linked`:** `SPEC.md` mentions re-queued items from `parked` or `linked`, whereas Muster `contract/v1/CONTRACT.md` only defines `recorded` -> `observed` for Sextant (`needs_subtool` replays return to `recorded`).

### What Sextant needs from other components

1. **Muster (`ClearedStack/muster`):** Merge PR #4 (`stateless-logbook-0010`) so `found` metadata and `core.result` persist across Muster restarts in PostgreSQL 17 (`Logbook`). Sextant already falls back from `found.address` to `results.trawl.address` and tolerates empty `found.source`. If `parked` or `linked` items should re-enter Sextant later, add those transitions explicitly to `contract/v1/CONTRACT.md`.
2. **Assay (`ClearedStack/assay`):** `results.sextant` now has the four keys and 22 pricing facts Assay's decoder reads. Assay must accept the `Designed` tier (its own PR) before the build facts parse, and its `private_by_design` gate must accept `Designed` for `emits_tracking` and `connect_src_self` or items pause there.
3. **Keelworks, Lazarette, and Shipkeeper:** Build `./cmd/sextant` with `ko` (`linux/arm64`), push and sign the image in Lazarette, replace the reproducible binary SHA-256 placeholder in [`deploy/sextant.yaml`](deploy/sextant.yaml) with the pushed OCI manifest digest, and provide a digest-pinned headless browser runner (or controller sidecar) if on-demand SPA rendering is enabled.

## Test-only setting: `SEXTANT_LOCAL_TEST_ORIGIN`

For end-to-end test runs only. Set it to exactly one origin, `http://127.0.0.1:<port>` or `http://[::1]:<port>` (port 1 to 65535, nothing after it), and `sextant work` fetches pages from that origin over plain http, without the public-https and private-address checks. Every other address keeps them. A redirect away from that origin, or into it from anywhere else, is refused. Any other value makes `sextant work` exit 2 with a fixed message that does not repeat the value, and a valid value logs one fixed warning line at startup. It is off when unset or empty. `deploy/` must never set it, and a test fails if it does.

When a step fails (`fail`, for example `E_TIMEOUT`), Muster keeps the item in place and offers it again at once, so `sextant work` waits before the next lease: the poll interval, doubled for each further failure in a row, up to 32 times the poll interval, with no randomness. A completed or paused item resets it.

## In this repository

| Path | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | Component specification and contract v1 alignment notes |
| [`PROMPT.md`](PROMPT.md) | Build prompt and constraints |
| [`component.json`](component.json) | ClearedStack component registry entry |
| [`.ko.yaml`](.ko.yaml) | Reproducible `ko` image build config pinned by Chainguard static SHA-256 digest |
| [`deploy/`](deploy) | Kubernetes `Namespace`, `ServiceAccount`, `Deployment`, and `NetworkPolicy` manifests for `cs-workers` |
| [`cmd/sextant/`](cmd/sextant) | `sextant observe` and `sextant work` CLI entrypoint and invariant tests |
| [`tools/`](tools) | Sub-tool registry (`gov-portal`, `saas-marketing`, `agency-pdf`, `on-demand-browser-job`) and golden fixtures |
| [`internal/`](internal) | `observation`, `derive`, `errcode`, `muster`, `fetch`, `robots`, `spool`, `worker`, `markup`, `rulefile`, `subtool` |
| [`testdata/contract/`](testdata/contract) | Saved contract v1 examples from `ClearedStack/muster` replayed in `internal/muster` |

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
