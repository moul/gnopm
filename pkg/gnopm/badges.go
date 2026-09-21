package gnopm

import (
	"fmt"
	"net/url"
	"strings"
)

// Badges emits shields.io badges describing the workspace.
//
// Generated rather than hand-written because a hand-written badge is a claim
// nobody re-checks: "12 packages" stays on a README long after there are 30.
// These come from the lock, so they are as current as the last run.
func Badges(e *Env, asJSON bool) error {
	lock, err := readLock(e.Root)
	if err != nil {
		return err
	}
	pkgs, err := scanPackages(e.Root)
	if err != nil {
		return err
	}
	pinned := len(materializedEntries(lock))

	type badge struct {
		Label, Message, Color, Alt string
	}
	bs := []badge{
		{"packages", fmt.Sprint(len(pkgs)), "blue", "packages in this workspace"},
		{"modules", fmt.Sprint(len(lock.Modules)), "blue", "resolvable module paths"},
		{"pinned", fmt.Sprint(pinned), pick(pinned > 0, "8957e5", "lightgrey"), "versions kept in the lock"},
		{"gnopm", "managed", "7ee787", "managed by gnopm"},
	}

	if asJSON {
		// The shields endpoint shape, so a repository can host these and have
		// badges that update without regenerating any markdown.
		out := make([]map[string]any, 0, len(bs))
		for _, b := range bs {
			out = append(out, map[string]any{
				"schemaVersion": 1, "label": b.Label, "message": b.Message, "color": b.Color,
			})
		}
		return e.writeJSON(out)
	}
	var sb strings.Builder
	for _, b := range bs {
		u := fmt.Sprintf("https://img.shields.io/badge/%s-%s-%s",
			esc(b.Label), esc(b.Message), b.Color)
		sb.WriteString(fmt.Sprintf("[![%s](%s)](https://github.com/moul/gnopm) ", b.Alt, u))
	}
	fmt.Fprintln(e.Out, strings.TrimSpace(sb.String()))
	return nil
}

// esc applies shields.io's escaping: a literal dash doubles, and the rest is
// ordinary URL escaping.
func esc(s string) string {
	return url.PathEscape(strings.ReplaceAll(s, "-", "--"))
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
