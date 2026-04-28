import type { SidebarsConfig } from "@docusaurus/plugin-content-docs";

const sidebars: SidebarsConfig = {
  docs: [
    "intro",
    {
      type: "category",
      label: "Getting Started",
      link: { type: "doc", id: "getting-started/index" },
      items: [
        "getting-started/docker-compose",
        "getting-started/from-source",
        "getting-started/first-message",
      ],
    },
    {
      type: "category",
      label: "Architecture",
      link: { type: "doc", id: "architecture/index" },
      items: [
        "architecture/cluster",
        "architecture/memory",
        "architecture/agent-loop",
        "architecture/discovery",
        "architecture/storage",
      ],
    },
    {
      type: "category",
      label: "Security",
      link: { type: "doc", id: "security/index" },
      items: [
        "security/threat-model",
        "security/policy-engine",
        "security/sandbox",
        "security/egress-and-acl",
        "security/mtls",
        "security/oauth-and-credentials",
      ],
    },
    {
      type: "category",
      label: "Configuration",
      link: { type: "doc", id: "configuration/index" },
      items: [
        "configuration/reference",
        "configuration/policy-rules",
        "configuration/providers",
        "configuration/channels",
        "configuration/storage-mounts",
      ],
    },
    {
      type: "category",
      label: "Features",
      items: [
        "features/skills",
        "features/clawhub",
        "features/commitments",
        "features/notifications",
        "features/research",
        "features/scheduler",
        "features/council",
        "features/memory",
      ],
    },
    {
      type: "category",
      label: "Operating",
      items: [
        "operating/cli",
        "operating/doctor",
        "operating/cert-rotation",
        "operating/cluster-membership",
      ],
    },
    {
      type: "category",
      label: "Reference",
      items: [
        "reference/builtin-tools",
        "reference/hooks",
        "reference/proto-schema",
      ],
    },
  ],
};

export default sidebars;
