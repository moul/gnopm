# AGENTS.md

Read **[CONTRIBUTING.md](./CONTRIBUTING.md)** first; this file does not repeat it.

The four things most often got wrong:

1. **Logic belongs in `pkg/`.** `main.go` is the entry point, nothing else.
   `pkg/gnomodlock` stays independently importable, standard library only.
2. **Detect, do not ask.** A required flag is friction paid forever. Work it
   out; if you genuinely cannot, say so in the pull request.
3. **Never hand-write output into docs.** `./scripts/screenshots.sh` captures
   real runs. An image that is not a real capture is wrong within a week.
4. **A fix without a regression test is half a fix.** Say in the test comment
   why existing tests could not see it.

Direction and roadmap: issue #2.
