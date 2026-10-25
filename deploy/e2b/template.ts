// E2B template for an Offload node: the offload-node image (see the
// repository's Dockerfile) pushed to a registry E2B can pull from.
//
// The node is started by connect.ts after the sandbox is created, not by a
// template start command: E2B snapshots the start command at build time, so a
// token generated there would be shared by every sandbox from this template.
import { Template } from 'e2b'

const image = process.env.OFFLOAD_NODE_IMAGE
if (!image) throw new Error('set OFFLOAD_NODE_IMAGE, e.g. ghcr.io/you/offload-node:latest')

export const template = Template().fromImage(image)
