import { themes as prismThemes } from "prism-react-renderer";
import type { Config } from "@docusaurus/types";
import type * as Preset from "@docusaurus/preset-classic";

const versions: string[] = require("./versions.json");
const hasReleasedVersions = versions.length > 0;

const config: Config = {
  title: "lobslaw",
  tagline:
    "A self-hosted personal-assistant cluster — Raft-replicated memory, policy-gated tools, sandboxed skills, end-to-end mTLS",
  favicon: "img/favicon.svg",

  url: "https://jmylchreest.github.io",
  baseUrl: "/lobslaw/",
  organizationName: "jmylchreest",
  projectName: "lobslaw",
  deploymentBranch: "gh-pages",
  trailingSlash: false,

  onBrokenLinks: "throw",

  markdown: {
    hooks: {
      onBrokenMarkdownLinks: "warn",
    },
  },

  i18n: {
    defaultLocale: "en",
    locales: ["en"],
  },

  presets: [
    [
      "classic",
      {
        docs: {
          routeBasePath: "/",
          sidebarPath: "./sidebars.ts",
          editUrl: "https://github.com/jmylchreest/lobslaw/tree/main/docs/",
          includeCurrentVersion: true,
          ...(hasReleasedVersions
            ? {
                versions: {
                  current: {
                    label: "main",
                    path: "next",
                    banner: "unreleased",
                  },
                },
                lastVersion: versions[0],
              }
            : {}),
        },
        blog: false,
        theme: {
          customCss: "./src/css/custom.css",
        },
      } satisfies Preset.Options,
    ],
  ],

  plugins: [
    [
      "@cmfcmf/docusaurus-search-local",
      {
        indexDocs: true,
        indexBlog: false,
        indexPages: true,
        language: "en",
        maxSearchResults: 8,
      },
    ],
  ],

  themeConfig: {
    colorMode: {
      defaultMode: "dark",
      disableSwitch: false,
      respectPrefersColorScheme: true,
    },

    navbar: {
      title: "lobslaw",
      logo: {
        alt: "lobslaw",
        src: "img/logo.svg",
      },
      items: [
        {
          type: "docSidebar",
          sidebarId: "docs",
          position: "left",
          label: "Documentation",
        },
        ...(hasReleasedVersions
          ? [
              {
                type: "docsVersionDropdown" as const,
                position: "right" as const,
                dropdownActiveClassDisabled: true,
              },
            ]
          : []),
        {
          href: "https://github.com/jmylchreest/lobslaw",
          label: "GitHub",
          position: "right",
        },
      ],
    },

    footer: {
      style: "dark",
      links: [
        {
          title: "Documentation",
          items: [
            { label: "Getting Started", to: "/getting-started" },
            { label: "Security", to: "/security" },
            { label: "Configuration", to: "/configuration" },
          ],
        },
        {
          title: "Source",
          items: [
            { label: "GitHub", href: "https://github.com/jmylchreest/lobslaw" },
            {
              label: "Issues",
              href: "https://github.com/jmylchreest/lobslaw/issues",
            },
          ],
        },
        {
          title: "Related",
          items: [
            { label: "ClawHub", href: "https://clawhub.ai" },
            { label: "Smokescreen", href: "https://github.com/stripe/smokescreen" },
            { label: "hashicorp/raft", href: "https://github.com/hashicorp/raft" },
          ],
        },
      ],
      copyright: `Copyright ${new Date().getFullYear()} John Mylchreest. Built with Docusaurus.`,
    },

    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: [
        "bash",
        "toml",
        "json",
        "go",
        "yaml",
        "markdown",
      ],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
