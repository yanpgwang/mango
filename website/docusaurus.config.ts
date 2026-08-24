import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

const config: Config = {
  title: 'Mango',
  tagline: 'A self-hosted, durable runtime for long-running AI agents',
  favicon: 'img/mango-mark.svg',

  future: {
    v4: true,
  },

  url: process.env.DOCS_URL ?? 'https://yanpgwang.github.io',
  baseUrl: process.env.DOCS_BASE_URL ?? '/mango/',
  organizationName: 'yanpgwang',
  projectName: 'mango',

  onBrokenLinks: 'throw',
  markdown: {
    mermaid: true,
  },
  themes: ['@docusaurus/theme-mermaid'],

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      {
        docs: {
          path: '../docs',
          routeBasePath: '/',
          sidebarPath: './sidebars.ts',
          showLastUpdateTime: true,
          showLastUpdateAuthor: true,
          editUrl: ({docPath}) =>
            `https://github.com/yanpgwang/mango/edit/main/docs/${docPath.replace(
              /^(\.\.\/docs\/)+/,
              '',
            )}`,
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    colorMode: {
      respectPrefersColorScheme: true,
    },
    navbar: {
      title: 'Mango',
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'docs',
          position: 'left',
          label: 'Docs',
        },
        {
          to: '/examples',
          label: 'Examples',
          position: 'left',
        },
        {
          to: '/api',
          label: 'API',
          position: 'left',
        },
        {
          href: 'https://github.com/yanpgwang/mango',
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Documentation',
          items: [
            {
              label: 'Getting started',
              to: '/getting-started',
            },
            {
              label: 'Examples',
              to: '/examples',
            },
            {
              label: 'Guides',
              to: '/guides',
            },
            {
              label: 'API reference',
              to: '/api',
            },
          ],
        },
        {
          title: 'Resources',
          items: [
            {
              label: 'Capabilities',
              to: '/capabilities',
            },
            {
              label: 'Operations',
              to: '/operations',
            },
            {
              label: 'Product direction',
              to: '/product',
            },
            {
              label: 'Releases',
              href: 'https://github.com/yanpgwang/mango/releases',
            },
            {
              label: 'GitHub',
              href: 'https://github.com/yanpgwang/mango',
            },
          ],
        },
      ],
      copyright: `Copyright © ${new Date().getFullYear()} Mango contributors. Apache-2.0.`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
