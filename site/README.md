# BigFleet Providers website (`bigfleet-providers.lucy.sh`)

Astro [Starlight](https://starlight.astro.build/) static site — the same system
and theme as the main [BigFleet site](https://bigfleet.lucy.sh) (`../../bigfleet/site`).
Source for [https://bigfleet-providers.lucy.sh](https://bigfleet-providers.lucy.sh).

It is currently a **stub**: a single hand-written landing page
(`src/content/docs/index.md`). When real providers land, add their pages under
`src/content/docs/` and wire them into the sidebar in `astro.config.mjs`. (The
main BigFleet site additionally syncs Markdown from its repo `/docs` via a
`scripts/sync-docs.mjs` step; this site can adopt the same pattern once it has
docs to sync.)

## Local development

```sh
cd site
npm install
npm run dev        # Astro dev server on http://localhost:4321
```

## Production build

```sh
npm run build      # static site → ./dist
npm run preview    # serve ./dist locally
```

## Deploying

The site builds to a static `dist/` directory and is hosted on **Cloudflare
Pages**, the same as the main BigFleet site. As there, deployment is configured
in the Cloudflare dashboard (connected to this GitHub repo), not via a committed
workflow. Project settings for this monorepo:

| Setting | Value |
|---|---|
| Framework preset | Astro |
| Root directory | `site` |
| Build command | `npm run build` |
| Build output directory | `dist` |
| Custom domain | `bigfleet-providers.lucy.sh` |

Cloudflare builds on every push to `main`; no repo-side workflow or `CNAME` is
needed.

## Theme

`src/styles/custom.css`, `src/assets/logo.svg`, and `public/favicon.svg` are
kept identical to the main BigFleet site so the two read as one project. Edit
them in lockstep with the BigFleet site if the brand changes.
