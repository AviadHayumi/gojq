package gojq

import (
	"strings"
	"testing"
)

// with MaxAlloc set, a memory bomb must error instead of allocating unboundedly.
func TestMaxAllocStopsBombs(t *testing.T) {
	defer func(o int64) { MaxAlloc = o }(MaxAlloc)
	MaxAlloc = 4 << 20 // 4 MiB

	for _, src := range []string{
		`"x" * 2000000000`,                        // string repeat
		`"xx"` + strings.Repeat(" | . + .", 30),   // doubling chain
		`[range(100000000)]`,                       // huge array
		`null | setpath([50000000]; 1)`,            // huge sparse array
		`reduce range(60) as $i ("xx"; . + .)`,     // growing reduce
	} {
		query, err := Parse(src)
		if err != nil {
			t.Fatalf("Parse(%q): %s", src, err)
		}
		code, err := Compile(query)
		if err != nil {
			t.Fatalf("Compile(%q): %s", src, err)
		}
		iter, gotErr := code.Run(nil), false
		for i := 0; i < 1000000; i++ {
			v, ok := iter.Next()
			if !ok {
				break
			}
			if _, ok := v.(error); ok {
				gotErr = true
				break
			}
		}
		if !gotErr {
			t.Errorf("expected an allocation error for %q", src)
		}
	}
}

// with MaxAlloc set, ordinary expressions must still succeed.
func TestMaxAllocAllowsNormal(t *testing.T) {
	defer func(o int64) { MaxAlloc = o }(MaxAlloc)
	MaxAlloc = 4 << 20

	for _, src := range []string{`"ab" * 3`, `[range(100)] | add`, `{a: 1, b: 2}`, `. + 1`} {
		query, err := Parse(src)
		if err != nil {
			t.Fatalf("Parse(%q): %s", src, err)
		}
		code, err := Compile(query)
		if err != nil {
			t.Fatalf("Compile(%q): %s", src, err)
		}
		v, ok := code.Run(3).Next()
		if !ok {
			t.Errorf("%q: no result", src)
		} else if e, ok := v.(error); ok {
			t.Errorf("%q: unexpected error: %s", src, e)
		}
	}
}
