// Build the E2B template. Size the sandbox for your test suites: Offload's
// speed-up comes from cores the laptop doesn't have.
//
//   OFFLOAD_NODE_IMAGE=ghcr.io/you/offload-node:latest npx tsx build.ts
import { Template, defaultBuildLogger } from 'e2b'
import { template } from './template'

await Template.build(template, process.env.OFFLOAD_TEMPLATE ?? 'offload-node', {
  cpuCount: Number(process.env.OFFLOAD_CPUS ?? 8),
  memoryMB: Number(process.env.OFFLOAD_MEMORY_MB ?? 8192),
  onBuildLogs: defaultBuildLogger(),
})
