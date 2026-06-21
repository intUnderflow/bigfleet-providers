// Sync each provider's operator docs into the Starlight site.
//
// Canonical content lives next to the code in `providers/<name>/docs/**.md(x)`
// (so it ships with the provider and is reviewed alongside it). This copies it
// into `site/src/content/docs/providers/<name>/**`, where Starlight routes it to
// /providers/<name>/... and the auto-generated "Providers" sidebar group picks
// it up. The synced tree is git-ignored — edit the source under providers/, not
// the copy.
//
// It also generates the /providers/ landing page (providers/index.mdx) from the
// set of providers it finds, so the list stays in sync automatically: add a
// provider with a docs/ folder and it appears on the site with no manual edit.
import {
  readdir,
  mkdir,
  copyFile,
  rm,
  access,
  readFile,
  writeFile,
} from "node:fs/promises";
import { join, dirname } from "node:path";

const scriptsDir = import.meta.dirname;
const siteDir = join(scriptsDir, "..");
const repoRoot = join(siteDir, "..");
const providersDir = join(repoRoot, "providers");
const destBase = join(siteDir, "src", "content", "docs", "providers");
// Provider logos are served as static assets at /providers/<name>.svg.
const publicProvidersDir = join(siteDir, "public", "providers");

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

// Minimal top-level YAML frontmatter reader: enough to pull `title` and
// `description` off a provider's docs/index.md. Only matches unindented keys, so
// nested blocks (e.g. `sidebar:`) are ignored. Strips one layer of quotes.
async function frontmatter(file) {
  const text = await readFile(file, "utf8");
  const block = text.match(/^---\r?\n([\s\S]*?)\r?\n---/);
  if (!block) return {};
  const out = {};
  for (const line of block[1].split("\n")) {
    const kv = line.match(/^([A-Za-z][\w-]*):\s*(.*)$/);
    if (!kv) continue;
    let v = kv[2].trim();
    if (
      (v.startsWith('"') && v.endsWith('"')) ||
      (v.startsWith("'") && v.endsWith("'"))
    ) {
      v = v.slice(1, -1);
    }
    out[kv[1]] = v;
  }
  return out;
}

await rm(destBase, { recursive: true, force: true });

let copied = 0;
const found = []; // { name, title, description } for the generated index
for (const entry of await readdir(providersDir, { withFileTypes: true })) {
  if (!entry.isDirectory()) continue;
  const docs = join(providersDir, entry.name, "docs");
  if (!(await exists(docs))) continue;
  // `_template` is the copy-me skeleton; skip its docs on the site.
  if (entry.name.startsWith("_")) continue;
  // Pull the listing metadata off the provider's overview page.
  const overview = join(docs, "index.md");
  const meta = (await exists(overview)) ? await frontmatter(overview) : {};
  const logoSrc = join(docs, "logo.svg");
  const logoDarkSrc = join(docs, "logo-dark.svg");
  found.push({
    name: entry.name,
    title: meta.title || entry.name,
    description: meta.description || "",
    logo: (await exists(logoSrc)) ? logoSrc : null,
    // Optional dark-theme variant, shown only when the site is in dark mode
    // (for logos whose default form is low-contrast on the dark card).
    logoDark: (await exists(logoDarkSrc)) ? logoDarkSrc : null,
  });
  for await (const file of walk(docs)) {
    const rel = file.slice(docs.length + 1);
    // logo.svg / logo-dark.svg are served from public/ (below), not as content.
    if (rel === "logo.svg" || rel === "logo-dark.svg") continue;
    const dest = join(destBase, entry.name, rel);
    await mkdir(dirname(dest), { recursive: true });
    await copyFile(file, dest);
    copied++;
  }
}

console.log(`sync-providers: ${copied} file(s) from ${found.length} provider(s) -> ${destBase}`);

// Generate the /providers/ landing page from the providers we just found, so the
// list is always in sync with the repo. Each card shows the provider's logo,
// title, and description and links to its overview; title/description/logo come
// from the provider itself (docs/index.md frontmatter + docs/logo.svg). Logos
// are copied to public/providers/<name>.svg and rendered via <img> (NOT inline
// SVG) so a provider's SVG can never execute script or collide with page ids.
// A provider without a logo.svg falls back to its initial.
const esc = (s) =>
  String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");

found.sort((a, b) => a.name.localeCompare(b.name));

await rm(publicProvidersDir, { recursive: true, force: true });
await mkdir(publicProvidersDir, { recursive: true });
let logos = 0;
const cards = [];
for (const p of found) {
  let mark;
  if (p.logo) {
    await copyFile(p.logo, join(publicProvidersDir, `${p.name}.svg`));
    logos++;
    mark = `<img class="provider-logo provider-logo--light" src="/providers/${p.name}.svg" alt="" loading="lazy" />`;
    if (p.logoDark) {
      await copyFile(p.logoDark, join(publicProvidersDir, `${p.name}-dark.svg`));
      mark += `\n    <img class="provider-logo provider-logo--dark" src="/providers/${p.name}-dark.svg" alt="" loading="lazy" />`;
    }
  } else {
    mark = `<span class="provider-logo provider-logo--fallback" aria-hidden="true">${esc(p.title.slice(0, 1))}</span>`;
  }
  cards.push(
    `  <a class="provider-card" href="/providers/${p.name}/">\n` +
      `    ${mark}\n` +
      `    <span class="provider-card-text">\n` +
      `      <span class="provider-card-title">${esc(p.title)}</span>\n` +
      `      <span class="provider-card-desc">${esc(p.description)}</span>\n` +
      `    </span>\n` +
      `  </a>`,
  );
}
const indexMdx = `---
title: Providers
description: Every capacity provider in the bigfleet-providers monorepo. Deploy one alongside BigFleet to provision and reclaim the machines your fleet runs on.
sidebar:
  order: -1
  label: All providers
---

{/* Generated by site/scripts/sync-providers.mjs from the providers in the repo — do not edit by hand. */}

Each provider below is a standalone capacity provider you deploy next to [BigFleet](https://bigfleet.lucy.sh): point it at your substrate and it provisions, configures, drains, and reclaims real machines as your fleet's demand moves. Every one is held to the same [conformance bar](/conformance/).

<div class="provider-grid not-content">
${cards.join("\n")}
</div>
`;
await mkdir(destBase, { recursive: true });
await writeFile(join(destBase, "index.mdx"), indexMdx);
console.log(
  `sync-providers: generated /providers/ index listing ${found.length} provider(s), ${logos} with logos`,
);

// Also emit the provider set as JSON so other pages (e.g. the homepage logo
// strip via src/components/ProviderStrip.astro) render the same list without
// re-declaring it — one source of truth, so nothing can drift out of sync.
const dataDir = join(siteDir, "src", "data");
await mkdir(dataDir, { recursive: true });
await writeFile(
  join(dataDir, "providers.json"),
  JSON.stringify(
    found.map((p) => ({
      name: p.name,
      title: p.title,
      description: p.description,
      href: `/providers/${p.name}/`,
      logo: p.logo ? `/providers/${p.name}.svg` : null,
      logoDark: p.logoDark ? `/providers/${p.name}-dark.svg` : null,
    })),
    null,
    2,
  ) + "\n",
);
console.log(
  `sync-providers: wrote src/data/providers.json (${found.length} provider(s))`,
);

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
