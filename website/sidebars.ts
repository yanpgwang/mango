import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

const sidebars: SidebarsConfig = {
  docs: [
    'intro',
    {
      type: 'category',
      label: 'Start here',
      link: {
        type: 'generated-index',
        slug: '/start',
        title: 'Start with Mango',
        description: 'Run Mango locally and understand its current support boundary.',
      },
      collapsed: false,
      items: [
        'getting-started',
        'sdk',
        'capabilities',
      ],
    },
    {
      type: 'category',
      label: 'Guides',
      link: {
        type: 'generated-index',
        slug: '/guides',
        title: 'Mango guides',
        description: 'Use Mango to build and operate durable agent workflows.',
      },
      items: [
        'guides/multi-agent',
      ],
    },
    {
      type: 'category',
      label: 'Examples',
      link: {
        type: 'generated-index',
        slug: '/examples',
        title: 'Mango examples',
        description: 'End-to-end scenarios backed by executable verification.',
      },
      items: [
        'examples/terminal-ui',
        'examples/coding-agent-iterate',
        'examples/hitl-gate',
        'examples/multi-agent-team',
      ],
    },
    {
      type: 'category',
      label: 'Concepts',
      link: {
        type: 'doc',
        id: 'architecture',
      },
      items: [
        'architecture/domain-model',
        'architecture/session-lifecycle',
        'architecture/runtime-and-sandbox',
        'architecture/storage-context-and-tools',
        'architecture/workspace-tenancy',
      ],
    },
    {
      type: 'category',
      label: 'Operations',
      link: {
        type: 'generated-index',
        slug: '/operations',
        title: 'Operate Mango',
        description: 'Deploy Mango and choose an execution backend.',
      },
      items: [
        'deployment',
        'sandboxes',
      ],
    },
    {
      type: 'category',
      label: 'API reference',
      link: {
        type: 'doc',
        id: 'api/overview',
      },
      items: [
        'api/agents',
        'api/environments',
        'api/environment-work',
        'api/sessions',
        'api/events',
        'api/session-threads',
        'api/session-resources',
        'api/files',
        'api/skills',
        'api/memory',
        'api/vaults',
        'api/webhooks',
        'api/deployments',
      ],
    },
    {
      type: 'category',
      label: 'Project',
      link: {
        type: 'generated-index',
        slug: '/project',
        title: 'Mango project',
        description: 'Product direction and the provenance of Mango design decisions.',
      },
      items: [
        'product',
        'provenance',
      ],
    },
  ],
};

export default sidebars;
