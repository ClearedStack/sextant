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

> **Status: design.** One piece is built: the `gov-portal` sub-tool, run as a command against a saved page. The worker described in the specification (the Muster queue, fetching, the spool, the browser job, the other sub-tools, the image and its deployment) is not built. See [What works today](#what-works-today).

Sextant is stage 02 of the ClearedStack pipeline. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Sextant is meant to be understood and used without the rest.

## What works today

One command, `sextant observe`. You give it a page you have already saved, the address it came from and the date you saved it. It prints the structured observation record as JSON.

```sh
go build -o sextant ./cmd/sextant

./sextant observe \
  -item PA-0001 \
  -url https://calc.example-agency.gov/due-date \
  -page tools/gov-portal/fixtures/observed-calculator/page.html \
  -observed-on 2026-01-15
```

It needs Go 1.24 or later and nothing else: there are no dependencies outside the standard library. The command opens no network connection, reads no clock and writes no file. `-page -` reads the page from standard input.

**What it recognises.** One sub-tool, `gov-portal`: an HTML page served over `https` from a host ending in `.gov` or `.mil` that has exactly one form (site-search forms are set aside). The rule and every word list are in [`tools/gov-portal/match.yaml`](tools/gov-portal/match.yaml).

**What the record holds.** The item, the address, the sub-tool and the date, then nine facts, each with its own tier:

| Field | What it is | How it is found |
| --- | --- | --- |
| `core_job` | One of four fixed job names | Phrases in the page title and main heading: `Stated` |
| `inputs` | The kind of each field the form asks for (`text`, `number`, `date`, `choice`, `file`) and whether it is required | The form's controls: `Seen` |
| `outputs` | One `computed_value` per `<output>` element | The page's markup: `Seen` |
| `login_requirement` | `required` or `not_required` | A password field on the form: `Seen`. Otherwise a phrase: `Stated` |
| `tracking` | Hosts, other than the page's own, that the saved markup loads scripts, images, frames or stylesheets from | The page's markup: `Seen` |
| `license_or_copyright` | `public_domain`, `copyright_asserted` or `open_license` | A phrase: `Stated` |
| `price_model` | `free` or `paid` | A phrase: `Stated` |
| `cited_public_rules` | FAR, DFARS and NIST SP citations, written one standard way and sorted | Patterns over the visible text: `Seen` |
| `offline_behavior` | `works_offline` or `server_round_trip` | A form that names a server address to submit to: `Seen`. Otherwise a phrase: `Stated` |

A fact no rule finds is `Unknown` with no value. Every value is a word from a fixed list, a citation in standard form, a host name or a date. No field can hold text lifted from the page, and a test plants marker words throughout the saved pages and checks none of them reach the output.

**When it will not answer.** The command never prints part of a record. It prints a small failure object and leaves with a status that says which kind:

| Exit status | Meaning |
| --- | --- |
| `0` | A record was printed. |
| `2` | The command was used wrongly. Nothing is printed on standard output. |
| `10` | `E_NEEDS_SUBTOOL`. The page is valid and unfamiliar: no sub-tool matches it, a form control is not in the rule file, or two rules in one word list disagree. The item would be paused as `needs_subtool`. |
| `11` | `E_PARSE`. The page is broken: empty, not UTF-8, cut off mid-tag, a form that never closes, or a record that fails its own schema check. |
| `12` | `E_TIMEOUT`. Defined, and nothing built so far can raise it. |

```json
{
  "item_id": "PA-0101",
  "outcome": "needs_subtool",
  "error": {
    "code": "E_NEEDS_SUBTOOL",
    "reason": "no_matching_sub_tool"
  }
}
```

The reason is always one of a fixed list of words in [`internal/errcode`](internal/errcode/errcode.go), so a failure cannot name a machine, an address or a credential.

**Tests.** `go test ./...` runs everything and needs no network. Each folder under [`tools/gov-portal/fixtures/`](tools/gov-portal/fixtures) is a saved example: a made-up page, the arguments (`case.json`) and the exact output expected (`expected.json`). There are 5 pages that are observed, 11 that are paused and 6 that are broken. After a deliberate change to a rule, `go test ./cmd/sextant -update` rewrites the expected outputs; read the difference before committing it.

**Limits worth knowing.**

- Only the saved markup is read. Anything a script would add in a browser (fields, outputs, trackers) is not seen.
- A host counts as "other" if it differs at all from the page's host, so a sibling subdomain is listed too.
- Pages over 2 MiB are paused. On a page of that size the command peaked at about 18 MiB of memory when measured by hand; that is a measurement of this one-shot command, not of the long-running worker, and no test enforces it.
- Each value a rule can produce has a saved example. Each individual phrase does not.

## Not built yet

Everything else in [`SPEC.md`](SPEC.md):

- The always-on worker: leasing from Muster (`lease`, `heartbeat`, `complete`, `fail`, `needs_subtool`), the three lanes, and the in-memory stand-in for Muster.
- Fetching pages over HTTPS, deadlines and `E_TIMEOUT`, `robots.txt`.
- The spool at `/var/spool/cs` and sealing of raw captures.
- The sub-tools `saas-marketing`, `agency-pdf` and `on-demand-browser-job`, and vendor-specific ones.
- The `ko` image, the Kubernetes manifests, pod hardening and the network policy.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Sextant does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`brand/`](brand) | The Sextant logo and mark, for light and dark backgrounds. |
| [`cmd/sextant/`](cmd/sextant) | The command. |
| [`tools/`](tools) | The sub-tools. `registry.yaml` lists them; each has a folder with `match.yaml`, `extract.go` and `fixtures/`. |
| [`internal/`](internal) | The record and its schema check, the error codes, the HTML reader and the rule-file reader. |

## At a glance

| | |
| --- | --- |
| **Stage** | 02 of 10 in the pipeline |
| **Formerly** | `cs-02-observe`, then Spyglass |
| **Namespace** | `cs-workers` |
| **Container image** | `lazarette.clearedstack.svc.cluster.local:5000/sextant` |
| **Memory budget** | 30 MiB |
| **Depends on** | [Muster](https://github.com/ClearedStack/muster) |
| **Depended on by** | Nothing else in the platform |
| **Rules for particular cases** | `saas-marketing`, `gov-portal`, `agency-pdf`, `on-demand-browser-job` |

### Boundaries

- Leases work from Muster over cluster-private HTTP
- Fetches public pages over HTTPS, and starts a temporary browser job when a page needs one

## Names

`sextant` is this component’s identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component’s entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
