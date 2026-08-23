import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

const sidebars: SidebarsConfig = {
  docs: [
    'intro',
    'product',
    'getting-started',
    'capabilities',
    {
      type: 'category',
      label: 'Guides',
      items: [
        'guides/terminal-ui',
        'guides/multi-agent',
        'sandboxes',
      ],
    },
    {
      type: 'category',
      label: 'Concepts',
      items: [
        'architecture',
        'architecture/domain-model',
        'architecture/session-lifecycle',
        'architecture/runtime-and-sandbox',
        'architecture/storage-context-and-tools',
      ],
    },
    {
      type: 'category',
      label: 'Reference',
      items: [
        'api/overview',
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
        'api/deployments',
      ],
    },
    {
      type: 'category',
      label: 'Operations',
      items: [
        'deployment',
      ],
    },
  ],
};

export default sidebars;
