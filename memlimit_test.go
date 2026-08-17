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

// each of these builtins used to build an input-proportional value in one make()
// before the value meter could charge it. with MaxAlloc set they must now error.
func TestMaxAllocStopsSingleShotBuiltins(t *testing.T) {
	defer func(o int64) { MaxAlloc = o }(MaxAlloc)
	MaxAlloc = 1 << 20 // 1 MiB

	bigStr := strings.Repeat("a", 1<<20)
	bigArr := make([]any, 1<<20)
	for i := range bigArr {
		bigArr[i] = float64(i)
	}
	for _, c := range []struct {
		src string
		in  any
	}{
		{`explode`, bigStr},      // string -> []any (x16)
		{`. / ""`, bigStr},       // split operator on empty separator
		{`split("")`, bigStr},    // split builtin
		{`reverse`, bigArr},      // input-sized array copy
		{`sort`, bigArr},         // sortItems make
		{`unique`, bigArr},       // via sort
	} {
		query, err := Parse(c.src)
		if err != nil {
			t.Fatalf("Parse(%q): %s", c.src, err)
		}
		code, err := Compile(query)
		if err != nil {
			t.Fatalf("Compile(%q): %s", c.src, err)
		}
		v, ok := code.Run(c.in).Next()
		if !ok {
			t.Errorf("%q: expected an error, got no result", c.src)
		} else if _, isErr := v.(error); !isErr {
			t.Errorf("%q: expected an allocation error, got a value", c.src)
		}
	}
}
