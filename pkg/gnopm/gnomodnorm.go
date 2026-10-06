package gnopm

import "strings"

// Comparing a gnomod.toml against the copy the chain holds.
//
// The chain does not store the bytes it was sent. It REWRITES the manifest:
// comments and blank lines are gone, and an [addpkg] table it authors itself is
// appended, recording who created the package, at what height, and under what
// deposit ceiling. For gno.land/r/moul/home, sent as
//
//	module = "gno.land/r/moul/home"
//	gno = "0.9"
//
//	# Redeployable by its creator. See README.md § "Why private".
//	# …six more lines of comment…
//	private = true
//
// what comes back out of vm/qfile is
//
//	module = "gno.land/r/moul/home"
//	gno = "0.9"
//	private = true
//
//	[addpkg]
//	  creator = "g1manfred47kzduec920z88wfr64ylksmdcedlf5"
//	  height = 396749
//	  max_deposit = "17000000ugnot"
//
// A byte comparison therefore reports this file as differing FOREVER, for every
// package, including one redeployed thirty seconds ago. That is not a cosmetic
// false positive: -republish exists partly to answer "there is nothing to send",
// and a check that can never say so is a check that pushes somebody into a
// redeploy that wipes realm state for no change at all. Found on the first real
// use, against a realm whose eight slots had just been restored by hand.
//
// So the comparison is semantic for this one file: what the repo ASKED for,
// against what the chain RECORDED, ignoring what only the chain can write.

// normalizeGnomod reduces a gnomod.toml to the lines that both sides can hold.
//
// Dropped: blank lines, whole-line comments, and the entire [addpkg] and
// [source] tables. [source] is the one gnopm itself writes into the uploaded
// copy and never into the tree (source.go), and its revision moves with every
// commit: comparing it would make each commit look like a reason to redeploy.
// Kept, with surrounding whitespace trimmed: everything else, in order.
//
// Inline trailing comments are deliberately NOT stripped. Doing it correctly
// means knowing whether the '#' is inside a quoted value, which is a TOML
// parser, and doing it incorrectly would silently drop half of a legitimate
// value. The chain has not been observed to produce one, so the case does not
// arise; if it ever does, it shows up as a difference rather than as a value
// quietly mangled, which is the right way round.
func normalizeGnomod(body string) string {
	var out []string
	skipping := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		// A table header ends whatever table preceded it, so a dropped table
		// is skipped up to the NEXT header rather than to the end of the file:
		// a manifest may legitimately carry a table after it.
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			skipping = t == "[addpkg]" || t == "[source]"
			if skipping {
				continue
			}
		}
		if skipping {
			continue
		}
		out = append(out, t)
	}
	return strings.Join(out, "\n")
}

// normalizeForCompare applies the per-file rule before two copies are compared.
//
// Only gnomod.toml has one. Every other file in a package travels and comes back
// byte for byte, and inventing a normalization for source would be the opposite
// mistake: it would hide a real difference in the files that matter most.
func normalizeForCompare(name, body string) string {
	if name == "gnomod.toml" {
		return normalizeGnomod(body)
	}
	return body
}
