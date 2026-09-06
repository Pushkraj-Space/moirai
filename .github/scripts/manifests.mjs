import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const version = JSON.parse(
  readFileSync("sdk/typescript/package.json", "utf8"),
).version;
const root = process.argv[2] || "dist/releases";
const base = `https://github.com/october-dev/moirai/releases/download/v${version}`;
const artifact = (os, arch, extension = "tar.gz") =>
  `moirai_${version}_${os}_${arch}.${extension}`;
function digest(name) {
  const value = readFileSync(join(root, name + ".sha256"), "utf8").split(
    /\s/,
  )[0];
  if (!/^[a-f0-9]{64}$/.test(value)) throw new Error("Invalid checksum");
  return value;
}
const stanza = (os, arch) =>
  `      url "${base}/${artifact(os, arch)}"\n      sha256 "${digest(artifact(os, arch))}"`;
writeFileSync(
  join(root, "moirai.rb"),
  `class Moirai < Formula
  desc "Portable session history for AI agents"
  homepage "https://github.com/october-dev/moirai"
  version "${version}"
  license "Apache-2.0"
  on_macos do
    on_arm do
${stanza("darwin", "arm64")}
    end
    on_intel do
${stanza("darwin", "amd64")}
    end
  end
  on_linux do
    on_arm do
${stanza("linux", "arm64")}
    end
    on_intel do
${stanza("linux", "amd64")}
    end
  end
  def install
    bin.install "moirai"
    doc.install "BUILD.txt", "SBOM.spdx.json"
  end
  test do
    assert_equal "${version}", shell_output("#{bin}/moirai version").strip
  end
end
`,
);
const exe = artifact("windows", "amd64", "exe");
writeFileSync(
  join(root, "October.Moirai.installer.yaml"),
  `PackageIdentifier: October.Moirai
PackageVersion: ${version}
InstallerType: portable
Commands:
  - moirai
Installers:
  - Architecture: x64
    InstallerUrl: ${base}/${exe}
    InstallerSha256: ${digest(exe).toUpperCase()}
ManifestType: installer
ManifestVersion: 1.6.0
`,
);
writeFileSync(
  join(root, "October.Moirai.locale.en-US.yaml"),
  `PackageIdentifier: October.Moirai
PackageVersion: ${version}
PackageLocale: en-US
Publisher: October
PackageName: Moirai
PackageUrl: https://github.com/october-dev/moirai
License: Apache-2.0
LicenseUrl: https://github.com/october-dev/moirai/blob/v${version}/LICENSE
ShortDescription: Portable session history for AI agents and harnesses.
ManifestType: defaultLocale
ManifestVersion: 1.6.0
`,
);
writeFileSync(
  join(root, "October.Moirai.yaml"),
  `PackageIdentifier: October.Moirai
PackageVersion: ${version}
DefaultLocale: en-US
ManifestType: version
ManifestVersion: 1.6.0
`,
);
console.log(
  "Generated Homebrew and winget manifests from verified release checksums.",
);
