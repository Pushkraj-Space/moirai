import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { webcrypto } from "node:crypto";

test("browser canonicalization verifies cross-language archive fixtures", async () => {
  const code = readFileSync("internal/cloud/assets/app.js", "utf8");
  const sandbox = {
    document: { querySelector: () => null },
    crypto: webcrypto,
    TextEncoder,
  };
  vm.createContext(sandbox);
  vm.runInContext(code, sandbox);
  for (const path of ["testdata/archive-v1.moirai"]) {
    const archive = JSON.parse(readFileSync(path, "utf8"));
    sandbox.value = archive.transcript;
    const canonical = vm.runInContext("canonical(value)", sandbox);
    const hash = Buffer.from(
      await webcrypto.subtle.digest(
        "SHA-256",
        new TextEncoder().encode(canonical),
      ),
    ).toString("hex");
    assert.equal(hash, archive.sha256);
  }
});
