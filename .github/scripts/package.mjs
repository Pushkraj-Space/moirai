import { mkdirSync, readFileSync, writeFileSync, copyFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { join, resolve } from "node:path";

const version = JSON.parse(
  readFileSync("sdk/typescript/package.json", "utf8"),
).version;
if (!/^\d+\.\d+\.\d+$/.test(version))
  throw new Error("Invalid package version");
const nativeOS = { darwin: "darwin", linux: "linux", win32: "windows" }[
  process.platform
];
const os = process.env.TARGET_OS || nativeOS;
const arch =
  process.env.TARGET_ARCH || { x64: "amd64", arm64: "arm64" }[process.arch];
if (
  !["linux", "darwin", "windows"].includes(os) ||
  !["amd64", "arm64"].includes(arch)
)
  throw new Error("Unsupported platform");
const out = resolve(process.env.PACKAGE_DIR || "dist/releases");
const stem = `moirai_${version}_${os}_${arch}`;
const stage = join(out, stem);
mkdirSync(stage, { recursive: true });
function run(program, args, options = {}) {
  const result = spawnSync(program, args, { encoding: "utf8", ...options });
  if (result.status !== 0)
    throw new Error(`${program} failed: ${result.stderr || result.error}`);
  return result.stdout.trim();
}
const binary = join(stage, os === "windows" ? "moirai.exe" : "moirai");
run(
  "go",
  ["build", "-trimpath", "-ldflags=-s -w", "-o", binary, "./cmd/moirai"],
  { env: { ...process.env, CGO_ENABLED: "0", GOOS: os, GOARCH: arch } },
);
if (
  os === nativeOS &&
  arch === { x64: "amd64", arm64: "arm64" }[process.arch]
) {
  if (run(binary, ["version"]) !== version)
    throw new Error("CLI and npm versions differ");
  const formats = JSON.parse(run(binary, ["formats", "--json"]));
  if (!formats.some((format) => format.capability.continue))
    throw new Error("Packaged CLI has no continuation targets");
}
copyFileSync("LICENSE", join(stage, "LICENSE"));
const buildInfo = run("go", ["version", "-m", binary]);
writeFileSync(
  join(stage, "BUILD.txt"),
  `Moirai ${version}\n${os}/${arch}\n${buildInfo}\n`,
);
const dependencies = [...buildInfo.matchAll(/^\s*dep\s+(\S+)\s+(\S+)/gm)];
const packages = [
  { name: "moirai", versionInfo: version },
  ...dependencies.map((match) => ({ name: match[1], versionInfo: match[2] })),
].map((pkg, index) => ({
  ...pkg,
  SPDXID: `SPDXRef-Package-${index}`,
  downloadLocation: "NOASSERTION",
  filesAnalyzed: false,
  licenseConcluded: "NOASSERTION",
  licenseDeclared: index === 0 ? "Apache-2.0" : "NOASSERTION",
  copyrightText: "NOASSERTION",
}));
writeFileSync(
  join(stage, "SBOM.spdx.json"),
  JSON.stringify(
    {
      spdxVersion: "SPDX-2.3",
      dataLicense: "CC0-1.0",
      SPDXID: "SPDXRef-DOCUMENT",
      name: stem,
      documentNamespace: `https://moirai.to/spdx/${stem}-${randomUUID()}`,
      creationInfo: {
        created: new Date().toISOString().replace(/\.\d{3}Z$/, "Z"),
        creators: ["Tool: moirai-release-packager"],
      },
      packages,
      relationships: [
        {
          spdxElementId: "SPDXRef-DOCUMENT",
          relationshipType: "DESCRIBES",
          relatedSpdxElement: "SPDXRef-Package-0",
        },
        ...packages
          .slice(1)
          .map((pkg) => ({
            spdxElementId: "SPDXRef-Package-0",
            relationshipType: "DEPENDS_ON",
            relatedSpdxElement: pkg.SPDXID,
          })),
      ],
    },
    null,
    2,
  ) + "\n",
);
const filename = `${stem}.tar.gz`;
run("tar", ["-czf", join(out, filename), "-C", stage, "."]);
const digest = createHash("sha256")
  .update(readFileSync(join(out, filename)))
  .digest("hex");
writeFileSync(join(out, `${filename}.sha256`), `${digest}  ${filename}\n`);
if (os === "windows") {
  const executableName = `${stem}.exe`;
  copyFileSync(binary, join(out, executableName));
  const executableDigest = createHash("sha256")
    .update(readFileSync(binary))
    .digest("hex");
  writeFileSync(
    join(out, `${executableName}.sha256`),
    `${executableDigest}  ${executableName}\n`,
  );
}
console.log(join(out, filename));
