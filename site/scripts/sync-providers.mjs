// Sync each provider's operator docs into the Starlight site.
//
// Canonical content lives next to the code in `providers/<name>/docs/**.md(x)`
// (so it ships with the provider and is reviewed alongside it). This copies it
// into `site/src/content/docs/providers/<name>/**`, where Starlight routes it to
// /providers/<name>/... and the auto-generated "Providers" sidebar group picks
// it up. The synced tree is git-ignored — edit the source under providers/, not
// the copy.
import { readdir, mkdir, copyFile, rm, access } from "node:fs/promises";
import { join, dirname } from "node:path";

const scriptsDir = import.meta.dirname;
const siteDir = join(scriptsDir, "..");
const repoRoot = join(siteDir, "..");
const providersDir = join(repoRoot, "providers");
const destBase = join(siteDir, "src", "content", "docs", "providers");

const exists = async (p) => {
  try {
    await access(p);
    return true;
  } catch {
    return false;
  }
};

async function* walk(dir) {
  for (const e of await readdir(dir, { withFileTypes: true })) {
    const full = join(dir, e.name);
    if (e.isDirectory()) {
      yield* walk(full);
    } else if (/\.(md|mdx|svg|png)$/.test(e.name)) {
      yield full;
    }
  }
}

await rm(destBase, { recursive: true, force: true });

let copied = 0;
let providers = 0;
for (const entry of await readdir(providersDir, { withFileTypes: true })) {
  if (!entry.isDirectory()) continue;
  const docs = join(providersDir, entry.name, "docs");
  if (!(await exists(docs))) continue;
  // `_template` is the copy-me skeleton; skip its docs on the site.
  if (entry.name.startsWith("_")) continue;
  providers++;
  for await (const file of walk(docs)) {
    const rel = file.slice(docs.length + 1);
    const dest = join(destBase, entry.name, rel);
    await mkdir(dirname(dest), { recursive: true });
    await copyFile(file, dest);
    copied++;
  }
}

console.log(`sync-providers: ${copied} file(s) from ${providers} provider(s) -> ${destBase}`);

// Sync the conformance program docs to /conformance. The canonical overview
// `conformance/docs/conformance.md` is mapped to index.md so it routes to
// /conformance/ (its source name is kept for the GitHub links that reference it).
const confSrc = join(repoRoot, "conformance", "docs");
const confDest = join(siteDir, "src", "content", "docs", "conformance");
await rm(confDest, { recursive: true, force: true });
let confCopied = 0;
if (await exists(confSrc)) {
  for await (const file of walk(confSrc)) {
    const rel = file.slice(confSrc.length + 1);
    const dest = join(confDest, rel === "conformance.md" ? "index.md" : rel);
    await mkdir(dirname(dest), { recursive: true });
    await copyFile(file, dest);
    confCopied++;
  }
}
console.log(`sync-providers: ${confCopied} conformance doc(s) -> ${confDest}`);
