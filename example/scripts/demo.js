// demo.js -- loaded and driven by golang (see example/main.go)

var version = "1.0.0";

// entry point called from golang: ctx.Call("entry", "world", 2)
function entry(name, times) {
    var out = [];
    for (var i = 0; i < times; i++) {
        out.push("hello " + name + " #" + i);
    }
    // "adder" is implemented in golang
    out.push("adder(1,2)=" + adder(1, 2));
    // "me" is a golang struct, Greet is its golang method
    out.push(me.Greet("hi"));
    // "cfg"/"names" are golang map/slice
    out.push("debug=" + cfg.debug + ", first=" + names[0] + ", all=" + sumAll(1, 2, 3, 4));
    return out;
}

// returns an object
function stats(nums) {
    var sum = 0;
    for (var i = 0; i < nums.length; i++) sum += nums[i];
    return {count: nums.length, sum: sum, avg: sum / nums.length};
}

// bound to a golang func variable
function calc(a, b) {
    return {sum: a + b, diff: a - b, prod: a * b};
}

// a javascript function returning a javascript function, bound in golang too
function makeAdder(n) {
    return function (x) {
        return x + n;
    };
}

function boom() {
    throw new Error("something went wrong in javascript");
}

function callWithErr(v) {
    return withErr(v);
}

async function later(x) {
    var v = await Promise.resolve(x);
    return v * 2;
}

console.log("demo.js loaded, version", version);
