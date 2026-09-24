# go-quickjs, make quickjs be embedded easily

[QuickJS](https://bellard.org/quickjs/) is a small embeddable javascript engine
written by [Fabrice Bellard](https://bellard.org): no JIT, a few hundred KB of
code, and a startup cost measured in microseconds. `go-quickjs` wraps it so a Go
program can run javascript, hand it Go values and let javascript call back into
Go, without meeting the sharp edges that usually come with a cgo binding.

  `go get github.com/rosbit/go-quickjs`

```go
ctx, err := quickjs.NewContext()
defer ctx.Close()

res, _ := ctx.Eval("a + b", map[string]interface{}{"a": 10, "b": 1})
fmt.Println(res)   // 11
```

## What this wrapper is for

### Calling across the boundary, one line each way

```go
// Go -> javascript: load a file, bind its function to a Go func variable
ctx.EvalFile("a.js", nil)
var add func(int, int) int
ctx.BindFunc("add", &add)
fmt.Println(add(1, 2))

// javascript -> Go: inject a Go func into the global scope
ctx.Set("hostAdd", func(a, b int) int { return a + b })
ctx.Eval(`hostAdd(2, 3)`)   // 5
```

Errors cross the boundary intact, in both directions: a Go function that returns
a non-nil `error` throws inside javascript, and a javascript exception reaches Go
as a `*quickjs.Error` carrying `Name`, `Message` and the javascript `Stack`.

### A Context is safe to share between goroutines

The cache entry points hand the same `*Context` to every caller, so an HTTP
service can use it straight from its workers:

```go
ctx, existing, err := quickjs.LoadFileFromCache("rules.js", nil, "/opt/js-libs")
```

That is safe because every Context owns a dedicated engine goroutine pinned to a
single OS thread, and every C call is routed onto it: quickjs is never touched
from two threads at once, and no finalizer frees a value on a foreign thread.
There is no global lock, so separate contexts do not serialise against each
other. (The historical `WithThreadPinning()` option is a no-op kept for
compatibility.)

### Scripts load once and reload when the file changes

`LoadFileFromCache` keys on the path, the search directories and the options:
the first call evaluates the file, later calls reuse the same Context, and a
changed mtime drops the old one and rebuilds it (`existing=false` reports that).
`ClearCache()` releases everything.

### Go values are handed over lazily, not copied

Maps, slices, arrays, structs and pointers arrive in javascript as a proxy: walk
to any depth, call exported methods, assign to fields, and the write reaches the
original Go value. Nothing is converted up front, so there is no depth limit and
cyclic data is fine. Exported names are also reachable lower-cased
(`Name` -> `name`, `HTTPStatus` -> `httpStatus`).

```go
ctx.Set("me", &Person{Name: "gopher", Age: 3})
```

```javascript
me.name          // "gopher"
me.greet("hi")   // methods are called on the Go value
me.age = 4       // writes through to the Go struct
console.log(me)  // {Name: gopher, Age: 4, Greet: [Function: Greet]}
```

### ...except slices, which can be real javascript arrays

A proxied slice is array-*like* but not an `Array`, which matters as soon as the
script uses array methods. `WithSlicesAsArrays()` materialises slices and arrays
as genuine javascript arrays:

|                                     | proxy (default for `NewContext`) | with `WithSlicesAsArrays()` |
| --- | --- | --- |
| `v.length`, `v[0]`, `for...in`      | array-like, works                | works                        |
| `Array.isArray(v)`, `instanceof`    | `false`                          | `true`                       |
| `v.forEach` / `map` / `filter`      | `undefined`                      | works                        |
| `[...v]`, `for (x of v)`            | throws "not iterable"            | works                        |
| `JSON.stringify(v)`                 | `{"0":1,"1":2}`                  | `[1,2]`                      |

The cache entry points (`LoadFileFromCache`, `LoadFileFromCacheWith`) turn it on
by default, because that is what scripts expect; pass `WithoutSlicesAsArrays()`
to get the lazy proxy back. `NewContext()` is unaffected and stays lazy unless
asked. The price of the array form is that it is a snapshot: reads re-materialise
it and javascript writes no longer reach the Go slice. `[]byte` is deliberately
never affected -- it keeps travelling as a string.

### Modules, both flavours

ESM `import` works, with a PATH-like list of search directories so scripts can
say `import { f } from "mylib"`; a module next to the entry file resolves with no
configuration at all. CommonJS is one option away in `NewContext` (already on in
the cache entry points) and gives scripts a global `require()` with
`node_modules` lookup, `.json` support and `require.cache`.

### Bounded resources, no leaks

`WithMemoryLimit`, `WithGCThreshold` and `WithMaxStackSize` bound what a script
can consume; Go functions and closures handed to javascript are released by
finalizers when javascript drops them, so long-running processes do not grow.
`console.log` is wired to a writer of your choice (`WithConsoleWriter`), plus a
global `print`. Log lines are coloured by value type (strings red, numbers
yellow, objects cyan, `undefined`/`null` grey) with no TTY detection, so redirect
the writer when you want them plain.

## Notes

### Numbers

Integers come back as `int64`, the rest as `float64`. Because quickjs uses
NaN-boxing, a floating point result that happens to be integral (`1.5 + 0.5`)
also arrives as `int64`, so accept both -- `reflect.ValueOf(v).Float()` is a
convenient normaliser.

### Layout

`csrc/` holds a self-contained copy of the quickjs C sources; all includes point
at it, so upgrading quickjs means replacing that directory and nothing else.

## Status

The behaviours described above are covered by a test suite that runs with
`-race`, including the concurrency and finalizer paths. The quickjs C sources in
`csrc/` are upstream's, embedded as-is.

## Contribution

Pull requests are welcome! Also, if you want to discuss something send a pull
request with proposal and changes.
__Convention:__ fork the repository and make changes on your fork in a feature branch.
