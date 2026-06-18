// @ts-check
import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";

// https://astro.build/config
export default defineConfig({
  site: "https://bigfleet-providers.lucy.sh",
  integrations: [
    starlight({
      title: "BigFleet Providers",
      description:
        "Out-of-tree capacity providers for BigFleet, and the shared library every provider is built on.",
      logo: { src: "./src/assets/logo.svg", replacesTitle: false },
      social: {
        github: "https://github.com/intUnderflow/bigfleet-providers",
      },
      editLink: {
        baseUrl:
          "https://github.com/intUnderflow/bigfleet-providers/edit/main/site/src/content/docs/",
      },
      sidebar: [
        {
          label: "Start here",
          items: [{ label: "Overview", link: "/" }],
        },
        {
          label: "Build a provider",
          items: [
            {
              label: "Provider author guide",
              link: "https://bigfleet.lucy.sh/provider-author-guide/",
              attrs: { target: "_blank", rel: "noopener" },
            },
            {
              label: "Contributing",
              link: "https://github.com/intUnderflow/bigfleet-providers/blob/main/CONTRIBUTING.md",
              attrs: { target: "_blank", rel: "noopener" },
            },
          ],
        },
        {
          label: "Related",
          items: [
            {
              label: "BigFleet",
              link: "https://bigfleet.lucy.sh",
              attrs: { target: "_blank", rel: "noopener" },
            },
            {
              label: "GitHub",
              link: "https://github.com/intUnderflow/bigfleet-providers",
              attrs: { target: "_blank", rel: "noopener" },
            },
          ],
        },
      ],
      customCss: ["./src/styles/custom.css"],
    }),
  ],
});
