// Start (or reconnect to) an Offload node in an E2B sandbox and print the
// `offload init` command for it.
//
//   npx tsx connect.ts              # new sandbox from the offload-node template
//   npx tsx connect.ts <sandboxId>  # reconnect; warm workspaces are still there
//
// Each connect starts the node with a fresh random token. The sandbox URL is
// HTTPS (E2B terminates TLS), so the node serves plain HTTP inside it.
import { Sandbox } from 'e2b'
import { randomBytes } from 'node:crypto'

const template = process.env.OFFLOAD_TEMPLATE ?? 'offload-node'
const timeoutMs = Number(process.env.OFFLOAD_TIMEOUT_MIN ?? 60) * 60_000
const id = process.argv[2]

const sbx = id ? await Sandbox.connect(id) : await Sandbox.create(template, { timeoutMs })
if (id) await sbx.setTimeout(timeoutMs)

const token = randomBytes(24).toString('hex')
await sbx.commands.run("pkill -f 'offload node' || true")
await sbx.commands.run('offload node --listen :7070 --root /data/offload --plain-http', {
  background: true,
  envs: { OFFLOAD_NODE_TOKEN: token },
})

const url = `https://${sbx.getHost(7070)}`
for (let i = 0; ; i++) {
  try {
    if ((await fetch(`${url}/v1/health`)).ok) break
  } catch {}
  if (i === 50) throw new Error(`node did not become healthy at ${url}`)
  await new Promise((r) => setTimeout(r, 200))
}

console.log(`sandbox ${sbx.sandboxId} is running an Offload node.\n`)
console.log(`  offload init --node ${url} --token ${token}\n`)
console.log(`Reconnect later with: npx tsx connect.ts ${sbx.sandboxId}`)
