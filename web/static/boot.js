// Starts the Go/Wasm dashboard client. This and wasm_exec.js are the
// only JavaScript; the client itself is Go (see web/client).
const go = new Go();
WebAssembly.instantiateStreaming(fetch("/static/client.wasm"), go.importObject)
  .then((r) => go.run(r.instance))
  .catch((err) => {
    const el = document.getElementById("conn");
    el.textContent = "Live updates unavailable: " + err;
    el.hidden = false;
  });
