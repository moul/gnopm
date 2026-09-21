# CLAUDE.md

This repository has a single, authoritative guide: **[AGENTS.md](./AGENTS.md)**.
Read it before doing anything. The essentials:

- **Logic belongs in `pkg/`.** `main.go` is the entry point and nothing else,
  so other programs can use the packages without shelling out.
  `pkg/gnomodlock` stays importable on its own, standard library only.
- **Detect, do not ask.** A required flag is friction paid on every invocation
  forever. Work it out instead.
- **Standard library only**, unless you argue for the dependency first.
- **`scripts/demo.sh` is the integration test**, and it runs under `go test`.
  Every bug gets a regression test in the same change as the fix.
- **Docs, screenshots and the demo repository are part of the change.**
  `./scripts/screenshots.sh` regenerates the terminal images from real output;
  never hand-write them.
- **Errors say what to run next.** Data on stdout, diagnostics on stderr,
  silent when there is nothing to report.
- **No em dashes.** Conventional single-line commits.

Direction and the roadmap are in issue #2. Do not restate them here.
