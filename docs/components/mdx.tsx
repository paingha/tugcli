import defaultMdxComponents from 'fumadocs-ui/mdx';
import { Accordion, Accordions } from 'fumadocs-ui/components/accordion';
import { File, Files, Folder } from 'fumadocs-ui/components/files';
import { Step, Steps } from 'fumadocs-ui/components/steps';
import { Tab, Tabs } from 'fumadocs-ui/components/tabs';
import { TypeTable } from 'fumadocs-ui/components/type-table';
import type { MDXComponents } from 'mdx/types';
import { Flow, FlowStep } from '@/components/docs/flow';
import { Icon } from '@/components/docs/icon';
import { InlineCode } from '@/components/docs/inline-code';
import { MatchLadder } from '@/components/docs/match-ladder';
import { Prompt, PromptModes } from '@/components/docs/prompt';

export function getMDXComponents(components?: MDXComponents) {
  return {
    ...defaultMdxComponents,
    code: InlineCode,
    Accordion,
    Accordions,
    File,
    Files,
    Folder,
    Step,
    Steps,
    Tab,
    Tabs,
    TypeTable,
    Flow,
    FlowStep,
    Icon,
    MatchLadder,
    Prompt,
    PromptModes,
    ...components,
  } satisfies MDXComponents;
}

export const useMDXComponents = getMDXComponents;

declare global {
  type MDXProvidedComponents = ReturnType<typeof getMDXComponents>;
}
