// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// @ts-check
import { defineConfig } from 'astro/config';
import { unified } from '@astrojs/markdown-remark';
import starlight from '@astrojs/starlight';
import { remarkPrependBase } from './src/plugins/remark-prepend-base.mjs';

const BASE = '/core-models';

// Mirrors mast's and core-agent's docs/site (the go-steer convention).
// Project-pages hosting at https://go-steer.github.io/core-models/.
// Starlight prefixes only its own chrome with `base`; the
// remark-prepend-base plugin rewrites root-relative content links onto
// it at build time, so authors write `[text](/concepts/...)`.
export default defineConfig({
  site: 'https://go-steer.github.io',
  base: BASE,
  markdown: {
    processor: unified({ remarkPlugins: [remarkPrependBase(BASE)] }),
  },
  integrations: [
    starlight({
      title: 'core-models',
      description:
        'One provider layer for Go agents on ADK: Gemini, Claude, OpenAI, Vertex AI partner models and self-hosted servers behind one contract.',
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: 'https://github.com/go-steer/core-models',
        },
      ],
      editLink: {
        baseUrl: 'https://github.com/go-steer/core-models/edit/main/docs/site/',
      },
      customCss: ['./src/styles/theme.css'],
      // Same audience-first order as the sibling sites: decide whether
      // this is for you, understand how it fits together, then look up
      // exact names.
      sidebar: [
        {
          label: 'Overview',
          items: [
            { label: 'Introduction', link: '/' },
            { label: 'Why core-models', link: '/why-core-models/' },
            { label: 'Roadmap', link: '/roadmap/' },
          ],
        },
        {
          label: 'Concepts',
          items: [{ autogenerate: { directory: 'concepts' } }],
        },
        {
          label: 'Reference',
          items: [{ autogenerate: { directory: 'reference' } }],
        },
      ],
    }),
  ],
});
