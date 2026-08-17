package gojq

import "math/big"

// MaxAlloc bounds the total number of bytes a single query execution may
// allocate for its values. Zero, the default, disables the limit and keeps
// the original behaviour. Set it once before running queries.
var MaxAlloc int64

type allocLimitError struct{}

func (*allocLimitError) Error() string {
	return "value allocation exceeds the configured memory limit"
}

// allocSize is a cheap, non-recursive estimate of a value's byte size,
// used only by the allocation meter.
func allocSize(v any) int64 {
	switch t := v.(type) {
	case string:
		return int64(len(t)) + 16 // string header (ptr + len)
	case []any:
		return int64(len(t))*16 + 16
	case map[string]any:
		return int64(len(t))*24 + 16
	case *big.Int:
		return int64(len(t.Bits()))*8 + 16
	default:
		return 16
	}
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
		int64(len(env.forks))*72 > MaxAlloc
}
