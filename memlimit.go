package gojq

import (
	"encoding/json"
	"math/big"
)

// MaxAlloc bounds the total number of bytes a single query execution may
// allocate for its values. Zero, the default, disables the limit and keeps
// the original behaviour. Set it once before running queries.
var MaxAlloc int64

type allocLimitError struct{}

func (*allocLimitError) Error() string {
	return "value allocation exceeds the configured memory limit"
}

// maxRecursionDepth caps the Go-recursive builtins ( compare, contains, encode,
// flatten, deleteEmpty, update, deepmerge, and the JSON decoder ) independently
// of MaxAlloc. Their per-level depth bounds scale with MaxAlloc, so a very large
// MaxAlloc plus a deeply nested input would let the recursion overflow the
// goroutine stack ( a fatal, uncatchable crash ). This hard cap converts that
// into a clean allocation error at any MaxAlloc. At MaxAlloc <= ~4 MiB the
// per-level bound ( MaxAlloc/16 ) is the binding one, so this never changes
// behavior there; it only fires far past any depth a real input reaches.
const maxRecursionDepth = 1 << 19

// allocSize is a cheap, non-recursive estimate of a value's byte size,
// used only by the allocation meter.
func allocSize(v any) int64 {
	switch t := v.(type) {
	case string:
		return int64(len(t)) + 16 // string header (ptr + len)
	case []any:
		return int64(len(t))*16 + 16
	case map[string]any:
		// A Go map has a ~320-byte minimum ( hmap header + a full first
		// bucket ) that len*24 missed entirely, so a chain or collection of
		// tiny 1-key maps ran ~6x over the limit. Charge that base plus ~40
		// per entry ( bucket slot + copied key header ); key string content is
		// still charged separately where the key is built.
		return int64(len(t))*40 + 320
	case *big.Int:
		return int64(len(t.Bits()))*8 + 16
	case json.Number:
		return int64(len(t)) + 16 // a numeric string, like string
	default:
		// scalars (int, float64, bool, nil) are tiny and transient - they
		// cannot grow a run out of memory, so the gas meter does not count
		// them. this keeps a streaming query that produces many scalars, such
		// as `reduce range(N) as $x (0; . + 1)`, from tripping the limit while
		// holding almost nothing live. retained scalars in an array still cost
		// their 16-byte slot, charged at opappend.
		return 0
	}
}

// matchResultSize is the deep byte size of a _match result: an array of match
// objects, each holding a captures array of capture objects. allocSize is
// shallow, so without accounting for the nested captures a query that collects
// many matches ( each with many capture groups ) allocates far past MaxAlloc
// while the meter, seeing only the top-level array, charges almost nothing. The
// shape is fixed ( array -> match map -> captures array -> capture map ), so
// this is a bounded walk, not open recursion.
func matchResultSize(w any) int64 {
	res, ok := w.([]any)
	if !ok {
		return allocSize(w)
	}
	n := allocSize(res)
	for _, m := range res {
		mm, ok := m.(map[string]any)
		if !ok {
			n += allocSize(m)
			continue
		}
		n += allocSize(mm)
		if caps, ok := mm["captures"].([]any); ok {
			n += allocSize(caps)
			for _, c := range caps {
				n += allocSize(c)
			}
		}
	}
	return n
}

// deepSize is the total byte size of a value including everything nested. It is
// used to charge builtins that return a freshly decoded tree the shallow meter
// cannot see ( fromjson ): decodeJSONLimited bounds a single decode, but the
// result is otherwise charged only at its top level, so collecting many decodes
// of a deeply-nested string allocated gigabytes while the meter saw almost
// nothing. It walks iteratively so an arbitrarily deep result cannot overflow
// the goroutine stack, and must only be called on trees ( no shared references,
// which a JSON decode never produces ), else a shared node is counted per path.
func deepSize(v any) int64 {
	var total int64
	stack := []any{v}
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		total += allocSize(x)
		switch x := x.(type) {
		case []any:
			stack = append(stack, x...)
		case map[string]any:
			for key, val := range x {
				total += int64(len(key)) + 16 // key string copied into the object
				stack = append(stack, val)
			}
		}
	}
	return total
}

// transposeResultSize is the byte size of a transpose result: an outer array of
// freshly-made inner arrays whose elements are references into the input. The
// shallow meter charges only the outer array, so an N-by-2 transpose ( two wide
// inner arrays ) collected in a loop allocated hundreds of MB while the meter
// saw ~48 bytes each. This charges the inner arrays' slots too, but stops at the
// elements ( they are refs, and recursing could revisit a shared sub-value ).
func transposeResultSize(w any) int64 {
	outer, ok := w.([]any)
	if !ok {
		return allocSize(w)
	}
	n := allocSize(outer)
	for _, inner := range outer {
		n += allocSize(inner)
	}
	return n
}

// spineSize is the byte size of the fresh spine setpath copies: the containers
// from the root down to the target along the path. setpath ( and |= / = ) copy
// only this spine and share the rest of the input by reference, so charging the
// whole result ( deepSize ) would over-count the shared branches and reject
// legitimate multi-assignments; charging only the top ( allocSize ) misses the
// deep spine, so collecting many deep-path setpaths allocated GB. Walking one
// path is DAG-safe ( no branching ) and matches what update actually allocates.
func spineSize(w, p any) int64 {
	path, ok := p.([]any)
	if !ok {
		return allocSize(w)
	}
	var total int64
	cur := w
	for _, k := range path {
		switch cur.(type) {
		case []any, map[string]any:
			total += allocSize(cur)
		default:
			return total
		}
		cur = funcIndex2(nil, cur, k)
		if _, isErr := cur.(error); isErr {
			return total
		}
	}
	return total
}

// delpathsSize is the byte size of the fresh spines delpaths copies. For each
// path it deletes, update copies the containers from the root down to the
// target, sharing the rest of the input by reference - the same copy-on-write
// as setpath. The shallow meter charges only the top of the result, so
// collecting many deep-path deletions ( del(f) over a deep value ) allocated
// unbounded memory while the meter saw almost nothing. This charges each path's
// spine from the input value, since the result no longer holds the deleted path.
// Overlapping paths share a prefix this double-counts, which only over-charges
// ( safe ), and delete sets are small in practice.
func delpathsSize(v, p any) int64 {
	paths, ok := p.([]any)
	if !ok {
		return 0
	}
	var total int64
	for _, path := range paths {
		total += spineSize(v, path)
	}
	return total
}

// deepMergeSize is the byte size of the fresh maps that map * map ( deepmerge )
// builds. deepMergeObjectsLimited recurses only where both sides hold a map, so
// it copies a spine of new maps and shares the rest by reference; charging the
// whole result would over-count the shared branches, and charging only the top
// misses the recursion, so collecting many deep merges allocated GB. This walks
// the same overlap the merge does, so it charges exactly the new maps. It is
// bounded because the merge already refused ( via its own size counter ) any
// result larger than MaxAlloc, and it returns 0 for non-map operands ( ordinary
// numeric / string multiply, charged by allocSize as before ).
func deepMergeSize(l, r any, depth int) int64 {
	if depth > maxRecursionDepth {
		return 0
	}
	lm, lok := l.(map[string]any)
	rm, rok := r.(map[string]any)
	if !lok || !rok {
		return 0
	}
	n := int64(len(lm)+len(rm))*40 + 320
	for k, rv := range rm {
		if lv, ok := lm[k].(map[string]any); ok {
			if _, ok := rv.(map[string]any); ok {
				n += deepMergeSize(lv, rv, depth+1)
			}
		}
	}
	return n
}

// chargeBytes adds n to the running total and reports whether the limit has
// now been exceeded.
func (env *env) chargeBytes(n int64) bool {
	if MaxAlloc <= 0 {
		return false
	}
	env.alloc += n
	return env.alloc > MaxAlloc
}

// charge adds v's size to the running total and reports whether the limit
// has now been exceeded.
func (env *env) charge(v any) bool {
	return env.chargeBytes(allocSize(v))
}

// arrayTooLarge reports whether a []any of n elements would exceed MaxAlloc.
// Used to pre-check builtins that allocate an input-proportional array in one make().
func arrayTooLarge(n int) bool {
	return MaxAlloc > 0 && int64(n)*16 > MaxAlloc
}

// overStackLimit reports whether the interpreter's own live stacks exceed
// MaxAlloc. Deep or infinite recursion, such as `def f: [f]; f`, grows these
// stacks without ever completing a value, so the value meter never charges it.
// The per-block sizes match the interpreter structs (stack/paths block, scope
// block, fork).
func (env *env) overStackLimit() bool {
	return int64(len(env.stack.data))*24+
		int64(len(env.paths.data))*24+
		int64(len(env.scopes.data))*48+
		int64(len(env.values))*16+
		int64(len(env.forks))*72 > MaxAlloc
}

// decodeJSONLimited decodes one JSON value from dec, tracking the cumulative
// shallow size of the structure it builds and erroring once that size passes
// MaxAlloc. It mirrors json.Decoder.Decode under UseNumber, so a huge document
// cannot be materialized in one shot before the value meter would see it.
// Sizes match allocSize: 16 bytes per array slot, 40 per object entry (24 map
// bucket + a 16-byte string header for the key), plus each value's own footprint.
func decodeJSONLimited(dec *json.Decoder, size *int64, depth int) (any, error) {
	if MaxAlloc > 0 && depth > maxRecursionDepth {
		return nil, &allocLimitError{}
	}
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := t.(type) {
	case json.Delim:
		switch t {
		case '[':
			arr := []any{}
			for dec.More() {
				if *size += 16; *size > MaxAlloc {
					return nil, &allocLimitError{}
				}
				v, err := decodeJSONLimited(dec, size, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			_, err := dec.Token() // consume ]
			return arr, err
		case '{':
			obj := map[string]any{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k := key.(string)
				if *size += int64(len(k)) + 40; *size > MaxAlloc {
					return nil, &allocLimitError{}
				}
				v, err := decodeJSONLimited(dec, size, depth+1)
				if err != nil {
					return nil, err
				}
				obj[k] = v
			}
			_, err := dec.Token() // consume }
			return obj, err
		default:
			return nil, &json.SyntaxError{}
		}
	case string:
		*size += int64(len(t)) + 16
	case json.Number:
		*size += int64(len(t)) + 16
	default: // bool, nil
		*size += 16
	}
	if *size > MaxAlloc {
		return nil, &allocLimitError{}
	}
	return t, nil
}
