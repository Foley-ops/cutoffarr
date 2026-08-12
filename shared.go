package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// errNullTagElement marks decodeTagIDs's new failure mode specifically (a
// null element inside an otherwise well-formed tags array), distinct from
// "the whole value isn't a JSON array of ids at all" (a plain json.Unmarshal
// error), so callers that already have their own wording for each case (see
// writer.go's pre-write tag re-check) can tell them apart with errors.Is
// instead of parsing error text.
var errNullTagElement = errors.New("tag array contains a JSON null element")

// shared.go holds helpers used by more than one *arr engine's code
// (Radarr's decision/write path, Sonarr's season decision path introduced in
// Phase 6) so that a rule needed by both never grows a second, drifting copy.
// Per the Phase 6 controller resolutions: containsIntID (writer.go) and
// containsTag (decision.go) used to be two copies of the same loop; this
// file is now the one place that loop is written, and Sonarr's season
// evaluation (sonarr.go) reuses containsTag rather than adding a third.

// containsIntID reports whether id is present in ids. The single shared
// implementation behind containsTag below and writer.go's pre-write
// exclusion-tag re-check, which already knows "tags present" from
// "tags absent" before it ever gets here and so only needs the plain-slice
// form.
func containsIntID(ids []int, id int) bool {
	for _, i := range ids {
		if i == id {
			return true
		}
	}
	return false
}

// containsTag reports whether id is present in tags. A nil tags pointer (the
// "tags" key entirely absent from the JSON) is treated as not containing
// anything, same as a present-but-empty tags list — the caller (Radarr's
// evaluateMovie, Sonarr's evaluateSeries) is responsible for treating
// "absent" itself as untrusted input when the exclusion tag is active; this
// function only ever answers "is the id actually in the list", never "was
// the list observed at all".
func containsTag(tags *[]int, id int) bool {
	if tags == nil {
		return false
	}
	return containsIntID(*tags, id)
}

// decodeTagIDs decodes a JSON array of tag ids the same way a plain []int
// destination would, EXCEPT that it refuses to silently accept a null array
// element (e.g. "tags": [3, null, 9]) the way a plain []int decode does.
//
// This closes a corner none of the project's existing tag-handling code
// caught (REVIEW FIX, Phase 6, controller-mandated, carried forward from the
// phase-5 branch review): encoding/json decodes a JSON null value into a
// non-pointer destination by leaving it at its zero value, with NO error at
// all — for a []int element that means a null array entry silently becomes
// tag id 0, indistinguishable from a genuine "tags": [..., 0, ...] and
// corrupting the one field every exclusion check in this project depends on.
// This is the same class of trap writer.go's pre-write re-check already
// guards against for the whole "tags" key being JSON null (see
// errTagsUnverifiable's doc comment) — this closes it for an individual
// element within an otherwise well-formed array. Decoding through []*int
// first makes a null element visible as a nil pointer instead of silently
// guessing zero, and this function reports an error instead of proceeding.
//
// Callers are responsible for the "key entirely absent" and "key present as
// JSON null" cases themselves (both are legitimate, common shapes with their
// own meaning — absent tags vs. explicit null tags — and neither is this
// function's job to interpret); it is only ever handed a present, non-null
// "tags" value to decode.
//
// The nil-vs-non-nil distinction a plain []int decode makes between "tags":
// null (nil slice) and "tags": [] (non-nil, empty slice) is preserved
// exactly: a JSON null value decodes []*int to a nil slice with no error
// (confirmed empirically, same as a plain []int would), which this function
// passes straight through as (nil, nil) rather than folding it into the
// zero-length non-nil slice a naive `make([]int, len(ptrs))` would produce
// regardless of ptrs being nil or merely empty.
func decodeTagIDs(raw []byte) ([]int, error) {
	var ptrs []*int
	if err := json.Unmarshal(raw, &ptrs); err != nil {
		return nil, err
	}
	if ptrs == nil {
		return nil, nil
	}
	ids := make([]int, len(ptrs))
	for i, p := range ptrs {
		if p == nil {
			return nil, fmt.Errorf("tag id at index %d is JSON null: %w", i, errNullTagElement)
		}
		ids[i] = *p
	}
	return ids, nil
}

// rawObjectField extracts the raw bytes of one key from a raw JSON object,
// without decoding the rest of the object. Used to independently
// re-inspect a single field (namely "tags", via decodeTagIDs above) after
// the object's typed struct decode already succeeded through the ordinary
// *[]int-shaped field, which cannot itself distinguish a null array element
// from a real zero. A raw value that is not a JSON object at all, or does
// not carry key, reports found=false: callers treat that as "nothing more
// to check here" rather than as a new error, since the earlier typed decode
// is what is authoritative for "is this a well-formed object".
//
// The lookup deliberately mirrors encoding/json's own key matching — exact
// match first, then a case-insensitive scan (REVIEW FIX, Phase 6 branch
// review, carried into Phase 7). This function's whole job is to re-inspect
// the SAME bytes a struct decode already consumed, and encoding/json matches
// field names case-insensitively when no exact match exists: a payload
// spelling the key "Tags" populated movieListElement.Tags (turning
// [3, null, 9] into [3, 0, 9] with no error) while an exact-only lookup here
// reported found=false, skipped the null-element re-check entirely, and left
// the corrupted array trusted — the one hole the re-check exists to close,
// open for any differently-cased key.
func rawObjectField(raw json.RawMessage, key string) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	if v, found := obj[key]; found {
		return v, true
	}
	for k, v := range obj {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}
