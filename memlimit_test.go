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


// the gas meter must not count transient scalars: a streaming query that
// produces millions of numbers but holds one accumulator (a counting reduce)
// would otherwise trip the limit though it holds almost nothing live. it must
// complete with the right value, while collecting the same range still errors.
func TestMaxAllocAllowsStreaming(t *testing.T) {
	defer func(o int64) { MaxAlloc = o }(MaxAlloc)
	MaxAlloc = 4 << 20

	query, err := Parse(`reduce range(2000000) as $x (0; . + 1)`)
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}
	code, err := Compile(query)
	if err != nil {
		t.Fatalf("Compile: %s", err)
	}
	v, ok := code.Run(nil).Next()
	if !ok {
		t.Fatal("counting reduce: no result")
	}
	if e, isErr := v.(error); isErr {
		t.Fatalf("counting reduce should complete, got error: %s", e)
	}
	if got := fmt.Sprint(v); got != "2000000" {
		t.Errorf("counting reduce: got %v, want 2000000", v)
	}

	// collecting the same range into an array retains it, so it must still error.
	q2, _ := Parse(`[range(2000000)] | length`)
	c2, _ := Compile(q2)
	v2, _ := c2.Run(nil).Next()
	if _, isAlloc := v2.(*allocLimitError); !isAlloc {
		t.Errorf("collecting 2M elements should error, got %v", v2)
	}
}


// fromjson yields json.Number values, a distinct type from string. allocSize and
// the streaming decoder must size them by their digit length, or a JSON document
// of a few huge numbers slips through the parse bound (each number charged as a
// 16-byte scalar) and downstream ops run on tens of MB of unmetered data.
func TestMaxAllocSizesJSONNumbers(t *testing.T) {
	defer func(o int64) { MaxAlloc = o }(MaxAlloc)
	MaxAlloc = 4 << 20

	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 30; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(strings.Repeat("9", 500000)) // a 500,000-digit number
	}
	b.WriteString("]")

	query, err := Parse(`fromjson | add | tostring`)
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}
	code, err := Compile(query)
	if err != nil {
		t.Fatalf("Compile: %s", err)
	}
	v, ok := code.Run(b.String()).Next()
	if !ok {
		t.Fatal("expected an error, got no result")
	}
	if _, isAlloc := v.(*allocLimitError); !isAlloc {
		t.Errorf("fromjson of ~15 MB of numbers should error, got %v", v)
	}

	// a small number document still decodes and sums correctly.
	q2, _ := Parse(`fromjson | add`)
	c2, _ := Compile(q2)
	if v2, ok := c2.Run("[1,2,3,4]").Next(); !ok || fmt.Sprint(v2) != "10" {
		t.Errorf("small fromjson: expected 10, got %v", v2)
	}
}


// index/indices/rindex on a string explode the whole haystack to a []any of
// runes (x16); on a big string that materialized tens of MB the meter never saw
// because the result is one number. indices on an array builds its result via an
// internal append. both must be bounded, while small inputs still work.
func TestMaxAllocBoundsIndex(t *testing.T) {
	defer func(o int64) { MaxAlloc = o }(MaxAlloc)
	MaxAlloc = 4 << 20

	bigStr := strings.Repeat("hello world ", 400000) // ~4.8 MB -> ~76 MB of runes
	for _, src := range []string{`index("world")`, `indices("o")`, `rindex("d")`} {
		query, err := Parse(src)
		if err != nil {
			t.Fatalf("Parse(%q): %s", src, err)
		}
		code, err := Compile(query)
		if err != nil {
			t.Fatalf("Compile(%q): %s", src, err)
		}
		v, ok := code.Run(bigStr).Next()
		if !ok {
			t.Errorf("%q: no result", src)
		} else if _, isAlloc := v.(*allocLimitError); !isAlloc {
			t.Errorf("%q on a big string: expected an allocation error, got %v", src, v)
		}
	}

	all := make([]any, 1000000)
	for i := range all {
		all[i] = float64(5)
	}
	query, _ := Parse(`indices(5)`)
	code, _ := Compile(query)
	if v, _ := code.Run(all).Next(); func() bool { _, ok := v.(*allocLimitError); return !ok }() {
		t.Errorf("indices on an all-matches array: expected an allocation error, got a value")
	}

	q2, _ := Parse(`index("world")`)
	c2, _ := Compile(q2)
	if v2, ok := c2.Run("hello world").Next(); !ok || fmt.Sprint(v2) != "6" {
		t.Errorf("small index: expected 6, got %v", v2)
	}
}


// shared references (`[., .]` repeated) make a value whose own footprint is tiny
// expand to an exponentially larger JSON encoding. tojson / tostring / @json
// build the whole string before the meter, which only sees the result, can
// charge it, so a tiny expression produced gigabytes. the encoder must bound it.
func TestMaxAllocBoundsEncoding(t *testing.T) {
	defer func(o int64) { MaxAlloc = o }(MaxAlloc)
	MaxAlloc = 4 << 20

	for _, src := range []string{
		`reduce range(30) as $i (0; [., .]) | tojson`,   // O(30) DAG, 2^30 expansion
		`reduce range(30) as $i (0; [., .]) | tostring`,
		`reduce range(25) as $i ({}; {a: ., b: .}) | tojson`,
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
		} else if _, isAlloc := v.(*allocLimitError); !isAlloc {
			t.Errorf("%q: expected an allocation error, got a value", src)
		}
	}

	// ordinary encoding still works.
	q2, _ := Parse(`tojson`)
	c2, _ := Compile(q2)
	if v2, ok := c2.Run([]any{1.0, "x", nil}).Next(); !ok || fmt.Sprint(v2) != `[1,"x",null]` {
		t.Errorf(`tojson: expected [1,"x",null], got %v`, v2)
	}
}


// add accumulates the whole sum internally (a strings.Builder for strings, an
// append for arrays, a merge for maps) and is charged only at the end, so on a
// large input it built 1+ GB before the meter fired. bound the accumulation.
func TestMaxAllocBoundsAdd(t *testing.T) {
	defer func(o int64) { MaxAlloc = o }(MaxAlloc)
	MaxAlloc = 4 << 20

	bigStr := strings.Repeat("a", 200000)
	sharedStrs := make([]any, 2000)
	for i := range sharedStrs {
		sharedStrs[i] = bigStr
	}
	bigArr := make([]any, 50000)
	for i := range bigArr {
		bigArr[i] = float64(i)
	}
	sharedArrs := make([]any, 2000)
	for i := range sharedArrs {
		sharedArrs[i] = bigArr
	}
	for _, in := range []any{sharedStrs, sharedArrs} {
		query, _ := Parse(`add | length`)
		code, _ := Compile(query)
		if v, _ := code.Run(in).Next(); func() bool { _, ok := v.(*allocLimitError); return !ok }() {
			t.Errorf("add of a large shared-reference input: expected an allocation error, got a value")
		}
	}

	// small add still returns the right values.
	for _, c := range []struct {
		src, in, want string
	}{
		{`add`, `[1,2,3,4]`, "10"},
		{`add`, `["a","b","c"]`, "abc"},
	} {
		q, _ := Parse(c.src)
		code, _ := Compile(q)
		var iv any
		jq, _ := Parse(c.in)
		jc, _ := Compile(jq)
		iv, _ = jc.Run(nil).Next()
		if v, ok := code.Run(iv).Next(); !ok || fmt.Sprint(v) != c.want {
			t.Errorf("add %s: expected %s, got %v", c.in, c.want, v)
		}
	}
}
