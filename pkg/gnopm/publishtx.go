package gnopm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The transaction document: one signature for a whole deploy.
//
// The gnokey command list is one handoff shape and the lowest common
// denominator. It assumes the signer is a CLI on the same machine, and it is
// not atomic: `set -e` stops at the first failure, which leaves the
// dependencies up and the thing that needed them not, a state neither the tree
// nor the chain describes. N packages is also N password prompts.
//
// A tm2 transaction carries a list of messages, not one (Tx.Msgs []Msg in
// tm2/pkg/std/tx.go), and `gnokey sign` signs the document without caring how
// many messages are in it. So the whole deploy can be one document, signed
// once, broadcast once: all the packages land or none do.
//
// This is not gnopm signing anything (architecture decision 3). It writes an
// unsigned document and prints the two commands that sign and broadcast it.
//
// The shape is copied from an upstream fixture that is signed and broadcast
// against a real gnoland in CI, gno.land/pkg/integration/testdata/
// addpkg_multi_msg.txtar, which is a two-message MsgAddPackage tx. Everything
// amino leaves optional is left out rather than guessed at.

const (
	// addPackageType is the amino type URL for vm.MsgAddPackage, registered as
	// "m_addpkg" in gno.land/pkg/sdk/vm/package.go.
	addPackageType = "/vm.m_addpkg"

	// maxTxBytes is the consensus limit on one transaction, read from
	// gnoland-1's /consensus_params on 2026-09-22: MaxTxBytes 1,000,000 with a
	// block MaxGas of 3,000,000,000. Size binds first: a package of ~30 KB of
	// source is ~53 M gas, so roughly 30 packages fit in one signature on gas
	// and fewer on size.
	maxTxBytes = 1_000_000

	// txBytesHeadroom is what is left for the signature, the memo and amino's
	// own framing, which are not in the document gnopm writes. A signature is
	// a few hundred bytes; 5% of the limit is generous and costs one extra
	// batch only on a deploy already near the ceiling.
	txBytesHeadroom = maxTxBytes / 20
)

// TxDocument is the unsigned transaction, in the amino JSON shape `gnokey
// sign -tx-path` reads.
type TxDocument struct {
	Msgs []AddPackageMsg `json:"msg"`
	Fee  TxFee           `json:"fee"`
	Memo string          `json:"memo"`
}

// TxFee is std.Fee. Both numbers are strings in amino JSON.
type TxFee struct {
	GasWanted string `json:"gas_wanted"`
	GasFee    string `json:"gas_fee"`
}

// AddPackageMsg is vm.MsgAddPackage.
type AddPackageMsg struct {
	Type       string    `json:"@type"`
	Creator    string    `json:"creator"`
	Package    *TxMemPkg `json:"package"`
	MaxDeposit string    `json:"max_deposit,omitempty"`
}

// TxMemPkg is std.MemPackage, with only the fields a deploy needs.
type TxMemPkg struct {
	Name  string      `json:"name"`
	Path  string      `json:"path"`
	Files []TxMemFile `json:"files"`
}

// TxMemFile is std.MemFile.
type TxMemFile struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

// JSON renders the document the way gnokey reads it.
func (d *TxDocument) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// AddPackageFor builds one MsgAddPackage from a package directory.
//
// The file set mirrors gno's own ReadMemPackage with MPUserAll
// (gnovm/pkg/gnolang/mempackage.go), which is what `gnokey maketx addpkg`
// uses: the package directory's allowed extensions and whole-name matches,
// hidden files and subdirectories excluded, plus the *_filetest.gno files the
// toolchain folds in from filetests/.
func AddPackageFor(dir, module, creator string, deposit int64) (AddPackageMsg, error) {
	files, err := payloadFiles(dir)
	if err != nil {
		return AddPackageMsg{}, err
	}
	if len(files) == 0 {
		return AddPackageMsg{}, fmt.Errorf("%s has no uploadable files", dir)
	}
	pkg := &TxMemPkg{Path: module}
	for _, f := range files {
		b, err := os.ReadFile(f.Path)
		if err != nil {
			return AddPackageMsg{}, err
		}
		body := string(b)
		pkg.Files = append(pkg.Files, TxMemFile{Name: f.Name, Body: body})
		// The package name is the `package` clause, and it comes from a
		// production file: a _test.gno may declare `foo_test` and a filetest
		// may declare anything at all, so either would name the package wrong.
		if pkg.Name == "" && isProdGno(f.Name) {
			pkg.Name = packageClause(body)
		}
	}
	if pkg.Name == "" {
		return AddPackageMsg{}, fmt.Errorf("%s: no production .gno file declares a package name, so there is nothing to deploy", dir)
	}
	msg := AddPackageMsg{Type: addPackageType, Creator: creator, Package: pkg}
	if deposit > 0 {
		msg.MaxDeposit = strconv.FormatInt(deposit, 10) + "ugnot"
	}
	return msg, nil
}

var packageClauseRe = regexp.MustCompile(`(?m)^\s*package\s+([A-Za-z_][A-Za-z0-9_]*)`)

func packageClause(body string) string {
	m := packageClauseRe.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// batchDocuments splits messages into documents that each fit under the
// consensus transaction limit, keeping the given order.
//
// Order is gnopm's problem once it batches, which is the cost of doing it: the
// input is already topological, so cutting it into contiguous runs keeps every
// dependency ahead of its dependents both within a batch and across them.
func batchDocuments(msgs []AddPackageMsg, gas func(AddPackageMsg) int64) ([]*TxDocument, error) {
	var out []*TxDocument
	cur := &TxDocument{}
	var curGas int64
	flush := func() error {
		if len(cur.Msgs) == 0 {
			return nil
		}
		cur.Fee = feeFor(curGas)
		out = append(out, cur)
		cur, curGas = &TxDocument{}, 0
		return nil
	}
	for _, m := range msgs {
		probe := &TxDocument{Msgs: append(append([]AddPackageMsg{}, cur.Msgs...), m)}
		b, err := probe.JSON()
		if err != nil {
			return nil, err
		}
		if len(b) > maxTxBytes-txBytesHeadroom {
			if len(cur.Msgs) == 0 {
				// One package larger than a whole transaction. Nothing gnopm
				// can do about that, and saying so beats writing a document
				// the mempool will reject.
				return nil, fmt.Errorf("%s alone is %d bytes as a transaction, over the %d limit: "+
					"it cannot be deployed in one message", m.Package.Path, len(b), maxTxBytes)
			}
			if err := flush(); err != nil {
				return nil, err
			}
		}
		cur.Msgs = append(cur.Msgs, m)
		curGas += gas(m)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

func feeFor(gas int64) TxFee {
	return TxFee{GasWanted: strconv.FormatInt(gas, 10), GasFee: FeeFor(gas)}
}

// writeDocuments writes one document, or a numbered set when it had to be
// split, and returns the paths in order.
func writeDocuments(path string, docs []*TxDocument) ([]string, error) {
	var out []string
	for i, d := range docs {
		p := path
		if len(docs) > 1 {
			ext := filepath.Ext(path)
			p = strings.TrimSuffix(path, ext) + fmt.Sprintf(".%d", i+1) + ext
		}
		b, err := d.JSON()
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Account is what a chain says about the signer, and it is part of the
// signature rather than of the document.
type Account struct {
	Number   uint64
	Sequence uint64
}

// ReadAccount reads account number and sequence from auth/accounts/<addr>.
//
// Both go into `gnokey sign` and both are covered by the signature, so a
// document is only valid until that account signs anything else. Saying the
// numbers without saying that would be worse than not saying them.
func ReadAccount(c *Chain, addr string) (Account, error) {
	raw, err := c.ABCIQuery("auth/accounts/"+addr, "")
	if err != nil {
		return Account{}, err
	}
	var wrapper struct {
		BaseAccount struct {
			Address       string `json:"address"`
			AccountNumber string `json:"account_number"`
			Sequence      string `json:"sequence"`
		} `json:"BaseAccount"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapper); err != nil {
		return Account{}, fmt.Errorf("reading %s: %w", addr, err)
	}
	if wrapper.BaseAccount.Address == "" {
		return Account{}, fmt.Errorf("%s does not exist on this chain yet: it has never received funds, so it has no account number", addr)
	}
	n, err := strconv.ParseUint(wrapper.BaseAccount.AccountNumber, 10, 64)
	if err != nil {
		return Account{}, fmt.Errorf("account number for %s: %w", addr, err)
	}
	s, err := strconv.ParseUint(wrapper.BaseAccount.Sequence, 10, 64)
	if err != nil {
		return Account{}, fmt.Errorf("sequence for %s: %w", addr, err)
	}
	return Account{Number: n, Sequence: s}, nil
}

// bech32ish reports whether a string looks like a gno address rather than a
// keybase name. Deliberately shallow: gnopm is not a wallet and does not
// validate checksums, it only has to tell "alice" from "g1...".
func bech32ish(s string) bool {
	return strings.HasPrefix(s, "g1") && len(s) == 40
}
