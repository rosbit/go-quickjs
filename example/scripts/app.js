// app.js -- an ES module (it uses import), evaluated by ctx.EvalFile.
// ES module exports do not land in the global scope, so the entry point is
// published on globalThis for golang to call.
import {twice, label} from "./lib.js";

globalThis.runModule = function () {
    return label + ":" + twice(21);
};
