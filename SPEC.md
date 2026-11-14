# Sextant specification

> Technical specification for the deterministic Sextant worker that inspects public pages, spawns on-demand browser Jobs, and seals raw captures behind the clean-room wall.

**Status: design.** This is a specification. It describes how Sextant is meant to work, not something that exists yet.

The same text is published at [clearedstack.com/platform/sextant/](https://clearedstack.com/platform/sextant/). To change the specification, change it here and say why.

## Overview & Role

Sextant (`v0.1.0-draft`) is the deterministic observation worker in the ClearedStack v3.1 pipeline, replacing the original Stage 3 (Observe). Implemented as a Go service in the Sextant repository under the MIT License, it inspects public web pages, `.gov` portals, and agency PDFs using repeatable extractors and zero runtime AI.

Its core responsibilities are:

- **Public-only inspection:** Fetch open-web vendor marketing pages, government portals, and procurement PDFs without logging in, bypassing paywalls, or submitting forms.
- **Deterministic fact extraction:** Produce strict, schema-validated JSON capturing a candidate tool's core job, inputs, outputs, login requirement, tracking profile, license or copyright, price model, cited public rule, observation date, and confidence tier (`Seen`, `Stated`, or `Unknown`).
- **Clean-room sealing:** Seal all raw HTML, DOM snapshots, screenshots, and downloaded PDFs behind the clean-room wall so downstream specification and build workers (Waybill and Moldloft) never see vendor markup, stylesheets, or client-side scripts.

## Runtime & Resource Budget

Sextant runs in the `cs-workers` Kubernetes namespace under the **Standard (Workers)** priority class, with ephemeral headless browser `Job` pods scheduled under **Low (Ephemeral Jobs)**.

| Attribute | Value |
| --- | --- |
| **Worker ID / Repo** | Sextant (`sextant`) |
| **Namespace** | `cs-workers` |
| **Kind / Version** | `worker` · `v0.1.0-draft` |
| **Baseline RAM Budget** | `30 MiB` (always-on Go HTTP pod) |
| **Ephemeral Footprint** | On-demand headless browser `Job` pods (created and deleted per JS-heavy target) |
| **Runtime AI Calls** | `0` (100% deterministic Go + YAML rules) |
| **Local Spool** | `/var/spool/cs` (persists completed extraction payloads across Muster restarts) |
| **Build & Deploy** | Built with `ko` (no Docker) by Keelworks, signed with `cosign` in Lazarette, deployed by Shipkeeper via Server-Side Apply |

Keeping headless browsers out of the always-on worker pool preserves the ~300 MiB baseline worker footprint so the full 17-pod ClearedStack platform fits on a single 4 GiB `k3s` node.

## Interfaces & State Transitions

Sextant communicates exclusively with Muster in the `clearedstack` namespace over cluster-private HTTP authenticated via its Kubernetes ServiceAccount (`CONTRACT_VERSION=v1`). It never connects directly to Logbook, Plainforge, or Lazarette.

- **Queue consumption:** Polls Muster using `lease`, `heartbeat`, `complete`, `fail`, and `needs_subtool`, draining priority lanes in strict order: `urgent` → `normal` → `backfill`.
- **Inbound states:**
  - `recorded`: Newly deduplicated `PA-XXXX` candidates from Trawl.
  - `observed` (re-queued): Items returned from `needs_subtool` replay after a sub-tool fix ships, `parked` items whose blocker cleared in Tidegauge, or `linked` items flagged for drift.
- **Outbound transitions:**
  - `observed`: Extraction succeeded; sanitized structured JSON facts are stored in `core.*` and raw captures remain sealed behind the clean-room wall.
  - `needs_subtool`: Fail-closed pause (`E_NEEDS_SUBTOOL`) when no registered sub-tool matches the target layout.
- **Spool durability:** Extracted payloads are written to `/var/spool/cs` before acknowledging `complete` to Muster, preventing lost work during control-plane restarts.

## Sub-Tools & Failure Modes

Per-site and per-format variations live inside the Sextant repository under `tools/<case>/`, where each sub-tool pairs a `match.yaml` rule, deterministic `extract.go` logic, and golden `fixtures/`.

| Sub-Tool | Target Pattern & Behavior |
| --- | --- |
| `saas-marketing` | Public vendor landing pages and feature tables; extracts stated inputs, outputs, pricing model, and login gates. |
| `gov-portal` | Federal `.gov` and `.mil` portals and calculators; captures cited FAR, DFARS, or NIST clauses and offline behavior. |
| `agency-pdf` | Downloadable procurement PDFs, worksheets, and instructions; extracts form fields, rules, and publication dates. |
| `on-demand-browser-job` | Ephemeral headless browser `Job` spawned only for JavaScript-heavy single-page apps to render the DOM and inspect client-side network calls using synthetic test inputs. |
| `<vendor-slug>` | Dedicated extractors for specific vendors whose public page structure differs from standard layouts. |

### Sanitized Failure Modes

Sextant never guesses when an extraction fails or encounters an unknown layout:

| Error Code | Trigger | Platform Behavior |
| --- | --- | --- |
| `E_NEEDS_SUBTOOL` | Page layout, table, or PDF format matches no registered `tools/<case>/` sub-tool. | Fails closed to `needs_subtool`. Bellbuoy opens a deduplicated task in Sextant on Plainforge; once a new sub-tool and fixture deploy, Muster auto-replays paused items. |
| `E_TIMEOUT` | Upstream public page, PDF download, or ephemeral browser `Job` exceeds deadline. | Item remains safely queued for bounded retry; Shipkeeper strips cluster internals before surfacing status. |
| `E_PARSE` | Malformed HTML, corrupted PDF, or schema validation failure on extracted JSON. | Halts item cleanly and reports fingerprinted error via Bellbuoy without emitting partial data. |

## Security & Clean-Room Boundaries

- **Passive public boundaries:** Sextant never creates accounts, starts free trials, submits lead-capture forms, enters real contracting data, or circumvents paywalls, `robots.txt`, or CAPTCHAs. If a tool sits behind a login or anti-bot gate, observation stops at the public surface and records vendor claims as `Stated` rather than `Seen`.
- **Strict clean-room wall:** Raw HTML, DOM trees, stylesheets, client-side scripts, and binary PDFs are sealed at Sextant and never cross into Waybill or Moldloft. Muster enforces this contract boundary by rejecting any build payload containing raw observation artifacts.
- **Pod hardening & network policy:** Runs as an unprivileged non-root user with dropped Linux capabilities, no privilege escalation, and a read-only root filesystem (writable only at `/var/spool/cs`). Default-deny Kubernetes `NetworkPolicy` allows outbound HTTP/HTTPS for public fetches and cluster-private HTTP to Muster, while blocking direct access to Logbook, Plainforge, and Lazarette.

## Proposed changes & contract v1 alignment

The implementation in this repository resolves four open details in the draft specification above without inventing Muster calls, fields, or states:

1. **Inbound state in Muster contract v1 (`recorded` -> `observed`):** `ClearedStack/muster` `contract/v1/CONTRACT.md` assigns `sextant` exclusively to `recorded` -> `observed` (queued next for `assay`). When Sextant pauses an item via `POST /v1/needs_subtool`, Muster records `paused_from = recorded` and returns the item to `recorded` when Shipkeeper reports a changed Sextant image digest. Re-queuing from `parked` or `linked` back into Sextant is not yet in `CONTRACT.md` and is not assumed here.
2. **Closed observation record schema (`results.sextant`):** Completed observations pass a validated `observation.Record` object as `result` on `POST /v1/complete` (`item_id`, `source_url`, `sub_tool`, `observed_on`, optional `publication_date` on `agency-pdf`, and tiered facts `core_job`, `inputs`, `outputs`, `login_requirement`, `tracking`, `license_or_copyright`, `price_model`, `cited_public_rules`, `offline_behavior`). Raw HTML, DOM snapshots, and PDF bytes are wiped in memory immediately after extraction and never written to `/var/spool/cs`.
3. **Ephemeral browser `Job` boundary:** Creating `batch/v1` `Job` pods from inside Sextant would require Kubernetes API egress and `jobs/create` RBAC, conflicting with Sextant's least-privilege `NetworkPolicy` (DNS, Muster, and public HTTPS only) and requiring a digest-pinned headless browser image in Lazarette. `tools/on-demand-browser-job` therefore consumes a strict capture envelope (`sextant-browser-capture/v1`) via `fetch.BrowserRunner`; when no runner is configured in-cluster, JavaScript-only SPA shells fail closed with `E_NEEDS_SUBTOOL` (`browser_job_runner_not_configured`).
4. **HTTPS-only external egress:** Because Sextant refuses plain `http://` targets, `deploy/networkpolicy.yaml` opens public egress on TCP 443 only while blocking private, loopback, CGNAT, link-local, multicast, and LAN CIDRs.

## Related pages

- [The pipeline and its ten workers](https://clearedstack.com/platform/pipeline/)
- [Clean room and credit](https://clearedstack.com/platform/clean-room-and-credit/)
- [An item’s life: fourteen states and three lanes](https://clearedstack.com/platform/item-lifecycle/)
- [Cluster architecture](https://clearedstack.com/platform/cluster-architecture/)
