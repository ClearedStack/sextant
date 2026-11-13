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

> **Status: design.** Nothing is built yet. This repository holds the specification and the prompt for building it, and no code.

Sextant is stage 02 of the ClearedStack pipeline. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Sextant is meant to be understood and used without the rest.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Sextant does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`brand/`](brand) | The Sextant logo and mark, for light and dark backgrounds. |

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
