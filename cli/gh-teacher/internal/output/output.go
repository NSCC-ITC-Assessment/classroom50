// Package output holds the CLI-wide presentation helpers that are not
// domain logic — currently the shared JSON encoder used for every
// `--json` view and every config-repo file gh-teacher writes.
//
// It exists so the per-domain files depend on a small, named seam
// instead of a flat package main helper, and so the JSON byte contract
// (the agent-consumable `--json` output and the hand-editable on-disk
// files) lives in one documented place. It depends only on the standard
// library.
package output

import (
	"bytes"
	"encoding/json"
)

// JSONPretty marshals v with 2-space indent and a trailing newline so
// teachers can inspect/hand-edit the files. EscapeHTML is off to keep
// `<`/`>` literal in URLs. This is the single encoder behind both the
// `--json` command output and the config-repo files written to
// <org>/classroom50, so its byte shape is a contract: changing the
// indent, HTML-escaping, or trailing newline is a breaking change.
func JSONPretty(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
