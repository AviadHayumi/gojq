package gojq

import (
	"fmt"
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


// deep or infinite recursion grows the interpreter's own stacks (operands,
// forks, scopes) without ever completing a value, so the value meter never
// charges it. and a big.Int can grow past the value meter because its size is
// not the size of the interface that holds it. both must error under MaxAlloc.
func TestMaxAllocStopsRecursionAndBigint(t *testing.T) {
	defer func(o int64) { MaxAlloc = o }(MaxAlloc)
	MaxAlloc = 4 << 20

	squareChain := "."
	for i := 0; i < 30; i++ {
		squareChain += " | (. * .)"
	}
	for _, c := range []struct {
		src string
		in  any
	}{
		{`def f: [f]; f`, nil}, // infinite nesting recursion
		{squareChain, 3},       // big.Int doubling by repeated squaring
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
		} else if _, isAlloc := v.(*allocLimitError); !isAlloc {
			t.Errorf("%q: expected *allocLimitError, got %v", c.src, v)
		}
	}
}


// a value in a []any slot costs 16 bytes for the slot plus its own footprint,
// so an array of a million one-character strings is ~33 MB, not the ~1 MB its
// contents suggest. before charging the slot, such an array grew far past the
// limit (and `add` then churned 150+ MB the meter never saw). it must now error.
func TestMaxAllocCountsArraySlots(t *testing.T) {
	defer func(o int64) { MaxAlloc = o }(MaxAlloc)
	MaxAlloc = 4 << 20

	for _, src := range []string{
		`[range(1000000) | tostring]`,       // a million tiny strings
		`[range(1000000) | tostring] | add`, // ... then concatenated
		`[range(1000000)]`,                   // a million boxed numbers
	} {
		query, err := Parse(src)
		if err != nil {
			t.Fatalf("Parse(%q): %s", src, err)
		}
		code, err := Compile(query)
		if err != nil {
			t.Fatalf("Compile(%q): %s", src, err)
		}
		v, ok := code.Run(nil).Next()
		if !ok {
			t.Errorf("%q: expected an error, got no result", src)
		} else if _, isErr := v.(error); !isErr {
			t.Errorf("%q: expected an allocation error, got a value", src)
		}
	}
}


// fromjson builds a whole structure from one json.Decode; a 2 MB array string
// of a million ones would materialize ~16 MB before the value meter saw it.
// the charging streaming decoder must stop it during the parse.
func TestMaxAllocStopsFromJSON(t *testing.T) {
	defer func(o int64) { MaxAlloc = o }(MaxAlloc)
	MaxAlloc = 4 << 20

	src := "[" + strings.Repeat("1,", 999999) + "1]"
	query, err := Parse(`fromjson`)
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}
	code, err := Compile(query)
	if err != nil {
		t.Fatalf("Compile: %s", err)
	}
	v, ok := code.Run(src).Next()
	if !ok {
		t.Fatal("expected an error, got no result")
	}
	if _, isAlloc := v.(*allocLimitError); !isAlloc {
		t.Errorf("expected *allocLimitError, got %v", v)
	}
}


// a recursive function that binds many variables or takes many arguments grows
// the interpreter's value-slot slice (env.values) far faster than its scope
// stack, so the slot slice must be part of the live stack limit too.
func TestMaxAllocStopsValueSlotGrowth(t *testing.T) {
	defer func(o int64) { MaxAlloc = o }(MaxAlloc)
	MaxAlloc = 4 << 20

	var binds strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&binds, ". as $a%d | ", i)
	}
	for _, src := range []string{
		"def f: " + binds.String() + "[f]; f",                        // many bindings per level
		"def f($a;$b;$c;$d;$e;$f;$g;$h): f(.;.;.;.;.;.;.;.); f(.;.;.;.;.;.;.;.)", // many args
	} {
		query, err := Parse(src)
		if err != nil {
			t.Fatalf("Parse: %s", err)
		}
		code, err := Compile(query)
		if err != nil {
			t.Fatalf("Compile: %s", err)
		}
		v, ok := code.Run("x").Next()
		if !ok {
			t.Errorf("expected an error, got no result")
		} else if _, isAlloc := v.(*allocLimitError); !isAlloc {
			t.Errorf("expected *allocLimitError, got %v", v)
		}
	}
}


// compileRegexp caches every distinct pattern; a run generating millions of
// them once grew the cache to 70-95 MB (and it persisted after the run). the
// cache must stop retaining once it reaches MaxAlloc, while patterns still work.
func TestRegexpCacheBounded(t *testing.T) {
	defer func(o int64) { MaxAlloc = o }(MaxAlloc)
	MaxAlloc = 1 << 20 // 1 MiB

	var cache reCache
	for i := 0; i < 50000; i++ {
		if _, err := compileRegexp(fmt.Sprintf("distinct-pattern-%d", i), "", &cache); err != nil {
			t.Fatalf("compile %d: %s", i, err)
		}
	}
	if got := cache.bytes.Load(); got > MaxAlloc+4096 {
		t.Errorf("cache retained %d bytes for 50000 patterns, want bounded near MaxAlloc (%d)", got, int64(MaxAlloc))
	}
	// still correct after the cache filled: a pattern compiles and matches.
	r, err := compileRegexp("^abc$", "", &cache)
	if err != nil || !r.MatchString("abc") || r.MatchString("xyz") {
		t.Errorf("regex broken after cache filled: r=%v err=%v", r, err)
	}
}


// match with the "g" flag builds one map per match (plus one per capture group)
// in a single make, and the result array is charged only shallowly, so a global
// match on a moderate string materialized tens of MB under the limit. it must
// now error, while a small match still returns correct results.
func TestMaxAllocBoundsGlobalMatch(t *testing.T) {
	defer func(o int64) { MaxAlloc = o }(MaxAlloc)
	MaxAlloc = 4 << 20

	big := strings.Repeat("ab", 500000)
	query, err := Parse(`[match("(a)(b)"; "g")] | length`)
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}
	code, err := Compile(query)
	if err != nil {
		t.Fatalf("Compile: %s", err)
	}
	v, ok := code.Run(big).Next()
	if !ok {
		t.Fatal("expected an error, got no result")
	}
	if _, isAlloc := v.(*allocLimitError); !isAlloc {
		t.Errorf("expected *allocLimitError for a huge global match, got %v", v)
	}

	// a small match must still return its results under the same limit.
	query2, _ := Parse(`[match("a"; "g") | .offset]`)
	code2, _ := Compile(query2)
	v2, ok := code2.Run("banana").Next()
	if !ok {
		t.Fatal("no result for small match")
	}
	if got, isArr := v2.([]any); !isArr || len(got) != 3 {
		t.Errorf("small match: expected 3 offsets, got %v", v2)
	}
}
