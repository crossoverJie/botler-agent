#!/usr/bin/env node
import { tsImport } from 'tsx/esm/api';

const args = process.argv.slice(2);
if (args[0] === 'init') {
  // Scaffold ~/.botler-agent/ (.env + providers.json + system-prompt.md templates)
  await tsImport('../src/init.ts', import.meta.url);
} else {
  await tsImport('../src/index.ts', import.meta.url);
}
