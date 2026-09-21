# AGENTS.md

Read **[CONTRIBUTING.md](./CONTRIBUTING.md)** first. It is the authoritative
guide and this file does not repeat it.

The four things most often got wrong here:

1. **Logic belongs in `pkg/`.** `main.go` is the entry point and nothing else,
   so other programs can use the packages without shelling out.
   `pkg/gnomodlock` stays importable on its own, standard library only.
2. **Detect, do not ask.** A required flag is friction paid on every
   invocation forever. Work it out instead, and say so in the pull request if
   you genuinely cannot.
3. **Never hand-write output into docs.** The terminal images come from
   `./scripts/screenshots.sh`, which captures real runs. An image that is not a
   real capture is wrong within a week.
4. **A bug fix without a regression test is half a fix**, and the test comment
   should say why the existing tests could not see it.

Direction and the roadmap are in issue #2. Do not restate them here.
